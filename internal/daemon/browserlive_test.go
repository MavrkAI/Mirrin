package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/push"
	"github.com/MavrkAI/Mirrin/internal/skills/browser"
)

// The screen hears that the twin is using its browser, as /browser/state
// says it, false included: the screen opens to watch when it turns on.
func TestTheScreenSeesTheTwinBrowsing(t *testing.T) {
	st := screenBrowserState(browser.LiveState{Open: true, Title: "Villa Bali", Active: true})
	if !st.Active {
		t.Fatalf("browser state %+v", st)
	}
	b, _ := json.Marshal(screenBrowserState(browser.LiveState{}))
	if !strings.Contains(string(b), `"active":false`) {
		t.Fatalf("an idle browser reads %s", b)
	}
}

// A new run tells the screen at once, on the feed, with nothing of where:
// the screen asks /browser/state for that.
func TestABrowsingRunTellsTheScreen(t *testing.T) {
	td := newTestDaemon(t, butler)
	ch, stop := td.bus.Subscribe()
	defer stop()
	td.browserActive()
	select {
	case ev := <-ch:
		if ev.Kind != "browser" || ev.Text != "active" || ev.Data != nil {
			t.Fatalf("the feed says %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the screen was never told")
	}
}

// The screen's address is given only to a chat at this computer: from
// WhatsApp, a call, or a task begun on WhatsApp, 127.0.0.1 is a dead link.
func TestScreenAddressOnlyForChatsHere(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.cmu.Lock()
	td.cfg.API.Listen = "127.0.0.1:4321" // only read, never bound
	td.cmu.Unlock()
	for _, key := range []string{"screen:local", "voice:local", "cli:local", "api:local", ""} {
		if got := td.screenAddress(key); got != "http://127.0.0.1:4321/ui" {
			t.Errorf("%q: %q", key, got)
		}
	}
	for _, key := range []string{"whatsapp:447700900000", "telegram:42", "whatsapp:447700900000#task-7", phoneChat, phoneChat + "#call-20261003-101500"} {
		if got := td.screenAddress(key); got != "" {
			t.Errorf("%q: %q", key, got)
		}
	}
}

// A hand-over reaches the owner's phone, even in quiet hours, but never a
// wall screen paired only to look; and the twin hears whether a phone was told.
func TestHandOverPushesToApprovers(t *testing.T) {
	td := newTestDaemon(t, butler)
	devs := devices.NewMemory()
	phone, _, err := devs.Add("Phone", devices.KindPWA, nil, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	kiosk, _, err := devs.Add("Kitchen", devices.KindKiosk, nil, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	store, _ := push.Open("")
	if td.browserHandedOver("https://example.com/signin", "Sign in") {
		t.Fatal("no notifications set up, but the twin was told a phone was")
	}
	for _, id := range []string{phone.ID, kiosk.ID} {
		sub := push.Subscription{Endpoint: "https://fcm.googleapis.com/test/" + id, DeviceID: id}
		sub.Keys.P256DH = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
		sub.Keys.Auth = "BTBZMqHH6r4Tts7J_aSIgg"
		if err := store.Put(sub); err != nil {
			t.Fatal(err)
		}
	}
	type sent struct{ device, kind, tag, title, body, u string }
	heard := make(chan sent, 4)
	dispatch := push.NewDispatcher(store, func(_ context.Context, s push.Subscription, b []byte, kind, tag string) error {
		var p struct{ T, B, U string }
		_ = json.Unmarshal(b, &p)
		heard <- sent{s.DeviceID, kind, tag, p.T, p.B, p.U}
		return nil
	})
	dispatch.Settings = func() (string, push.Config) {
		return td.Config().Name, push.Config{QuietHours: "00:00-23:59"}
	}
	dispatch.Approves = func(id string) bool { return approves(devs, id) }
	pushDispatchers.Store(td.Daemon, dispatch)
	defer pushDispatchers.Delete(td.Daemon)
	go dispatch.Run(t.Context())
	if !td.browserHandedOver("https://example.com/signin", "Sign in") {
		t.Fatal("the phone was told, but the twin wasn't")
	}
	select {
	case s := <-heard:
		if s.device != phone.ID || s.kind != "handover" || s.tag != "handover" || s.title != td.Config().Name+" needs you in the browser" || s.body != "Tap to see what it needs." {
			t.Fatalf("sent %+v", s)
		}
		// a tap opens the screen with the page up
		if s.u != "/ui#browser" {
			t.Fatalf("a tap opens %q", s.u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hand-over push not delivered")
	}
	select {
	case s := <-heard:
		t.Fatalf("a second push: %+v", s)
	case <-time.After(300 * time.Millisecond):
	}
	// With only the wall screen subscribed, no phone is told, and the twin
	// doesn't say one was.
	_ = store.Delete(phone.ID, "")
	if td.browserHandedOver("https://example.com/signin", "Sign in") {
		t.Fatal("only a kiosk is subscribed, but the twin was told a phone was")
	}
}
