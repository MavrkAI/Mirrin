package api

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// A click on the orb asks voice on this computer to listen now.
func TestOrbClickListensOnThisComputerOnly(t *testing.T) {
	e := newEnv(t)
	if w := e.do(onLoopback, req{method: "POST", path: "/voice/listen", header: bearer(master)}); w.Code != 200 || e.f.listens != 1 {
		t.Fatalf("from this computer: %d %s (listens %d)", w.Code, w.Body, e.f.listens)
	}
	// Another device can't open this computer's microphone.
	_, tok, err := e.store.Add("Phone", devices.KindPWA, nil, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/voice/listen", header: bearer(tok)}); w.Code == 200 || e.f.listens != 1 {
		t.Fatalf("from a phone: %d (listens %d)", w.Code, e.f.listens)
	}
	// With voice off, the orb gets a plain reason and what to do.
	e.f.listenErr = errors.New("off")
	w := e.do(onLoopback, req{method: "POST", path: "/voice/listen", header: bearer(master)})
	var ae apiError
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &ae) != nil || ae.Error != "voice_off" || ae.Fix == "" {
		t.Fatalf("voice off: %d %s", w.Code, w.Body)
	}
}

// A click on the orb while a page waits opens the screen on this computer,
// and only there.
func TestOrbClickOpensTheScreenOnThisComputerOnly(t *testing.T) {
	e := newEnv(t)
	if w := e.do(onLoopback, req{method: "POST", path: "/screen/open", header: bearer(master)}); w.Code != 200 || e.f.opens != 1 {
		t.Fatalf("from this computer: %d %s (opens %d)", w.Code, w.Body, e.f.opens)
	}
	_, tok, err := e.store.Add("Phone", devices.KindPWA, nil, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/open", header: bearer(tok)}); w.Code == 200 || e.f.opens != 1 {
		t.Fatalf("from a phone: %d (opens %d)", w.Code, e.f.opens)
	}
}

// A "Needs you" card's Drop cancels its task, for a device that may approve.
func TestDropATaskFromTheScreen(t *testing.T) {
	e := newEnv(t)
	if w := e.do(onLoopback, req{method: "POST", path: "/tasks/09233/cancel", header: bearer(master)}); w.Code != 200 || len(e.f.cancelled) != 1 || e.f.cancelled[0] != "09233" {
		t.Fatalf("drop: %d %s %v", w.Code, w.Body, e.f.cancelled)
	}
	_, tok, err := e.store.Add("Wall", devices.KindPWA, []devices.Scope{devices.View}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/tasks/1/cancel", header: bearer(tok)}); w.Code == 200 {
		t.Fatalf("a view-only screen dropped a task: %d", w.Code)
	}
}
