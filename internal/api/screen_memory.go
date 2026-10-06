package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// A fact the twin keeps in a chat on this Mac shows under its reply as
// "Noted: … · Undo" (the "remembered" event, daemon/remembered.go). Undo
// forgets it as "forget that" would, while it is fresh: taking back what was
// just said is part of talking to the twin, so it needs a device that may
// chat, not a wall screen that only looks. Anything older, or private, is
// forgotten by asking, or on the memory page.

// FactUndoer is a Backend that can forget a fact it has just learned.
type FactUndoer interface {
	// UndoFact forgets fact id. It returns ErrNotUndoable when the fact
	// can't be taken back from a screen.
	UndoFact(ctx context.Context, id int64) error
}

// ErrNotUndoable means a fact is too old to undo from a screen, private,
// learned somewhere other than a chat on this Mac, or not there.
var ErrNotUndoable = errors.New("that fact can't be undone from a screen")

// factUndoRoutes serves POST /screen/facts/{id}/undo, when the backend can
// undo facts.
func (s *Server) factUndoRoutes(mux *http.ServeMux, a authz) {
	fu, ok := s.backend.(FactUndoer)
	if !ok {
		return
	}
	mux.HandleFunc("POST /screen/facts/{id}/undo", a.Require(devices.Chat, func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		switch err := fu.UndoFact(r.Context(), id); {
		case errors.Is(err, ErrNotUndoable):
			s.fail(w, r, http.StatusConflict, apiError{Error: "too_late", Message: "Too late to undo here. Say “forget that” and I will."})
		case err != nil:
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "not_forgotten", Message: "That couldn't be forgotten just now.", Fix: "Try again, or say \"forget that\"."})
		default:
			writeJSON(w, map[string]bool{"forgotten": true})
		}
	}))
}

// A reminder on the screen's day has a tick, for a device that may chat:
// ticking it off is the owner's "done", said with a tap. A wall screen that
// only looks shows the reminder without one.

// ReminderTicker is a Backend that can tick a reminder off.
type ReminderTicker interface {
	// TickReminder records reminder id as done; one that hasn't gone out
	// yet never will. It returns ErrNoReminder when there is none by that
	// number.
	TickReminder(ctx context.Context, id int64) error
}

// ErrNoReminder means there is no reminder by that number (cancelled, say).
var ErrNoReminder = errors.New("no such reminder")

// reminderDoneRoutes serves POST /screen/reminders/{id}/done, when the
// backend can tick reminders off.
func (s *Server) reminderDoneRoutes(mux *http.ServeMux, a authz) {
	rt, ok := s.backend.(ReminderTicker)
	if !ok {
		return
	}
	mux.HandleFunc("POST /screen/reminders/{id}/done", a.Require(devices.Chat, func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		switch err := rt.TickReminder(r.Context(), id); {
		case errors.Is(err, ErrNoReminder):
			s.fail(w, r, http.StatusNotFound, apiError{Error: "no_reminder", Message: "That reminder isn't there any more."})
		case err != nil:
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "not_ticked", Message: "That couldn't be ticked off just now.", Fix: "Try again, or reply \"done\" in the chat it came to."})
		default:
			writeJSON(w, map[string]bool{"done": true})
		}
	}))
}

// The screen's "How Mirrin sees you" asks whether the portrait is right.
// "That's you" puts the question away until the next portrait ("Not quite"
// only opens the text box). Saying so is talking to the twin, so it needs a
// device that may chat; a wall screen that only looks never sees the
// portrait at all.

// PortraitAcker is a Backend that can hear that its portrait is right.
type PortraitAcker interface {
	// AckPortrait records "That's you" for the portrait shown now.
	AckPortrait(ctx context.Context) error
}

// portraitAckRoutes serves POST /screen/portrait/ack, when the backend can
// take it.
func (s *Server) portraitAckRoutes(mux *http.ServeMux, a authz) {
	pa, ok := s.backend.(PortraitAcker)
	if !ok {
		return
	}
	mux.HandleFunc("POST /screen/portrait/ack", a.Require(devices.Chat, func(w http.ResponseWriter, r *http.Request) {
		if err := pa.AckPortrait(r.Context()); err != nil {
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "not_acked", Message: "That didn't reach me just now.", Fix: "Try again in a moment."})
			return
		}
		writeJSON(w, map[string]bool{"acked": true})
	}))
}
