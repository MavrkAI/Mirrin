package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// teachBinding is how the recorder reports to Go.
const teachBinding = "__mirrinTeach"

// teachWorld is the isolated world the recorder runs in: it sees the page's
// elements and events, but the page's own scripts can't see it or its
// binding, so a site can't write steps of its own into the routine.
const teachWorld = "mirrin-teach"

// recorderJS watches what the user does and reports each action to Go
// through the teach binding, with a description of the field for each value.
// Passwords, card details and one-time codes never leave the page; the rest
// of the secrets are caught in Go (secrets.go) before anything is kept.
const recorderJS = `(() => {
  if (window.__mirrinTeachInstalled) return; window.__mirrinTeachInstalled = true;
  const SECRET = '` + secretValue + `';
  const sel = (el) => {
    if (!el || el.nodeType !== 1) return '';
    if (el.id) return '#' + CSS.escape(el.id);
    if (el.name) return el.tagName.toLowerCase() + '[name="' + el.name + '"]';
    const aria = el.getAttribute('aria-label'); if (aria) return el.tagName.toLowerCase() + '[aria-label="' + aria.replace(/"/g,'\\"') + '"]';
    const path = []; let cur = el;
    while (cur && cur.nodeType === 1 && path.length < 5) {
      let s = cur.tagName.toLowerCase(); const p = cur.parentElement;
      if (p) { const sib = [...p.children].filter(c => c.tagName === cur.tagName); if (sib.length > 1) s += ':nth-of-type(' + (sib.indexOf(cur) + 1) + ')'; }
      path.unshift(s); cur = p;
    }
    return path.join(' > ');
  };
  const label = (el) => {
    const isField = el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT';
    const cands = isField ? [el.placeholder, el.getAttribute('aria-label'), el.title, el.labels && el.labels[0] && el.labels[0].innerText, el.name, el.id]
                          : [el.innerText, el.getAttribute('aria-label'), el.title, (el.type === 'submit' || el.type === 'button') ? el.value : ''];
    return ((cands.find(c => c && String(c).trim()) || '') + '').trim().replace(/\s+/g, ' ').slice(0, 60);
  };
  const field = (el) => ({type: (el.type || '').toLowerCase(), ac: el.getAttribute('autocomplete') || '', name: el.getAttribute('name') || '',
    id: el.id || '', mode: el.getAttribute('inputmode') || '', label: (el.labels && el.labels[0] && el.labels[0].innerText) || '',
    ph: el.placeholder || '', aria: el.getAttribute('aria-label') || ''});
  const luhn = (v) => {
    const d = String(v || '').replace(/[\s-]/g, ''); if (!/^\d{13,19}$/.test(d)) return false;
    let s = 0, dbl = false;
    for (let i = d.length - 1; i >= 0; i--) { let n = +d[i]; if (dbl) { n *= 2; if (n > 9) n -= 9; } s += n; dbl = !dbl; }
    return s % 10 === 0;
  };
  const secret = (el) => el.type === 'password' || luhn(el.value) ||
    (el.getAttribute('autocomplete') || '').toLowerCase().split(/\s+/).some(t => t.startsWith('cc-') || t === 'one-time-code' || t.endsWith('-password'));
  const send = (o) => { try { window.` + teachBinding + `(JSON.stringify(o)); } catch (e) {} };
  document.addEventListener('click', (e) => {
    const el = e.target.closest('a, button, input, select, [role=button], [role=link], label, summary') || e.target;
    if (el.tagName === 'INPUT' && ['text','email','search','tel','url','number','password'].includes(el.type)) return;
    send({type: 'click', selector: sel(el), label: label(el), tag: el.tagName.toLowerCase()});
  }, true);
  document.addEventListener('change', (e) => {
    const el = e.target; if (!el || !el.tagName) return;
    if (el.tagName === 'SELECT') send({type: 'select', selector: sel(el), label: label(el), value: secret(el) ? SECRET : el.value, field: field(el)});
    else if (el.type === 'checkbox' || el.type === 'radio') send({type: 'click', selector: sel(el), label: label(el) || el.value, tag: 'input'});
    else send({type: 'type', selector: sel(el), label: label(el) || el.name || el.id || el.type, value: secret(el) ? SECRET : el.value, field: field(el)});
  }, true);
  document.addEventListener('keydown', (e) => { if (e.key === 'Enter') send({type: 'press', key: 'Enter', selector: sel(e.target)}); }, true);
  document.addEventListener('submit', (e) => send({type: 'submit', selector: sel(e.target), label: 'form'}), true);
})()`

type teachStep struct {
	Type     string `json:"type"`
	Selector string `json:"selector"`
	Label    string `json:"label"`
	Tag      string `json:"tag"`
	Value    string `json:"value"`
	Key      string `json:"key"`
	URL      string `json:"url"`
	Field    *field `json:"field,omitempty"` // the input a value went into; not kept
}

// redact makes a step safe to keep: a secret value becomes secretValue,
// card numbers anywhere and sign-in parameters in addresses are hidden, and
// the field description is dropped.
func (st *teachStep) redact() {
	if (st.Type == "type" || st.Type == "select") && st.Value != secretValue {
		f := field{}
		if st.Field != nil {
			f = *st.Field
		}
		if f.Label == "" {
			f.Label = st.Label
		}
		if secretField(f, st.Value) {
			st.Value = secretValue
		}
	}
	if st.Value != secretValue {
		st.Value = scrubCards(st.Value)
	}
	st.Label = scrubCards(st.Label)
	st.Selector = scrubCards(st.Selector)
	if st.URL != "" {
		st.URL = scrubURL(st.URL)
	}
	st.Field = nil
}

// teaching is one recording session.
type teaching struct {
	mu     sync.Mutex
	name   string
	steps  []teachStep
	start  time.Time
	cancel context.CancelFunc
}

// add records a step from the page, redacted.
func (t *teaching) add(st teachStep) {
	st.redact()
	t.mu.Lock()
	defer t.mu.Unlock()
	if st.Type == "navigate" {
		if n := len(t.steps); n > 0 && t.steps[n-1].Type == "navigate" && t.steps[n-1].URL == st.URL {
			return
		}
	}
	t.steps = append(t.steps, st)
}

func (s *Session) teachTools() []tools.Tool {
	return []tools.Tool{
		tools.New("teach_start",
			"Start watching the user do something in the browser so it can be turned into a routine: opens a visible window at the URL and records their clicks, typing (never passwords, card details, one-time codes or ID numbers) and page changes until teach_stop. Tell the user the window is open, to do the task once as they normally would, and to say \"done\" when finished.",
			tools.Schema(map[string]tools.Prop{
				"url":  {Type: "string", Description: "Where the task starts", Required: true},
				"name": {Type: "string", Description: "What to call the routine, e.g. \"submit timesheet\"", Required: true},
			}), tools.RiskRead,
			func(parent context.Context, call tools.Call) (string, error) {
				var in struct{ URL, Name string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if err := s.StartTeaching(in.URL, in.Name); err != nil {
					return "", err
				}
				return "Recording. A browser window is open at " + in.URL + ". Ask the user to do the task once, then say done; then call teach_stop.", nil
			}),
		tools.New("teach_stop",
			"Stop recording and get the steps the user performed, ready to turn into a protocol (with create_protocol) that replays them with browser_act. Show the user the steps in plain words and ask whether to save it and on what schedule.",
			tools.Schema(nil), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				return s.StopTeaching()
			}),
	}
}

// StartTeaching installs the recorder in a visible window.
func (s *Session) StartTeaching(url, name string) error {
	if _, err := s.webURL(context.Background(), url); err != nil {
		return err
	}
	ctx, err := s.tab(!s.forceHeadless)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.teach != nil {
		s.teach.cancel()
	}
	tctx, cancel := context.WithCancel(ctx)
	t := &teaching{name: name, start: time.Now(), cancel: cancel}
	s.teach = t
	s.handedTo = time.Now()
	s.mu.Unlock()
	chromedp.ListenTarget(tctx, func(ev any) {
		switch e := ev.(type) {
		case *runtime.EventBindingCalled:
			if e.Name != teachBinding {
				return
			}
			var st teachStep
			if json.Unmarshal([]byte(e.Payload), &st) == nil && st.Type != "navigate" {
				t.add(st)
			}
		case *page.EventFrameNavigated:
			if e.Frame.ParentID == "" && e.Frame.URL != "" {
				t.add(teachStep{Type: "navigate", URL: e.Frame.URL})
			}
		}
	})
	rctx, rcancel := context.WithTimeout(ctx, 45*time.Second)
	defer rcancel()
	if err := chromedp.Run(rctx,
		runtime.AddBinding(teachBinding).WithExecutionContextName(teachWorld),
		chromedp.ActionFunc(func(c context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(recorderJS).WithWorldName(teachWorld).Do(c)
			return err
		}),
	); err != nil {
		return err
	}
	if err := s.open(rctx, url, chromedp.Sleep(800*time.Millisecond)); err != nil {
		return err
	}
	// The page is loaded; make sure it is watched (a no-op when the
	// script above already runs there).
	return chromedp.Run(rctx, chromedp.ActionFunc(func(c context.Context) error {
		tree, err := page.GetFrameTree().Do(c)
		if err != nil {
			return err
		}
		id, err := page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName(teachWorld).Do(c)
		if err != nil {
			return err
		}
		_, exc, err := runtime.Evaluate(recorderJS).WithContextID(id).Do(c)
		if err == nil && exc != nil {
			err = exc
		}
		return err
	}))
}

// StopTeaching ends the recording and renders the steps.
func (s *Session) StopTeaching() (string, error) {
	s.mu.Lock()
	t := s.teach
	s.teach = nil
	s.mu.Unlock()
	if t == nil {
		return "", fmt.Errorf("nothing is being recorded; call teach_start first")
	}
	t.cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.steps) == 0 {
		return "", fmt.Errorf("nothing was recorded; the user may not have done anything in the window yet")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Recorded routine %q (%d actions, %s):\n", t.name, len(t.steps), time.Since(t.start).Round(time.Second))
	n := 0
	var lastTyped string
	secrets := false
	value := func(v string) string {
		if v == secretValue {
			secrets = true
			return v
		}
		return fmt.Sprintf("%q", v)
	}
	for _, st := range t.steps {
		switch st.Type {
		case "navigate":
			n++
			fmt.Fprintf(&b, "%d. Go to %s\n", n, st.URL)
		case "click":
			n++
			fmt.Fprintf(&b, "%d. Click %q (%s)\n", n, st.Label, st.Selector)
		case "type":
			n++
			fmt.Fprintf(&b, "%d. Type %s into %q (%s)\n", n, value(st.Value), st.Label, st.Selector)
			lastTyped = st.Selector
		case "select":
			n++
			fmt.Fprintf(&b, "%d. Choose %s in %q (%s)\n", n, value(st.Value), st.Label, st.Selector)
		case "press":
			if st.Selector == lastTyped {
				fmt.Fprintf(&b, "   then press Enter\n")
			} else {
				n++
				fmt.Fprintf(&b, "%d. Press Enter (%s)\n", n, st.Selector)
			}
		case "submit":
			n++
			fmt.Fprintf(&b, "%d. Submit the form (%s)\n", n, st.Selector)
		}
	}
	b.WriteString("\nTo replay: browse_page the first URL, then browser_act with these selectors (or the refs you see), typing the same values unless the routine should ask for them each time.")
	if secrets {
		b.WriteString(" Steps marked " + secretValue + " were not recorded (a password, card details, a one-time code or an ID number): never ask the user to send them in a message and never write them into the protocol. The user types them in a window they can see: the routine opens its starting page (or, at the latest, the page with those fields) with browser_signin instead of browse_page, carries on there with browser_act, and when it reaches such a field calls browser_signin with no url, which hands that page over as it is, then waits for the user to say it's done before going on.")
	}
	b.WriteString(" If the routine pays for something, the payment click will ask the user first. Turn it into a protocol with create_protocol; its prompt should describe the steps in words with the selectors, and say what to report back.")
	return b.String(), nil
}
