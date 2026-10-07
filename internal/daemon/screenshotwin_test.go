package daemon

import (
	"slices"
	"testing"
)

// Screenshot paths were only found when they began with /, so on Windows
// (C:\…) the twin's screenshots never reached the chat.
func TestScreenshotPathsAreFoundInWindowsForm(t *testing.T) {
	text := `Here: C:\Users\me\.mirrin\data\browser-1-1.png and /home/me/.mirrin/data/browser-2-1.png.`
	got := rePNG.FindAllString(text, -1)
	want := []string{`C:\Users\me\.mirrin\data\browser-1-1.png`, "/home/me/.mirrin/data/browser-2-1.png"}
	if !slices.Equal(got, want) {
		t.Fatalf("found %q", got)
	}
}
