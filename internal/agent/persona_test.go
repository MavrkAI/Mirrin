package agent

import (
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// With the default (empty) form of address each persona addresses the
// owner its own way: Nyra by name, Mirrin as "sir". The regression: the
// default was "sir", which overrode Nyra's and Pickoo's.
func TestThePersonaDecidesTheDefaultFormOfAddress(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	cfg.User.Name = "Akshaya"

	nyra := persona(cfg, personaData{Name: "Nyra", Character: "Warm and quick."}, nil)
	if !strings.Contains(nyra, "You address Akshaya by name now and then, never with honorifics.") || strings.Contains(nyra, `as "sir"`) {
		t.Fatalf("Nyra's prompt:\n%s", nyra)
	}
	mirrin := persona(cfg, personaData{Name: "Mirrin", Spoken: "Mirrin", Character: "A dry, loyal butler.", Address: "sir"}, nil)
	if !strings.Contains(mirrin, `You address Akshaya as "sir"`) {
		t.Fatalf("Mirrin's prompt:\n%s", mirrin)
	}
	cfg.User.Honorific = "ma'am"
	if p := persona(cfg, personaData{Name: "Nyra"}, nil); !strings.Contains(p, `You address Akshaya as "ma'am"`) {
		t.Fatalf("the owner's choice: \n%s", p)
	}
}

// The twin keeps its word: a promise to check back is a follow_up, and the
// evening "how did it go?" is about what the owner said mattered, never
// their health, money or relationships.
func TestThePromptSaysToKeepPromisesWithAFollowUp(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	p := persona(config.Default(), personaData{Name: "Mirrin"}, nil)
	for _, want := range []string{
		"- When you tell the owner you'll check back on something, set a follow_up so you really do.",
		"you may set one follow_up for that evening to ask how it went, never about health, money or relationships.",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("the prompt doesn't say %q:\n%s", want, p)
		}
	}
}

// The default twin carries the product's name, so his prompt doesn't say
// "Mirrin is the system you run on; Mirrin is who you are", nor how to say
// a name that is said as it is written.
func TestThePromptNamesMirrinOnce(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	p := persona(config.Default(), personaData{Name: "Mirrin", Spoken: "Mirrin", Character: "A dry, loyal butler."}, nil)
	if !strings.HasPrefix(p, "You are Mirrin, ") || strings.Contains(p, "Mirrin is who you are") || strings.Contains(p, "say it") ||
		!strings.Contains(p, "You share your name with Mirrin, the open-source system you run on.") {
		t.Fatalf("Mirrin's prompt:\n%s", p)
	}
	p = persona(config.Default(), personaData{Name: "Nyra", Spoken: "Neera", Character: "Warm."}, nil)
	if !strings.HasPrefix(p, `You are Nyra (say it "Neera"), `) || !strings.Contains(p, "Mirrin is the open-source system you run on; Nyra is who you are.") {
		t.Fatalf("Nyra's prompt:\n%s", p)
	}
}
