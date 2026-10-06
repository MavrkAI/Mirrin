package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// UndoFact takes back fact 1 and any other but 2, which is too late to undo.
func (f *fake) UndoFact(_ context.Context, id int64) error {
	f.called(fmt.Sprintf("undo:%d", id))
	if id == 2 {
		return ErrNotUndoable
	}
	return nil
}

// A noted fact's Undo forgets it from a device that may chat. One the twin
// won't take back from a screen gets a 409 that says what to do instead,
// and a wall screen that only looks can't undo anything.
func TestUndoANotedFactFromTheScreen(t *testing.T) {
	e := newEnv(t)
	_, phone, err := e.store.Add("Phone", devices.KindPWA, []devices.Scope{devices.Chat}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/facts/1/undo", header: bearer(phone)}); w.Code != 200 || !e.f.saw("undo:1") {
		t.Fatalf("undo from the phone: %d %s", w.Code, w.Body)
	}
	w := e.do(onRemote, req{method: "POST", path: "/screen/facts/2/undo", header: bearer(phone)})
	var got apiError
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Message != "Too late to undo here. Say “forget that” and I will." {
		t.Fatalf("undo too late: %d %s", w.Code, w.Body)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/facts/x/undo", header: bearer(phone)}); w.Code != 400 {
		t.Fatalf("undo of no fact: %d", w.Code)
	}
	_, wall, err := e.store.Add("Wall", devices.KindPWA, []devices.Scope{devices.View}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/facts/3/undo", header: bearer(wall)}); w.Code != 403 || e.f.saw("undo:3") {
		t.Fatalf("undo from a wall screen: %d", w.Code)
	}
	// Nor does a wall screen hear what was noted.
	if _, ok := viewOnlyEvent(events.Event{Kind: "remembered", Text: "Akshay doesn't eat meat.", Data: map[string]any{"id": 1}}); ok {
		t.Fatal("a wall screen that only looks was told what the twin noted")
	}
}

// TickReminder ticks off reminder 1 and any other but 2, which isn't there.
func (f *fake) TickReminder(_ context.Context, id int64) error {
	f.called(fmt.Sprintf("tick:%d", id))
	if id == 2 {
		return ErrNoReminder
	}
	return nil
}

// A reminder's tick ticks it off from a device that may chat. One that
// isn't there any more says so, and a wall screen that only looks can't
// tick anything.
func TestTickAReminderFromTheScreen(t *testing.T) {
	e := newEnv(t)
	_, phone, err := e.store.Add("Phone", devices.KindPWA, []devices.Scope{devices.Chat}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/reminders/1/done", header: bearer(phone)}); w.Code != 200 || !e.f.saw("tick:1") {
		t.Fatalf("tick from the phone: %d %s", w.Code, w.Body)
	}
	w := e.do(onRemote, req{method: "POST", path: "/screen/reminders/2/done", header: bearer(phone)})
	var got apiError
	if w.Code != 404 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Message != "That reminder isn't there any more." {
		t.Fatalf("tick of a reminder that's gone: %d %s", w.Code, w.Body)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/reminders/x/done", header: bearer(phone)}); w.Code != 400 {
		t.Fatalf("tick of no reminder: %d", w.Code)
	}
	_, wall, err := e.store.Add("Wall", devices.KindPWA, []devices.Scope{devices.View}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/reminders/3/done", header: bearer(wall)}); w.Code != 403 || e.f.saw("tick:3") {
		t.Fatalf("tick from a wall screen: %d", w.Code)
	}
}

// AckPortrait hears "That's you".
func (f *fake) AckPortrait(context.Context) error {
	f.called("portrait:ack")
	return nil
}

// "That's you" under the portrait reaches the twin from a device that may
// chat. A wall screen that only looks never sees the portrait, and can't
// answer for it.
func TestThatsYouFromTheScreen(t *testing.T) {
	e := newEnv(t)
	_, phone, err := e.store.Add("Phone", devices.KindPWA, []devices.Scope{devices.Chat}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/screen/portrait/ack", header: bearer(phone)}); w.Code != 200 || !e.f.saw("portrait:ack") {
		t.Fatalf("That's you from the phone: %d %s", w.Code, w.Body)
	}
	_, wall, err := e.store.Add("Wall", devices.KindKiosk, nil, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	e.f.calls = nil
	if w := e.do(onRemote, req{method: "POST", path: "/screen/portrait/ack", header: bearer(wall)}); w.Code != 403 || e.f.saw("portrait:ack") {
		t.Fatalf("That's you from a wall screen: %d", w.Code)
	}
}
