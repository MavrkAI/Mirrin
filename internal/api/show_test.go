package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
)

const shownDraft = "Subject: Dinner on Friday\n\nHi Sam, are we still on for eight?"

func (f *fake) ShownAnswer(context.Context) (ShownAnswer, bool) {
	return ShownAnswer{Text: shownDraft, At: time.Now()}, true
}

// A long answer put on the screen is the owner's: their phone may ask what
// it was, a wall screen in the kitchen may not.
func TestAShownAnswerIsTheOwnersOnly(t *testing.T) {
	e := newEnv(t)
	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	_, phone, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	if w := e.do(onRemote, req{path: "/show", header: bearer(kiosk)}); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "Sam") {
		t.Fatalf("kiosk /show: %d %s", w.Code, w.Body)
	}
	if w := e.do(onRemote, req{path: "/show", header: bearer(phone)}); w.Code != 200 || !strings.Contains(w.Body.String(), "Dinner on Friday") {
		t.Fatalf("phone /show: %d %s", w.Code, w.Body)
	}
}

// A wall screen hears what was said out loud, but not an answer that went
// on the owner's screen instead: its "show" card, nor its line marked shown.
func TestAWallScreenNeverSeesAShownAnswer(t *testing.T) {
	e := newEnv(t)
	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	_, phone, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	publish := func(b *events.Bus) {
		b.Publish(events.Event{Kind: "said", Text: shownDraft, Data: map[string]string{"channel": "voice", "shown": "screen"}})
		b.Publish(events.Event{Kind: "show", Text: shownDraft})
		b.Publish(events.Event{Kind: "said", Text: "18 degrees and cloudy", Data: map[string]string{"channel": "voice"}})
	}
	got := e.heardOnEvents(kiosk, publish, "18 degrees")
	if strings.Contains(got, "Sam") || strings.Contains(got, `"show"`) {
		t.Fatalf("a wall screen saw the shown answer:\n%s", got)
	}
	got = e.heardOnEvents(phone, publish, "18 degrees")
	if !strings.Contains(got, `"kind":"show"`) || !strings.Contains(got, "Dinner on Friday") {
		t.Fatalf("the phone didn't get the shown answer:\n%s", got)
	}
}
