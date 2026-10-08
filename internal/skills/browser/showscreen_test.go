package browser

import (
	"strings"
	"testing"
)

// A page handed over by voice is put in front of the owner: the screen
// opens at it, and the twin says it's there now, not "go find the screen".
func TestHandOverByVoiceShowsTheScreen(t *testing.T) {
	var shown []string
	open := true
	s := &Session{
		OnHandOver: func(string, string) bool { return false },
		ScreenURL:  func(string) string { return "http://127.0.0.1:1/ui" },
		ShowScreen: func(chatKey string) bool { shown = append(shown, chatKey); return open },
	}
	here := "https://accounts.google.example/signin"
	if got := s.handedOver("voice:local", here, "Sign in"); !strings.Contains(got, "is open in front of the user now") || strings.Contains(got, "click the orb") {
		t.Fatalf("voice, screen opened: %q", got)
	}
	open = false
	if got := s.handedOver("voice:local", here, "Sign in"); !strings.Contains(got, "is on the presence screen (http://127.0.0.1:1/ui)") {
		t.Fatalf("voice, screen not opened: %q", got)
	}
	s.ScreenURL = func(string) string { return "" }
	s.handedOver("whatsapp:447700900000", here, "Sign in")
	if len(shown) != 2 || shown[0] != "voice:local" {
		t.Fatalf("asked to show the screen for %q", shown)
	}
}
