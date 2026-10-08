package browser

import (
	"strings"
	"testing"
)

// A page handed over by voice is put in front of the owner: the screen
// opens at it, and the twin says it's there now, not "go find the screen".
// When it couldn't be shown, the twin is told so plainly: before, it heard
// "it's on the presence screen" and told the owner "it's on your screen
// now" while they were looking at nothing.
func TestHandOverByVoiceShowsTheScreen(t *testing.T) {
	var shown []string
	show := ScreenInSight
	s := &Session{
		OnHandOver: func(string, string) bool { return false },
		ScreenURL:  func(string) string { return "http://127.0.0.1:1/ui" },
		ShowScreen: func(chatKey string) ScreenShow { shown = append(shown, chatKey); return show },
	}
	here := "https://accounts.google.example/signin"
	if got := s.handedOver("voice:local", here, "Sign in"); !strings.Contains(got, "is open in front of the user now") || strings.Contains(got, "click the orb") {
		t.Fatalf("voice, screen opened: %q", got)
	}
	show = ScreenNotShown
	got := s.handedOver("voice:local", here, "Sign in")
	if strings.Contains(got, "in front of the user now") || strings.Contains(got, "is on the presence screen") ||
		!strings.Contains(got, "could not be put in front of them, so don't say it's on their screen") ||
		!strings.Contains(got, "Open screen… in the") || !strings.Contains(got, "http://127.0.0.1:1/ui in their browser; by voice, don't read the address out") ||
		strings.Contains(got, "ask you to open the screen") {
		t.Fatalf("voice, screen not shown: %q", got)
	}
	show = ScreenNotTried
	if got := s.handedOver("screen:local", here, "Sign in"); !strings.Contains(got, "is on the presence screen (http://127.0.0.1:1/ui)") {
		t.Fatalf("typed on the screen: %q", got)
	}
	s.ScreenURL = func(string) string { return "" }
	s.handedOver("whatsapp:447700900000", here, "Sign in")
	if len(shown) != 3 || shown[0] != "voice:local" || shown[2] != "screen:local" {
		t.Fatalf("asked to show the screen for %q", shown)
	}
}
