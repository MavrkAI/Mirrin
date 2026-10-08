package agent

import (
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// The owner said "I don't see the screen" and the twin kept saying the page
// was there: it only says so when it is, opens the screen when they can't
// see it, and hands over again in a window when the page won't work.
func TestThePromptSaysToUseAWindowWhenTheScreenFails(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	p := persona(config.Default(), personaData{Name: "Mirrin"}, nil)
	for _, want := range []string{
		"Only tell them it's on their screen when browser_signin says so.",
		"If they ask to see the screen or say they can't see it, call open_screen",
		"if they say the page isn't working for them, call browser_signin again with window set to true.",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("the prompt doesn't say %q", want)
		}
	}
	if strings.Contains(p, "in the window that appears") {
		t.Fatal("the prompt still says a window appears for every sign-in")
	}
}
