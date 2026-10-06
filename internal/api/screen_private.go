package api

import (
	"encoding/json"
	"net/http"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// What a screen paired only to look sees. A wall screen in the kitchen shows
// whoever walks past the time, the weather, the day's plans and that
// something needs you. It never shows the portrait, the owner's chats, an
// email's subject or what an approval would pay for. A device that may chat
// or approve is the owner's own, and sees everything.

// viewOnly reports whether p may only look: it holds none of chat, approve
// or admin. The master key and this computer are never view-only.
func viewOnly(p Peer) bool {
	if p.Master || p.Loopback {
		return false
	}
	return !p.Has(devices.Chat) && !p.Has(devices.Approve) && !p.Has(devices.Admin)
}

// shownToAll are the screen's fields a view-only screen gets as they are.
// Approvals, tasks, Left for you, what is connected and recent lines are
// cut down below. Every other field is private, including one added later:
// it reaches a wall only once it is listed here. A pause and the night's
// quiet hours are the twin's own state, as its state is: a wall shows it
// paused, and dozes at night.
var shownToAll = []string{"name", "state", "time", "health", "problems", "events", "reminders",
	"weather", "ambient_after_seconds", "calendar_error", "persona", "can", "paused", "paused_until", "quiet_hours"}

// redactScreen is the screen's data (as withScopes decodes it) cut down for
// a view-only screen.
func redactScreen(m map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range shownToAll {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	// That something waits, and how risky it is, but not what it is, who
	// asked, or a screenshot of the page.
	if aps, ok := m["approvals"].([]any); ok {
		cut := []any{}
		for _, a := range aps {
			if a, ok := a.(map[string]any); ok {
				cut = append(cut, pick(a, "id", "status", "tool", "risk"))
			}
		}
		out["approvals"] = cut
	}
	// A task's title and how far it has got, but not its goal, notes,
	// question, result or error. When it last moved keeps a finished one on
	// the board for a while, as on every other screen.
	if ts, ok := m["tasks"].([]any); ok {
		cut := []any{}
		for _, t := range ts {
			t, ok := t.(map[string]any)
			if !ok {
				continue
			}
			c := pick(t, "id", "title", "status", "updated")
			steps, _ := t["steps"].([]any)
			done := 0
			for _, s := range steps {
				if s, ok := s.(map[string]any); ok && s["done"] == true {
					done++
				}
			}
			c["steps_done"], c["steps_total"] = done, len(steps)
			cut = append(cut, c)
		}
		out["tasks"] = cut
	}
	// That the twin left something (a routine's result, a reminder) and
	// when, but not what it says.
	if ls, ok := m["left_for_you"].([]any); ok {
		cut := []any{}
		for _, l := range ls {
			if l, ok := l.(map[string]any); ok {
				cut = append(cut, pick(l, "id", "source", "title", "at", "briefing", "held_until"))
			}
		}
		out["left_for_you"] = cut
	}
	// Whether a calendar is connected, so an empty day reads "Calendar not
	// connected" rather than a free one; not what else the twin reaches.
	if c, ok := m["connected"].(map[string]any); ok {
		out["connected"] = pick(c, "calendar")
	}
	// Only what was already said out loud in the room.
	if rs, ok := m["recent"].([]any); ok {
		aloud := []any{}
		for _, r := range rs {
			if r, ok := r.(map[string]any); ok && saidAloud(r["kind"], r["data"]) {
				aloud = append(aloud, r)
			}
		}
		out["recent"] = aloud
	}
	return out
}

// viewOnlyEvent is what a view-only screen hears of ev on GET /events, and
// whether it hears it at all. Kinds not named here are private, including
// ones added later.
func viewOnlyEvent(ev events.Event) (events.Event, bool) {
	switch ev.Kind {
	case "state", "react":
		return ev, true
	case "notice":
		// The twin's own system lines carry no data. A notice that does is
		// a message delivered to a chat, or someone else's request, in their
		// words.
		return ev, ev.Data == nil
	case "approval":
		// That one was raised or settled, so the screen looks again: not
		// what it was for.
		var a struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		}
		if b, err := json.Marshal(ev.Data); err == nil {
			_ = json.Unmarshal(b, &a)
		}
		return events.Event{Kind: ev.Kind, Data: map[string]any{"id": a.ID, "status": a.Status}, At: ev.At}, true
	case "browser":
		ev.Data = nil // not the page's address, nor what it asks
		return ev, true
	case "heard", "said", "note":
		return ev, saidAloud(ev.Kind, ev.Data)
	}
	return events.Event{}, false
}

// saidAloud reports whether a line of conversation (an event's kind and
// data) was spoken in the room, where anyone there heard it already.
func saidAloud(kind, data any) bool {
	switch kind {
	case "heard", "said", "note":
	default:
		return false
	}
	var ch string
	switch d := data.(type) {
	case map[string]string:
		ch = d["channel"]
	case map[string]any:
		ch, _ = d["channel"].(string)
	}
	return ch == "voice"
}

// pick is m with only keys.
func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// requireAny needs a credential holding at least one of scopes.
func (a authz) requireAny(scopes []devices.Scope, h http.HandlerFunc) http.HandlerFunc {
	return a.require(scopes, false, h)
}
