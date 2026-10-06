package browser

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// A browser_act approval names what the owner is saying yes to in their
// words: Click "Pay now $49" on shop.example, not click ref 12. The labels
// come from the fingerprint checkAct took when the owner was about to be
// asked (the agent runs the check first, then asks for the summary).

// ApprovalSummary implements tools.Summarizer for browser_act.
func (c checked) ApprovalSummary(call tools.Call) string {
	if c.summary == nil {
		return c.Spec().Name
	}
	return c.summary(call)
}

// actSummary describes a browser_act call for its approval.
func (s *Session) actSummary(call tools.Call) string {
	var in actInput
	if err := tools.Decode(call, &in); err != nil {
		return "browser_act " + string(call.Input)
	}
	steps, err := parseSteps(in.Steps)
	if err != nil || len(steps) == 0 {
		return "browser_act " + in.Steps
	}
	labels := map[string]string{}
	page := in.URL
	if in.URL == "" {
		s.mu.Lock()
		a, ok := s.asked[askKey(call.ChatKey, call.Input)]
		s.mu.Unlock()
		if ok {
			page = a.url
			for _, t := range a.targets {
				if t.label != "" {
					labels[t.sel] = t.label
				}
			}
		}
	}
	open := false // the call's page is the one already on screen
	if in.URL != "" {
		s.mu.Lock()
		tab := s.ctx
		s.mu.Unlock()
		if tab != nil && tab.Err() == nil {
			if loc, targets, err := fingerprint(tab, stepTargets(steps)); err == nil && samePage(loc, in.URL) {
				open, page = true, loc
				for _, t := range targets {
					if t.label != "" {
						labels[t.sel] = t.label
					}
				}
			}
		}
	}
	parts := make([]string, 0, len(steps))
	for _, st := range steps {
		parts = append(parts, describeStep(st, labels))
	}
	out := strings.Join(parts, ", then ")
	if in.URL != "" && !open {
		out = fmt.Sprintf("open %s, then %s", in.URL, out)
	}
	if r, n := utf8.DecodeRuneInString(out); n > 0 {
		out = string(unicode.ToUpper(r)) + out[n:]
	}
	if host := hostOf(page); host != "" {
		out += " on " + host
	}
	return out
}

// describeStep names one step, with the element's label where it is known.
func describeStep(st actStep, labels map[string]string) string {
	el := func() string {
		sel, err := target(st.Ref, st.Selector)
		if err != nil {
			return "an element"
		}
		if l := labels[sel]; l != "" {
			return fmt.Sprintf("%q", l)
		}
		if st.Ref > 0 {
			return fmt.Sprintf("element %d", st.Ref)
		}
		return fmt.Sprintf("%q", sel)
	}
	switch st.Type {
	case "click":
		return "click " + el()
	case "submit":
		return "submit " + el()
	case "type":
		s := fmt.Sprintf("type %q into %s", st.Text, el())
		if st.Enter {
			s += " and press Enter"
		}
		return s
	case "select":
		return fmt.Sprintf("choose %q in %s", st.Value, el())
	case "press":
		return "press " + st.Key
	case "scroll":
		if st.Direction == "up" {
			return "scroll up"
		}
		return "scroll down"
	case "wait":
		return fmt.Sprintf("wait for %q", st.Selector)
	case "sleep":
		return fmt.Sprintf("wait %dms", st.Ms)
	}
	return fmt.Sprintf("%q", st.Type)
}

// hostOf is the site part of a page address.
func hostOf(page string) string {
	u, err := url.Parse(page)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
