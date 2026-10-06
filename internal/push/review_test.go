package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFullPreviewLongApprovalDelivered(t *testing.T) {
	for _, summary := range []string{strings.Repeat("x", 5000), strings.Repeat("界<&", 5000)} {
		_, sub := vector(t)
		store, _ := Open("")
		if err := store.Put(sub); err != nil {
			t.Fatal(err)
		}
		sent := 0
		d := NewDispatcher(store, func(_ context.Context, _ Subscription, b []byte, _, _ string) error {
			sent++
			var p map[string]any
			if err := json.Unmarshal(b, &p); err != nil {
				t.Fatal(err)
			}
			body := p["b"].(string)
			if len(b) > 3993 || !strings.HasSuffix(body, "…") || !strings.HasPrefix(summary, strings.TrimSuffix(body, "…")) || p["notification"].(map[string]any)["body"] != body {
				t.Fatal("invalid shortened payload")
			}
			return nil
		})
		d.Settings = func() (string, Config) { return "Ava", Config{Preview: "full"} }
		d.Approves = func(string) bool { return true }
		d.Enqueue(Approval(12, summary, "pending", "Ava", 1), "")
		key, item := d.next()
		if item == nil {
			t.Fatal("not queued")
		}
		err := d.deliver(t.Context(), item)
		d.finish(key, item, err)
		if err != nil || sent != 1 || len(d.pending) != 0 {
			t.Fatalf("sent=%d err=%v pending=%d", sent, err, len(d.pending))
		}
	}
}

func TestResolvedAndTestPreviewText(t *testing.T) {
	for _, preview := range []string{"private", "brief", "full", "Private", "none"} {
		for _, status := range []string{"approved", "denied", "expired", "superseded"} {
			n := Approval(12, "secret", status, "Ava", 2)
			n.Origin = "https://twin.test"
			b, err := n.Payload("Ava", preview, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			var p map[string]any
			json.Unmarshal(b, &p)
			decl := p["notification"].(map[string]any)
			if p["u"] != "/ui" || decl["navigate"] != "https://twin.test/ui" || decl["body"] != "This request no longer needs you." || bytes.Contains(b, []byte("secret")) {
				t.Fatal(string(b))
			}
		}
		b, err := (Notification{Kind: "test"}).Payload("Ava", preview, time.Now())
		if err != nil || !bytes.Contains(b, []byte("Notifications are ready.")) {
			t.Fatal(string(b), err)
		}
	}
}

func TestPreviewValidationAndQuietHoursWarning(t *testing.T) {
	for _, preview := range []string{"Private", "none"} {
		var logs bytes.Buffer
		d := NewDispatcher(nil, nil)
		d.Log = slog.New(slog.NewTextHandler(&logs, nil))
		d.Settings = func() (string, Config) { return "Ava", Config{Preview: preview, QuietHours: "bad"} }
		_, c := d.settings()
		b, err := Approval(12, "payment secret", "pending", "Ava", 1).Payload("Ava", c.Preview, time.Now())
		if err != nil || bytes.Contains(b, []byte("payment secret")) || c.Preview != "private" {
			t.Fatal(string(b), c, err)
		}
		if !strings.Contains(logs.String(), "quiet_hours") || (preview == "none" && !strings.Contains(logs.String(), "unknown push preview")) {
			t.Fatal(logs.String())
		}
	}
}

func TestSubscriptionMovesToRepairedDevice(t *testing.T) {
	_, sub := vector(t)
	store, _ := Open(filepath.Join(t.TempDir(), "push.json"))
	if err := store.Put(sub); err != nil {
		t.Fatal(err)
	}
	old := sub
	sub.DeviceID = "new-device"
	if err := store.Put(sub); err != nil {
		t.Fatal(err)
	}
	if store.contains(old) {
		t.Fatal("old queued subscription remains active")
	}
	if err := store.Delete(old.DeviceID, ""); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(store.path)
	if err != nil || len(reopened.List()) != 1 || !reopened.contains(sub) {
		t.Fatal("transfer not persisted", err)
	}
}

func TestTestPushBypassesFiltersAndFailuresAreLogged(t *testing.T) {
	_, sub := vector(t)
	store, _ := Open("")
	store.Put(sub)
	var logs bytes.Buffer
	sent := 0
	d := NewDispatcher(store, func(context.Context, Subscription, []byte, string, string) error { sent++; return nil })
	d.Log = slog.New(slog.NewTextHandler(&logs, nil))
	d.Settings = func() (string, Config) { return "Ava", Config{Kinds: []string{"approval"}, QuietHours: "00:00-23:59"} }
	d.Approves = func(string) bool { return true }
	d.Test(sub.DeviceID, "Ava")
	k, item := d.next()
	if item == nil {
		t.Fatal("test filtered")
	}
	if err := d.deliver(t.Context(), item); err != nil || sent != 1 {
		t.Fatal(sent, err)
	}
	d.finish(k, item, errors.New("push service returned 503"))
	if !strings.Contains(logs.String(), "503") || !strings.Contains(logs.String(), "will retry") {
		t.Fatal(logs.String())
	}
	item.attempts = 5
	d.finish(k, item, errors.New("push service returned 503"))
	if !strings.Contains(logs.String(), "retries exhausted") {
		t.Fatal(logs.String())
	}
	d.Enqueue(Approval(1, "x", "pending", "Ava", 1), "")
	_, item = d.next()
	d.Send = func(context.Context, Subscription, []byte, string, string) error { return ErrGone }
	if err := d.deliver(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "subscription removed") {
		t.Fatal(logs.String())
	}
}

func TestPayloadFailureRemainsQueued(t *testing.T) {
	_, sub := vector(t)
	store, _ := Open("")
	store.Put(sub)
	var logs bytes.Buffer
	d := NewDispatcher(store, func(context.Context, Subscription, []byte, string, string) error {
		t.Fatal("invalid payload sent")
		return nil
	})
	d.Log = slog.New(slog.NewTextHandler(&logs, nil))
	d.Approves = func(string) bool { return true }
	d.Enqueue(Notification{Kind: "approval", Title: strings.Repeat("x", 5000), Tag: "approval-1"}, "")
	key, item := d.next()
	err := d.deliver(t.Context(), item)
	d.finish(key, item, err)
	if err == nil || len(d.pending) != 1 || !strings.Contains(logs.String(), "could not be encoded") {
		t.Fatal(err, len(d.pending), logs.String())
	}
}
