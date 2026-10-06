package api

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// A tip card's "No more tips" turns the tips off from a device that may
// chat; a wall screen that only looks can't.
func TestNoMoreTipsFromTheScreen(t *testing.T) {
	e := newEnv(t)
	_, phone, err := e.store.Add("Phone", devices.KindPWA, []devices.Scope{devices.Chat}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/tips/off", header: bearer(phone)}); w.Code != 200 || e.f.tipsOff != 1 {
		t.Fatalf("from the phone: %d %s (stopped %d)", w.Code, w.Body, e.f.tipsOff)
	}
	_, wall, err := e.store.Add("Wall", devices.KindPWA, []devices.Scope{devices.View}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/tips/off", header: bearer(wall)}); w.Code != 403 || e.f.tipsOff != 1 {
		t.Fatalf("from a wall screen: %d (stopped %d)", w.Code, e.f.tipsOff)
	}
}
