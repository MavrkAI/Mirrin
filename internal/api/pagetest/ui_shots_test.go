package pagetest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// busyDay is a realistic presence screen: a request waiting, a task with a
// question, the day's events and reminders, a conversation, the weather.
func busyDay() map[string]any {
	now := time.Now()
	at := func(h int) string { return now.Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }
	return screen(func(m map[string]any) {
		m["weather"] = map[string]any{"temp_c": 18.4, "summary": "light rain"}
		m["approvals"] = []any{map[string]any{"id": 12, "tool": "send_email", "summary": "Email Sam Chen the signed lease (lease-final.pdf)", "risk": "write", "status": "pending", "created_at": at(0), "chat": "whatsapp:owner"}}
		m["tasks"] = []any{
			map[string]any{"id": "t1", "title": "Plan the Bali trip", "status": "waiting_user", "question": "Comfortable ($2,400) or mid-range ($1,900)?", "asked_at": at(0), "steps": []any{map[string]any{"text": "Flights", "done": true}, map[string]any{"text": "Hotels", "done": true}, map[string]any{"text": "Book", "done": false}}, "updated": at(0)},
			map[string]any{"id": "t2", "title": "Chase the Qantas refund", "status": "running", "steps": []any{map[string]any{"text": "Find booking", "done": true}, map[string]any{"text": "Submit claim", "done": false}}, "updated": at(0)},
		}
		m["events"] = []any{
			map[string]any{"title": "Standup", "start": at(1), "end": at(2)},
			map[string]any{"title": "Dentist", "start": at(4), "end": at(5), "location": "Collins St"},
		}
		m["reminders"] = []any{map[string]any{"text": "Call Mum", "due": at(3)}}
		m["recent"] = []any{
			map[string]any{"kind": "heard", "text": "What's on this afternoon?", "at": at(0)},
			map[string]any{"kind": "said", "text": "Standup at two, the dentist at five on Collins Street, and a reminder to call your mum at four. Light rain, eighteen degrees.", "at": at(0)},
		}
	})
}

// MIRRIN_UI_SHOTS=<dir> writes the presence screen, busy, at phone and
// desktop sizes in light and dark, and the orb.
func TestPresenceScreenShots(t *testing.T) {
	dir := os.Getenv("MIRRIN_UI_SHOTS")
	if dir == "" {
		t.Skip("set MIRRIN_UI_SHOTS to a folder to write screenshots of the presence screen")
	}
	_ = os.MkdirAll(dir, 0o755)
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(busyDay()))
	for _, v := range []struct {
		name, path string
		w, h       int64
		dark       bool
	}{
		{"desktop-dark", "/ui", 1440, 900, true}, {"desktop-light", "/ui", 1440, 900, false},
		{"phone-dark", "/ui", 390, 844, true}, {"phone-light", "/ui", 390, 844, false},
		{"orb", "/ui?mode=orb", 380, 300, true},
	} {
		ctx := tab(t)
		scheme := "light"
		if v.dark {
			scheme = "dark"
		}
		run(t, ctx, chromedp.EmulateViewport(v.w, v.h), emulateScheme(scheme), chromedp.Navigate(d.url(v.path)))
		waitFor(t, ctx, loaded, "loaded")
		run(t, ctx, chromedp.Sleep(1200*time.Millisecond))
		var png []byte
		if v.w > 1000 {
			run(t, ctx, chromedp.CaptureScreenshot(&png))
		} else {
			run(t, ctx, chromedp.FullScreenshot(&png, 90))
		}
		_ = os.WriteFile(filepath.Join(dir, v.name+".png"), png, 0o644)
	}
}

func emulateScheme(s string) chromedp.Action {
	return emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: s}})
}
