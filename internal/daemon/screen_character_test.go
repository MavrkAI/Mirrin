package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/skills/browser"
)

// /screen says which persona is active, so the screen draws Nyra as her own
// character rather than the penguin butler, and follows a switch.
func TestScreenDataCarriesPersona(t *testing.T) {
	off := false
	r := newVoiceRig(t, func(c *config.Config, _ string) {
		c.Persona, c.Name = "nyra", "Nyra"
		c.UI.Weather = &off // no call to the weather service
	})
	r.restart()
	ctx := context.Background()
	sd := r.d.screenData(ctx)
	if sd.Persona != "nyra" {
		t.Fatalf("persona %q on the nyra persona", sd.Persona)
	}
	b, _ := json.Marshal(sd)
	if !strings.Contains(string(b), `"persona":"nyra"`) {
		t.Fatalf("the page reads persona: %s", b)
	}
	r.update(choosePersona("mirrin", "Mirrin"))
	if sd := r.d.screenData(ctx); sd.Persona != "mirrin" {
		t.Fatalf("persona %q after switching to Mirrin", sd.Persona)
	}
}

// /browser/state carries a hand-over and what it asks: the screen's ask above
// the page never showed in the real app, because only the fake sent them.
func TestBrowserStateCarriesHandover(t *testing.T) {
	st := screenBrowserState(browser.LiveState{Open: true, URL: "https://www.jetstar.com/", Title: "Jetstar", Held: true, Handover: true, Ask: "Solve the check, then tap Search."})
	if !st.Handover || st.Ask != "Solve the check, then tap Search." || !st.Held || !st.Open || st.Title != "Jetstar" {
		t.Fatalf("browser state %+v", st)
	}
	b, _ := json.Marshal(st)
	if !strings.Contains(string(b), `"handover":true`) || !strings.Contains(string(b), `"ask":"Solve the check, then tap Search."`) {
		t.Fatalf("the screen reads %s", b)
	}
	if b, _ := json.Marshal(screenBrowserState(browser.LiveState{Open: true, Title: "Jetstar"})); strings.Contains(string(b), "handover") || strings.Contains(string(b), "ask") {
		t.Fatalf("a page the twin is driving claims a hand-over: %s", b)
	}
}
