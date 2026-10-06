package agent

import "testing"

// The portrait's last line on what's new comes off the portrait, however
// the model set it out, and NONE leaves nothing new.
func TestSplitPortraitTakesOffTheNewLine(t *testing.T) {
	const p = "You like quiet mornings and plain answers."
	for _, c := range []struct{ reply, text, news string }{
		{p + "\nNEW: you've been guarding Friday afternoons.", p, "you've been guarding Friday afternoons."},
		{p + "\n\n**NEW:** you've started running again", p, "you've started running again"},
		{p + " NEW: Friday afternoons are yours now.", p, "Friday afternoons are yours now."},
		{p + "\nNEW: NONE", p, ""},
		{p + "\nNEW: None.", p, ""},
		{p, p, ""},
		// Only a last line counts, and only a word on its own.
		{"NEW: a thing\n" + p, "NEW: a thing\n" + p, ""},
		{p + " RENEW: something", p + " RENEW: something", ""},
	} {
		text, news := splitPortrait(c.reply)
		if text != c.text || news != c.news {
			t.Errorf("%q: got %q, %q; want %q, %q", c.reply, text, news, c.text, c.news)
		}
	}
}
