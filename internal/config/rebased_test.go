package config

import "testing"

// On Windows a moved path came out half and half ("~/.mirrin\tts",
// "C:\Users\me\.mirrin/models/x.bin"), as / and \ are alike there.
func TestRebasedPathKeepsOneSeparator(t *testing.T) {
	for _, c := range []struct {
		fold                 bool
		orig, to, rest, want string
	}{
		{true, `~\.old\tts`, "~/.mirrin", `\tts`, `~\.mirrin\tts`},
		{true, "~/.old/models/x.bin", "~/.mirrin", "/models/x.bin", "~/.mirrin/models/x.bin"},
		{true, "~/.old/models/x.bin", `C:\Users\me\.mirrin`, "/models/x.bin", `C:\Users\me\.mirrin\models\x.bin`},
		{false, `~/.old/a\b`, "~/.mirrin", `/a\b`, `~/.mirrin/a\b`},
	} {
		if got := rebasedPath(c.fold, c.orig, c.to, c.rest); got != c.want {
			t.Errorf("fold %v %q: %q, want %q", c.fold, c.orig, got, c.want)
		}
	}
}
