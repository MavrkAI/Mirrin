package api

import "testing"

// Each case gets an honest sentence: when everything was in mind it says so
// rather than pretending the listed facts were all it used.
func TestWhyLead(t *testing.T) {
	for _, c := range []struct {
		w    Why
		want string
	}{
		{Why{}, "I didn't keep a note of what went into that one. Replies from before I started keeping track don't have one."},
		{Why{Known: true, Everything: true, Facts: []WhyFact{{ID: 1}}}, "I had everything you've told me in mind; these matched most closely:"},
		{Why{Known: true, Everything: true}, "I had everything you've told me in mind, and nothing in particular stood out for this one."},
		{Why{Known: true, Facts: []WhyFact{{ID: 1}}}, "These are the things you've told me that I drew on:"},
		{Why{Known: true}, "Nothing you've told me came into that one."},
	} {
		if got := whyLead(c.w); got != c.want {
			t.Errorf("whyLead(%+v) = %q, want %q", c.w, got, c.want)
		}
	}
}
