package browser

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// The last button of a web chore (Send on a contact form, Submit order,
// Book) sends something to someone, so, like a payment, it always asks the
// owner first with a screenshot of what will go, whatever the autonomy:
// "stop before sending" is the owner's ask, and the gate is what keeps it.

// reCommit is a button's own words that send or book: the whole label, so
// "Search", "Next" and a cookie banner's choices stay ordinary clicks.
var reCommit = regexp.MustCompile(`(?i)^\s*(submit|send|book|reserve)(\s+[\w']+){0,2}\s*[.!›»→>]*\s*$`)

// commitJS returns the visible words of what a step would press: the
// element itself (or the button it sits in), or, for a form sent from a
// field (a submit step, Enter in a field), the form's own send button. A
// form sent with no button says nothing: a search box has none.
const commitJS = `((sel, viaForm) => {
  let el = sel ? document.querySelector(sel) : document.activeElement;
  if (!el) return '';
  if (viaForm) {
    const f = el.form || (el.closest && el.closest('form'));
    if (!f) return '';
    el = f.querySelector('button[type=submit],input[type=submit],button:not([type])');
    if (!el) return '';
  } else if (el.closest) {
    el = el.closest('button,[role=button],input[type=submit],a') || el;
  }
  return String(el.innerText || el.value || el.getAttribute('aria-label') || '').trim().slice(0, 80);
})`

// probe is one thing on the page a step list might press: Commit asks for
// its send-button words (commitJS), Pay for its payment words (labelJS).
type probe struct {
	Sel     string `json:"sel"`
	ViaForm bool   `json:"viaForm"`
	Commit  bool   `json:"commit"`
	Pay     bool   `json:"pay"`
}

// stepProbes works out, before anything runs, what each step could press.
// Enter goes to the field the steps were last working in, since the focus
// only moves there when the steps run; with none, it goes to the field
// that has the focus now (sel "").
func stepProbes(steps []actStep) []probe {
	var out []probe
	last := ""
	for _, st := range steps {
		sel, terr := target(st.Ref, st.Selector)
		switch st.Type {
		case "click", "submit":
			if terr == nil {
				out = append(out, probe{Sel: sel, ViaForm: st.Type == "submit", Commit: true, Pay: true})
			}
		case "type":
			if terr == nil && st.Enter {
				out = append(out, probe{Sel: sel, ViaForm: true, Commit: true})
			}
		case "press":
			if strings.EqualFold(strings.TrimSpace(st.Key), "Enter") {
				out = append(out, probe{Sel: last, ViaForm: true, Commit: true})
			}
		}
		switch st.Type {
		case "click", "submit", "type", "select":
			if terr == nil {
				last = sel
			}
		}
	}
	return out
}

// riskJS reads every probe's words, and where the tab is, in one go.
const riskJS = `((ps) => ({href: location.href, words: ps.map(p => [p.commit ? ` + commitJS + `(p.sel, p.viaForm) : '', p.pay ? ` + labelJS + `(p.sel) : ''])}))`

// stepsCommit says whether the steps press a send, book or payment button.
// Steps for a page that isn't open yet can't be read, so any press on one
// asks.
func stepsCommit(tab context.Context, url string, steps []actStep) bool {
	ps := stepProbes(steps)
	if len(ps) == 0 {
		return false
	}
	if tab == nil {
		return url != ""
	}
	arg, _ := json.Marshal(ps)
	var got struct {
		Href  string     `json:"href"`
		Words [][]string `json:"words"`
	}
	tctx, cancel := context.WithTimeout(tab, 3*time.Second)
	defer cancel()
	if err := chromedp.Run(tctx, chromedp.Evaluate(riskJS+"("+string(arg)+")", &got)); err != nil {
		return url != ""
	}
	if url != "" && strings.TrimRight(url, "/") != strings.TrimRight(got.Href, "/") {
		return true
	}
	for _, w := range got.Words {
		if len(w) == 2 && (reCommit.MatchString(w[0]) || rePayment.MatchString(w[1])) {
			return true
		}
	}
	return false
}
