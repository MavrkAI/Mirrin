package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/persona"
)

// The screen greets the owner for the part of the day, in the persona's
// words: Mirrin says "Good morning, sir." at 7:40 and "Late one, sir." at
// 1am, not "Good evening"; Nyra says "Morning, Akshaya.", or "Morning,
// ma'am." to an owner who chose ma'am.
func TestTheScreenGreetsYouForThePartOfTheDay(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	ctx := context.Background()
	if err := td.UpdateConfig(func(c *config.Config) { c.User.Name = "Akshay Kumar" }); err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) string { return persona.PartOfDay(time.Date(2026, 10, 3, h, m, 0, 0, time.UTC)) }
	hellos := td.screenData(ctx).Hellos
	if got := hellos[at(7, 40)]; got != "Good morning, sir." {
		t.Errorf("Mirrin at 7:40: %q", got)
	}
	if got := hellos[at(1, 0)]; got != "Late one, sir." {
		t.Errorf("Mirrin at 1am: %q", got)
	}
	if got := hellos[at(19, 0)]; got != "Good evening, sir." {
		t.Errorf("Mirrin at 7pm: %q", got)
	}

	asNyra(t, td, "")
	if got := td.screenData(ctx).Hellos[at(7, 40)]; got != "Morning, Akshaya." {
		t.Errorf("Nyra at 7:40: %q", got)
	}
	// Ma'am, chosen for Nyra, is what her hellos call the owner too.
	asNyra(t, td, "ma'am")
	if got := td.screenData(ctx).Hellos[at(7, 40)]; got != "Morning, ma'am." {
		t.Errorf("Nyra to ma'am at 7:40: %q", got)
	}
	// "By my name" saves the first name, and she says it.
	asNyra(t, td, "Akshaya")
	if got := td.screenData(ctx).Hellos[at(7, 40)]; got != "Morning, Akshaya." {
		t.Errorf("Nyra by name at 7:40: %q", got)
	}
	// The owner's own form of address is the one Mirrin uses.
	if err := td.UpdateConfig(func(c *config.Config) { c.Persona, c.User.Honorific = "mirrin", "ma'am" }); err != nil {
		t.Fatal(err)
	}
	if got := td.screenData(ctx).Hellos[at(23, 30)]; got != "Late one, ma'am." {
		t.Errorf("Mirrin to ma'am, late: %q", got)
	}
}

// A persona with no hellos of its own sends none, and the screen says the
// plain words.
func TestHellosForAPersonaWithoutThem(t *testing.T) {
	cfg := config.Default()
	cfg.User.Name = "Akshaya"
	if h := hellosFor(cfg, persona.Persona{Name: "Nova", Character: "Kind."}); h != nil {
		t.Fatalf("hellos %v", h)
	}
}
