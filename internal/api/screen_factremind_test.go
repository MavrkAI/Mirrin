package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// RemindFact sets fact 1's reminder, and any other but 2's, which offers none.
func (f *fake) RemindFact(_ context.Context, id int64) (string, error) {
	f.called(fmt.Sprintf("remind:%d", id))
	if id == 2 {
		return "", ErrNoOffer
	}
	return "I'll remind you on the 11th.", nil
}

// A noted fact's "Remind me…?" sets its reminder from a device that may
// chat and says when it will come. One that offers none gets a 409 that
// says what to do instead, and a wall screen that only looks sets nothing.
func TestRemindANotedFactFromTheScreen(t *testing.T) {
	e := newEnv(t)
	_, phone, err := e.store.Add("Phone", devices.KindPWA, []devices.Scope{devices.Chat}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	w := e.do(onRemote, req{method: "POST", path: "/screen/facts/1/remind", header: bearer(phone)})
	var ok struct{ Said string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ok) != nil || ok.Said != "I'll remind you on the 11th." || !e.f.saw("remind:1") {
		t.Fatalf("remind from the phone: %d %s", w.Code, w.Body)
	}
	w = e.do(onRemote, req{method: "POST", path: "/screen/facts/2/remind", header: bearer(phone)})
	var got apiError
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Message != "I can't set that one from here now. Ask me to remind you and I will." {
		t.Fatalf("remind with no offer: %d %s", w.Code, w.Body)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/facts/x/remind", header: bearer(phone)}); w.Code != 400 {
		t.Fatalf("remind of no fact: %d", w.Code)
	}
	_, wall, err := e.store.Add("Wall", devices.KindPWA, []devices.Scope{devices.View}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/facts/3/remind", header: bearer(wall)}); w.Code != 403 || e.f.saw("remind:3") {
		t.Fatalf("remind from a wall screen: %d", w.Code)
	}
}
