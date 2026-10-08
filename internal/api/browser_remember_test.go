package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// rememberSays is what the fake's BrowserRemember answers next: "" saves,
// "already" was saved before, "none" has no page open.
var rememberSays = ""

func (f *fake) BrowserRemember(context.Context) (bool, error) {
	f.called("remember-page")
	switch rememberSays {
	case "already":
		return true, nil
	case "none":
		return false, ErrNoWebPage
	}
	return false, nil
}

// "Remember this page" saves from a device that may chat, says so in plain
// words, says when there's nothing to save, and a wall screen that only
// looks can't save anything.
func TestRememberThisPageFromTheScreen(t *testing.T) {
	t.Cleanup(func() { rememberSays = "" })
	e := newEnv(t)
	_, phone, err := e.store.Add("Phone", devices.KindPWA, []devices.Scope{devices.Chat}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		says, message string
		code          int
	}{
		{"", "Saved. Ask Mirrin for it any time.", 200},
		{"already", "Already saved. Ask Mirrin for it any time.", 200},
		{"none", "There's no web page open to remember.", 409},
	} {
		rememberSays = c.says
		w := e.do(onRemote, req{method: "POST", path: "/browser/remember", header: bearer(phone)})
		var got struct{ Message string }
		if w.Code != c.code || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Message != c.message {
			t.Fatalf("%q: %d %s", c.says, w.Code, w.Body)
		}
	}
	_, wall, err := e.store.Add("Wall", devices.KindPWA, []devices.Scope{devices.View}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	e.f.mu.Lock()
	e.f.calls = nil
	e.f.mu.Unlock()
	if w := e.do(onRemote, req{method: "POST", path: "/browser/remember", header: bearer(wall)}); w.Code != 403 || e.f.saw("remember-page") {
		t.Fatalf("saved from a wall screen: %d", w.Code)
	}
}
