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
func busyDay() map[string]any { return busyDayAt(time.Now()) }

// busyDayAt is the busy day as of now.
func busyDayAt(now time.Time) map[string]any {
	at := func(h int) string { return now.Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }
	// The day's events fall on the hour, and what the twin said names the
	// same hours, so a screenshot's timeline and conversation agree.
	top := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())
	on := func(h int) string { return top.Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }
	spoken := func(h int) string {
		return []string{"twelve", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven"}[top.Add(time.Duration(h)*time.Hour).Hour()%12]
	}
	return screen(func(m map[string]any) {
		m["weather"] = map[string]any{"temp_c": 18.4, "summary": "light rain"}
		m["approvals"] = []any{map[string]any{"id": 12, "tool": "send_email", "summary": "Email Sam Chen the signed lease (lease-final.pdf)", "risk": "write", "status": "pending", "created_at": at(0), "chat": "whatsapp:owner"}}
		m["tasks"] = []any{
			map[string]any{"id": "t1", "title": "Plan the Bali trip", "status": "waiting_user", "question": "Comfortable ($2,400) or mid-range ($1,900)?", "asked_at": at(0), "steps": []any{map[string]any{"text": "Flights", "done": true}, map[string]any{"text": "Hotels", "done": true}, map[string]any{"text": "Book", "done": false}}, "updated": at(0)},
			map[string]any{"id": "t2", "title": "Chase the Qantas refund", "status": "running", "steps": []any{map[string]any{"text": "Find booking", "done": true}, map[string]any{"text": "Submit claim", "done": false}}, "updated": at(0)},
		}
		m["events"] = []any{
			map[string]any{"title": "Standup", "start": on(1), "end": on(2)},
			map[string]any{"title": "Dentist", "start": on(3), "end": on(4), "location": "Collins St"},
		}
		m["reminders"] = []any{map[string]any{"text": "Call Mum", "due": on(2)}}
		m["recent"] = []any{
			map[string]any{"kind": "heard", "text": "What's on this afternoon?", "at": at(0)},
			map[string]any{"kind": "said", "text": "Standup at " + spoken(1) + ", a reminder to call your mum at " + spoken(2) + ", and the dentist at " + spoken(3) + " on Collins Street. Light rain, eighteen degrees.", "at": at(0)},
		}
	})
}

// shotsNow is when the screenshots are taken: now, or today at
// MIRRIN_UI_SHOTS_AT (HH:MM), so the busy afternoon in the README is an
// afternoon whenever it is drawn again.
func shotsNow() time.Time {
	now := time.Now()
	if at, err := time.Parse("15:04", os.Getenv("MIRRIN_UI_SHOTS_AT")); err == nil {
		return time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	}
	return now
}

// MIRRIN_UI_SHOTS=<dir> writes the presence screen, busy, at phone and
// desktop sizes in light and dark, and the orb; MIRRIN_UI_SHOTS_AT=15:10
// takes them at ten past three today.
func TestPresenceScreenShots(t *testing.T) {
	dir := os.Getenv("MIRRIN_UI_SHOTS")
	if dir == "" {
		t.Skip("set MIRRIN_UI_SHOTS to a folder to write screenshots of the presence screen")
	}
	_ = os.MkdirAll(dir, 0o755)
	now := shotsNow()
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(busyDayAt(now)))
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
		run(t, ctx, chromedp.EmulateViewport(v.w, v.h), emulateScheme(scheme), clockAt(now.Format(time.RFC3339)), chromedp.Navigate(d.url(v.path)))
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
