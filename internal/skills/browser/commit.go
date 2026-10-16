package browser

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Ordinary browsing runs unasked: opening pages, clicking through,
// scrolling, searching and filtering are reads. The last button of a web
// chore (Send on a contact form, Submit order, Book, Post, Unsubscribe)
// sends something to someone, so, like a payment, it always asks the owner
// first with a screenshot of what will go, whatever the autonomy: "stop
// before sending" is the owner's ask, and the gate is what keeps it.
// Agreeing is the same: a tick box for the terms, Create account, a bare
// Accept. Typing personal details (a name, an email, a phone number) is a
// write, so it asks under the default autonomy; typing a search term is a
// read. Anything that can't be read, a page not open yet, an element not
// on it yet, a page that won't answer, asks too. ask_every_click in the
// browser section makes every click and every typed word a write, as the
// twin did before 0.2.1.

// reCommit is a button's own words that send, book or undo: the first
// word, in a short label, so "Search", "Next", "Add to cart", a dialog's
// Cancel and a cookie banner's choices stay ordinary clicks.
var reCommit = regexp.MustCompile(`(?is)^\s*(yes,?\s+)?(submit|send|book|reserve|post|publish|reply|apply\s+(now|for)|order\s+now|confirm|delete|remove\s+(my\s+)?account|cancel\s+\w+|unsubscribe|rsvp)\b.{0,36}$`)

// reAgree is the words that agree to something, anywhere in them: I agree,
// accept the terms, sign up, create an account, register. reAgreeBare is a
// button that is only Accept, Agree or Consent, which a cookie banner may
// also say: agrees lets those through when cookies are what is meant.
var reAgree = regexp.MustCompile(`(?i)(\bi\s+(agree|accept|consent)\b|\b(agree|consent)\s+(to|and)\b|\baccept(ing)?\s+(the\s+|all\s+)?(terms|conditions|agreement|policy|privacy)|\bsign\s*up\b|\bcreate\s+(\w+\s+){0,2}account\b|\bregister\b|\bregistration\b|\bjoin\s+now\b)`)
var reAgreeBare = regexp.MustCompile(`(?i)^\s*(yes,?\s+)?(i\s+)?(agree|accept|consent)\b`)
var reCookies = regexp.MustCompile(`(?i)\bcookies?\b`)

// agrees says whether a button's or a tick box's words agree to terms,
// sign up or create an account. A cookie banner's Accept or Agree doesn't,
// when cookies are what the words (or the dialog they sit in) are about.
func agrees(words string) bool {
	if reAgree.MatchString(words) {
		return true
	}
	return reAgreeBare.MatchString(words) && !reCookies.MatchString(words)
}

// rePersonal is a field that takes personal details, by its type,
// autocomplete, name, id, placeholder or label, read as words
// (fieldWords): typing there is a write. reSearchy is a search box, which
// is never one, whatever it is searching; a combobox role doesn't make
// one, since address and email pickers have it too.
var rePersonal = regexp.MustCompile(`\b(e ?mail|tele?phone|phone|tel|mobile|address|street|suburb|city|post(al)? ?code|zip|name|surname|given|family|birth|bday|dob|age|card|cc|cvc|cvv|iban|bsb|account ?(number|no)|passport|licen[cs]e|tax|tfn|ssn|gender|salary|income|medicare)\b`)
var reSearchy = regexp.MustCompile(`\b(search|find|filter|look ?up|query)\b`)

// fieldWords is a field's attributes as words: lower case, with the
// underscores, dashes and capitals forms spell with turned into spaces, so
// first_name, firstName, postal-code and cc-number read as the words they
// are.
var reCamel = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func fieldWords(s string) string {
	s = reCamel.ReplaceAllString(s, "$1 $2")
	return strings.ToLower(strings.NewReplacer("_", " ", "-", " ").Replace(s))
}

// personal says whether a field, by its words, takes personal details.
func personal(field string) bool {
	w := fieldWords(field)
	return rePersonal.MatchString(w) && !reSearchy.MatchString(w)
}

// Goals handed to Jev (browser_run) are sentences, so they get rules of
// their own: reGoalCommit is a goal that sends, books, posts or undoes,
// reGoalAgree one that signs up or agrees, reGoalPersonal one that carries
// personal details (an email address, a phone number, "my name").
// A word that is also a noun (a book, a post, an order, a message) counts
// only as the verb, with its object after it, so finding a book or
// reading a message is browsing.
var reGoalCommit = regexp.MustCompile(`(?i)(\b(send|submit|publish|reply|unsubscribe|rsvp)\b|\bapply\s+(now|for)\b|\b(book|reserve|post|order|message|delete|cancel)\s+(a|an|the|my|our|this|that|it|them|me|us|him|her|all|now|\d)\b)`)
var reGoalAgree = regexp.MustCompile(`(?i)(\bsign\s*up\b|\bcreate\s+(\w+\s+){0,2}account\b|\bregister\s+(an?\s+)?account\b|\bregister\s+(me|us|now)\b|\baccept(ing)?\s+(the\s+|all\s+)?(terms|conditions|agreement|policy|privacy)|\bagree\s+to\b|\bconsent\s+to\b)`)
var reGoalPersonal = regexp.MustCompile(`(?i)(@[a-z0-9]|(\+|\b0)\d[\d\s-]{7,}\d|\bmy\s+(name|phone|number|e-?mail|address|date\s+of\s+birth|birthday|card|details|password)\b|\b(first|last|full)\s+name\b)`)

// goalRisk is a goal's risk from base: dangerous when it pays, sends,
// books or agrees, a write when it carries personal details.
func goalRisk(goals string, base tools.Risk) tools.Risk {
	if rePayment.MatchString(goals) || reGoalCommit.MatchString(goals) || reGoalAgree.MatchString(goals) {
		return tools.RiskDangerous
	}
	if reGoalPersonal.MatchString(goals) {
		return max(base, tools.RiskWrite)
	}
	return base
}

// wordsJS is an element's visible words.
const wordsJS = `(e => String(e.innerText || e.value || e.getAttribute('aria-label') || e.title || '').replace(/\s+/g, ' ').trim())`

// realHrefJS says whether a link goes somewhere: following one is
// reading, where a link that goes nowhere is a button in disguise.
const realHrefJS = `(a => { const h = a.getAttribute('href'); return !!h && h !== '#' && !/^javascript:/i.test(h); })`

// commitJS returns the visible words of what a step would press: the
// element itself (or the button it sits in), or, for a form sent from a
// field (a submit step, Enter in a field), the form's own send button. A
// form sent with no button says nothing: a search box has none. Enter in
// a composer with no form at all (a chat, a comment box) sends, so that
// says Send; Enter in a search box with no form searches.
const commitJS = `((el, viaForm) => {
  const words = ` + wordsJS + `;
  if (viaForm) {
    const f = el.form || (el.closest && el.closest('form'));
    if (!f) {
      const hint = (el.name || '') + ' ' + (el.placeholder || '') + ' ' + (el.getAttribute('aria-label') || '') + ' ' + (el.getAttribute('role') || '');
      const searchy = el.type === 'search' || /search|find|filter|combobox/i.test(hint);
      const composer = el.isContentEditable || el.tagName === 'TEXTAREA' || (el.tagName === 'INPUT' && !searchy);
      return composer ? 'Send' : '';
    }
    const b = f.querySelector('button[type=submit],input[type=submit],button:not([type])');
    return b ? words(b).slice(0, 80) : '';
  }
  const b = el.closest ? el.closest('button,[role=button],input[type=submit],input[type=button],a') : null;
  if (b && b.tagName === 'A' && ` + realHrefJS + `(b)) return '';
  return words(b || el).slice(0, 80);
})`

// payJS returns an element's payment words: its label, id and name.
const payJS = `(el => ((el.innerText || el.value || el.getAttribute('aria-label') || el.title || '') + ' ' + (el.id || '') + ' ' + (el.name || '')).trim().slice(0, 120))`

// agreeJS returns the words of what a click would agree with: a button's
// (or the button the click lands in, or the element's own), a tick box's
// or a switch's with its label, or the words beside it. A link that goes
// somewhere says nothing: following one to read the terms isn't agreeing
// to them. Inside a cookie dialog the words start with "cookies", so a
// bare Accept there is read as the banner it is.
const agreeJS = `((el) => {
  const words = ` + wordsJS + `;
  const cookies = e => { const c = e.closest && e.closest('dialog,[role=dialog],[aria-modal=true],[id*=cookie i],[class*=cookie i],[id*=consent i],[class*=consent i],[id*=gdpr i],[class*=gdpr i]'); return c && /cookies?/i.test(c.innerText || '') ? 'cookies ' : ''; };
  const named = e => { const ids = e.getAttribute('aria-labelledby'); let t = ''; if (ids) for (const id of ids.split(/\s+/)) { const l = document.getElementById(id); if (l) t += ' ' + (l.innerText || ''); } return t; };
  const tick = 'input[type=checkbox],input[type=radio],[role=checkbox],[role=radio],[role=switch]';
  let box = el.matches && el.matches(tick) ? el : null;
  if (!box && el.closest) { const lab = el.closest('label'); if (lab) box = lab.querySelector(tick); }
  if (box) {
    const own = box.closest('label') || (box.id && document.querySelector('label[for="' + CSS.escape(box.id) + '"]'));
    let t = (own ? words(own) : '') + ' ' + (box.getAttribute('aria-label') || '') + named(box);
    if (!t.trim() && box.parentElement) t = box.parentElement.innerText || '';
    return cookies(box) + t.trim().slice(0, 160);
  }
  const b = el.closest ? el.closest('button,[role=button],input[type=submit],input[type=button],a') : null;
  if (b && b.tagName === 'A' && ` + realHrefJS + `(b)) return '';
  return cookies(b || el) + words(b || el).slice(0, 160);
})`

// fieldJS returns what a field is for: its type, role, autocomplete,
// name, id, placeholder, aria-label and label, so a field for personal
// details can be told from a search box.
const fieldJS = `((el) => {
  const lab = (el.closest && el.closest('label')) || (el.id && document.querySelector('label[for="' + CSS.escape(el.id) + '"]'));
  return [el.type, el.getAttribute('role'), el.getAttribute('autocomplete'), el.name, el.id, el.placeholder, el.getAttribute('aria-label'), lab ? lab.innerText : ''].map(x => x || '').join(' ').trim().slice(0, 200);
})`

// probe is one thing on the page a step list might touch: Commit asks for
// its send-button words (commitJS), Pay for its payment words (payJS),
// Agree for what it would agree with (agreeJS), Field for what the field
// it types into or chooses from is for (fieldJS).
type probe struct {
	Sel     string `json:"sel"`
	ViaForm bool   `json:"viaForm"`
	Commit  bool   `json:"commit"`
	Pay     bool   `json:"pay"`
	Agree   bool   `json:"agree"`
	Field   bool   `json:"field"`
}

// presses says whether the probe presses something.
func (p probe) presses() bool { return p.Commit || p.Pay || p.Agree }

// stepProbes works out, before anything runs, what each step could touch.
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
				out = append(out, probe{Sel: sel, ViaForm: st.Type == "submit", Commit: true, Pay: true, Agree: st.Type == "click"})
			}
		case "type", "select":
			if terr == nil {
				out = append(out, probe{Sel: sel, Field: true})
				if st.Type == "type" && st.Enter {
					out = append(out, probe{Sel: sel, ViaForm: true, Commit: true})
				}
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

// anyPress says whether any probe presses something; anyTyping whether
// any types into or chooses from a field.
func anyPress(ps []probe) bool {
	for _, p := range ps {
		if p.presses() {
			return true
		}
	}
	return false
}

func anyTyping(ps []probe) bool {
	for _, p := range ps {
		if p.Field {
			return true
		}
	}
	return false
}

// probeWords is one probe's words as the page gives them; nil for an
// element that isn't on the page.
type probeWords struct {
	Commit, Pay, Agree, Field string
}

// riskJS reads every probe's words, and where the tab is, in one go. A
// probe whose element isn't on the page is null.
const riskJS = `((ps) => ({href: location.href, probes: ps.map(p => {
  const el = p.sel ? document.querySelector(p.sel) : document.activeElement;
  if (!el) return null;
  return {Commit: p.commit ? ` + commitJS + `(el, p.viaForm) : '', Pay: p.pay ? ` + payJS + `(el) : '', Agree: p.agree ? ` + agreeJS + `(el) : '', Field: p.field ? ` + fieldJS + `(el) : ''};
})}))`

// unreadRisk is the risk of steps on a page that can't be read: a press
// is dangerous, typing a write, anything else base.
func unreadRisk(ps []probe, base tools.Risk) tools.Risk {
	switch {
	case anyPress(ps):
		return tools.RiskDangerous
	case anyTyping(ps):
		return max(base, tools.RiskWrite)
	}
	return base
}

// stepsRisk is the risk of steps on the open page, from base (a read, or a
// write with ask_every_click): dangerous for a press that sends, pays or
// agrees, write for typing personal details. Whatever can't be read, a
// page not open yet, an element not on it, a page that won't answer, is
// taken the careful way (unreadRisk).
func stepsRisk(tab context.Context, url string, steps []actStep, base tools.Risk) tools.Risk {
	ps := stepProbes(steps)
	if len(ps) == 0 || tab == nil {
		return unreadRisk(ps, base)
	}
	arg, _ := json.Marshal(ps)
	var got struct {
		Href   string        `json:"href"`
		Probes []*probeWords `json:"probes"`
	}
	tctx, cancel := context.WithTimeout(tab, 3*time.Second)
	defer cancel()
	if err := evalRisk(tctx, riskJS+"("+string(arg)+")", &got); err != nil || len(got.Probes) != len(ps) {
		return unreadRisk(ps, base)
	}
	if url != "" && strings.TrimRight(url, "/") != strings.TrimRight(got.Href, "/") {
		return unreadRisk(ps, base)
	}
	risk := base
	for i, w := range got.Probes {
		if w == nil {
			risk = max(risk, unreadRisk(ps[i:i+1], base))
			continue
		}
		if reCommit.MatchString(w.Commit) || rePayment.MatchString(w.Pay) || agrees(w.Agree) {
			return tools.RiskDangerous
		}
		if ps[i].Field && personal(w.Field) {
			risk = max(risk, tools.RiskWrite)
		}
	}
	return risk
}
