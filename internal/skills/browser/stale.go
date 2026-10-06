package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// A browser_act that needs approval acts on the page as it was when the
// owner was asked. The browser is shared by every chat, task and protocol,
// and pages change by themselves, so by the time the owner says yes "click 12"
// could press something they never saw. When the owner is asked, the page
// address and each targeted element are fingerprinted (checkAct). When they
// say yes, CheckApproved compares, before anything runs, so a changed page
// ends the approval unrun; the tool itself checks once more just before it
// clicks. A call nobody was asked about (allowed by policy) is never held
// to an old question.

// fingerprintJS describes each element the selectors find, as it is now: what
// it is, where it leads and what it says. A field's typed value is left out;
// a button's is its label.
const fingerprintJS = `((sels) => sels.map((sel) => {
  let el = null; try { el = document.querySelector(sel); } catch (e) { return null; }
  if (!el) return null;
  const t = (s) => (s == null ? '' : String(s)).trim().replace(/\s+/g, ' ').slice(0, 160);
  const button = el.tagName === 'INPUT' && /^(submit|button|reset|image)$/i.test(el.type);
  const field = !button && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT');
  const text = field ? '' : t(el.innerText || (button ? el.value : ''));
  const label = t(text || el.getAttribute('aria-label') || el.placeholder || el.title || el.alt || el.name || el.id || el.tagName.toLowerCase()).slice(0, 60);
  const form = el.form ? t(el.form.getAttribute('action')) + ' ' + t(el.form.getAttribute('method')) : '';
  return {label, print: [el.tagName, el.type || '', el.id, el.getAttribute('name') || '', el.getAttribute('role') || '',
    t(el.getAttribute('aria-label')), t(el.getAttribute('href')), form, text, t(el.placeholder), t(el.title), t(el.alt)].join('|')};
}))`

// askedPage is the page an approval was asked on. Past askedFor only the
// time is kept, so a very late yes is refused rather than let through.
type askedPage struct {
	url      string
	targets  []askedTarget
	at       time.Time
	approved bool // the owner said yes and the page was still the same
}

// askedTarget is one element a step aims at; print is "" when it wasn't on
// the page then (a later step may bring it).
type askedTarget struct {
	sel, label, print string
	ref               int
}

// askedFor is how long an asked page counts (after that a yes is refused),
// and maxAsked how many records are kept, tombstones included.
const (
	askedFor = 24 * time.Hour
	maxAsked = 256
)

// askKey identifies one asked call: the conversation and the exact input.
func askKey(chatKey string, input json.RawMessage) string {
	var buf bytes.Buffer
	in := []byte(input)
	if json.Compact(&buf, in) == nil {
		in = buf.Bytes()
	}
	h := sha256.New()
	h.Write([]byte(chatKey))
	h.Write([]byte{0})
	h.Write(in)
	return hex.EncodeToString(h.Sum(nil))
}

// stepTargets lists the elements the steps aim at, once each.
func stepTargets(steps []actStep) []askedTarget {
	var out []askedTarget
	seen := map[string]bool{}
	for _, st := range steps {
		if st.Ref <= 0 && st.Selector == "" {
			continue
		}
		sel, err := target(st.Ref, st.Selector)
		if err != nil || seen[sel] {
			continue
		}
		seen[sel] = true
		out = append(out, askedTarget{sel: sel, ref: st.Ref})
	}
	return out
}

// fingerprint describes the page and the targets as they are now.
func fingerprint(ctx context.Context, targets []askedTarget) (string, []askedTarget, error) {
	sels := make([]string, len(targets))
	for i, t := range targets {
		sels[i] = t.sel
	}
	arg, _ := json.Marshal(sels)
	var loc string
	var prints []*struct{ Label, Print string }
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := chromedp.Run(tctx, chromedp.Location(&loc), chromedp.Evaluate(fingerprintJS+"("+string(arg)+")", &prints)); err != nil {
		return "", nil, err
	}
	out := make([]askedTarget, len(targets))
	for i, t := range targets {
		out[i] = askedTarget{sel: t.sel, ref: t.ref} // what it was doesn't carry over
		if i < len(prints) && prints[i] != nil {
			out[i].label, out[i].print = prints[i].Label, prints[i].Print
		}
	}
	return loc, out, nil
}

// checkAct runs when the owner is about to be asked for a browser_act: a
// call that could never work is refused now, and the page is fingerprinted
// for the approved call to compare against.
func (s *Session) checkAct(ctx context.Context, call tools.Call) error {
	var in actInput
	if err := tools.Decode(call, &in); err != nil {
		return err
	}
	steps, err := parseSteps(in.Steps)
	if err != nil {
		return err
	}
	if in.URL != "" {
		_, err := s.webURL(ctx, in.URL) // opens its own page: not tied to this one
		return err
	}
	s.mu.Lock()
	tab := s.ctx
	s.mu.Unlock()
	if tab == nil || tab.Err() != nil {
		return errors.New("there's no page open in the browser, so there is nothing to act on yet. Open it with browse_page first, then ask again")
	}
	loc, targets, err := fingerprint(tab, stepTargets(steps))
	if err != nil {
		return nil // can't look now; the owner's yes still meets the address check
	}
	s.remember(askKey(call.ChatKey, call.Input), askedPage{url: loc, targets: targets, at: time.Now()})
	return nil
}

// remember keeps the page a call was asked on, within maxAsked: records past
// askedFor become tombstones (the time alone), and the oldest go first.
func (s *Session) remember(key string, a askedPage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.asked == nil {
		s.asked = map[string]askedPage{}
	}
	oldest, oldestAt := "", time.Now()
	for k, old := range s.asked {
		if time.Since(old.at) > askedFor && old.targets != nil {
			s.asked[k] = askedPage{at: old.at}
		}
		if old.at.Before(oldestAt) {
			oldest, oldestAt = k, old.at
		}
	}
	if _, ok := s.asked[key]; !ok && len(s.asked) >= maxAsked {
		delete(s.asked, oldest)
	}
	s.asked[key] = a
}

// notRun ends every refusal: nothing happened, and what to do.
const notRun = "so nothing was clicked or typed. Look at the page again and ask afresh."

// CheckApproved runs when the owner has said yes to a browser_act, before it
// runs: the page and the elements it aims at must still be the ones they were
// asked about. A refusal says what changed; the approval should then end
// unrun. A call that opens its own page, or that nobody was asked about here,
// passes.
func (s *Session) CheckApproved(ctx context.Context, chatKey string, input json.RawMessage) error {
	key := askKey(chatKey, input)
	s.mu.Lock()
	a, ok := s.asked[key]
	if ok && time.Since(a.at) > askedFor {
		delete(s.asked, key)
	}
	tab := s.ctx
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if time.Since(a.at) > askedFor {
		return errors.New("this was asked too long ago, " + notRun)
	}
	if err := stillAsked(tab, a); err != nil {
		s.forget(key)
		return err
	}
	s.mu.Lock()
	if cur, ok := s.asked[key]; ok && cur.at.Equal(a.at) {
		cur.approved = true
		s.asked[key] = cur
	}
	s.mu.Unlock()
	return nil
}

func (s *Session) forget(key string) {
	s.mu.Lock()
	delete(s.asked, key)
	s.mu.Unlock()
}

// checkStillAsked runs as browser_act starts: a call the owner approved
// (CheckApproved) is checked once more, right before it clicks.
func (s *Session) checkStillAsked(call tools.Call) error {
	key := askKey(call.ChatKey, call.Input)
	s.mu.Lock()
	a, ok := s.asked[key]
	if ok && a.approved {
		delete(s.asked, key)
	}
	tab := s.ctx
	s.mu.Unlock()
	if !ok || !a.approved {
		return nil
	}
	return stillAsked(tab, a)
}

// stillAsked compares the page in tab with the one a was asked on.
func stillAsked(tab context.Context, a askedPage) error {
	if tab == nil || tab.Err() != nil {
		return fmt.Errorf("the browser was closed after this was asked (it was on %s), %s", a.url, notRun)
	}
	loc, now, err := fingerprint(tab, a.targets)
	if err != nil {
		return fmt.Errorf("I couldn't check the page is still the one you were asked about (%v), %s", err, notRun)
	}
	if !samePage(a.url, loc) {
		return fmt.Errorf("the browser has moved on since this was asked (it was on %s, now %s), %s", a.url, loc, notRun)
	}
	for i, t := range a.targets {
		if t.print == "" {
			continue
		}
		name := describe(t)
		switch {
		case now[i].print == "":
			return fmt.Errorf("the page changed since this was asked: %s is no longer there, %s", name, notRun)
		case now[i].print != t.print:
			if now[i].label != t.label {
				return fmt.Errorf("the page changed since this was asked: %s now reads %q, %s", name, now[i].label, notRun)
			}
			return fmt.Errorf("the page changed since this was asked: %s is not the same element any more, %s", name, notRun)
		}
	}
	return nil
}

// describe names an element for the owner.
func describe(t askedTarget) string {
	name := fmt.Sprintf("%q", t.label)
	if t.ref > 0 {
		return fmt.Sprintf("%s (element %d)", name, t.ref)
	}
	return name
}

// samePage compares page addresses, ignoring the #fragment.
func samePage(a, b string) bool {
	cut := func(s string) string {
		if i := strings.IndexByte(s, '#'); i >= 0 {
			s = s[:i]
		}
		return strings.TrimSuffix(s, "/")
	}
	return a != "" && b != "" && cut(a) == cut(b)
}
