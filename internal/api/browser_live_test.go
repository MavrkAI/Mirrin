package api

import (
	"encoding/json"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// The screen always hears whether the twin is using its browser: false is
// said too, so a run starting (false, then true) is never mistaken for a
// field an older twin left out.
func TestBrowserStateSaysWhetherTheTwinIsBrowsing(t *testing.T) {
	e := newEnv(t)
	_, tok, err := e.store.Add("Phone", devices.KindPWA, nil, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, on := range []bool{false, true} {
		e.f.mu.Lock()
		e.f.active = on
		e.f.mu.Unlock()
		w := e.do(onRemote, req{path: "/browser/state", header: bearer(tok)})
		if w.Code != 200 {
			t.Fatalf("a phone watching: %d %s", w.Code, w.Body)
		}
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if got, ok := m["active"]; !ok || got != on {
			t.Fatalf("active %v: the state says %s", on, w.Body)
		}
	}
}

// A paired phone may watch the twin's browser but never drive it: taking
// over acts on signed-in sites as the owner.
func TestBrowserLiveViewAndDriveScopes(t *testing.T) {
	e := newEnv(t)
	_, tok, err := e.store.Add("Phone", devices.KindPWA, nil, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{path: "/browser/state", header: bearer(tok)}); w.Code != 200 {
		t.Fatalf("a phone watching: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/browser/control", "/browser/input"} {
		if w := e.do(onRemote, req{method: "POST", path: p, body: `{"hold":true}`, header: bearer(tok)}); w.Code == 200 || w.Code == 204 {
			t.Fatalf("a phone driving %s: %d", p, w.Code)
		}
	}
	if w := e.do(onLoopback, req{method: "POST", path: "/browser/control", body: `{"hold":true}`, header: bearer(master)}); w.Code != 200 || !e.f.held {
		t.Fatalf("this computer taking over: %d %s", w.Code, w.Body)
	}
}
