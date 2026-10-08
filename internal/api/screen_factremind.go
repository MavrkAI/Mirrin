package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// A noted fact with a date in it ("Mum's birthday is on the 12th") asks,
// beside its Undo, "Remind me on the 11th?" (the "remembered" event's
// "remind", daemon/datereminder.go). One tap sets that reminder. Setting a
// reminder is something said to the twin, so it needs a device that may
// chat, not a wall screen that only looks.

// FactReminder is a Backend that can set the reminder a noted fact offered.
type FactReminder interface {
	// RemindFact sets it and says when it will come ("I'll remind you on
	// the 11th."). It returns ErrNoOffer when the fact offers none (any
	// more).
	RemindFact(ctx context.Context, id int64) (string, error)
}

// ErrNoOffer means a fact has no reminder to offer from a screen: it has no
// date still to come, is private, is from elsewhere, is gone, or was noted
// too long ago.
var ErrNoOffer = errors.New("that fact has no reminder to offer")

// factRemindRoutes serves POST /screen/facts/{id}/remind, when the backend
// can set them.
func (s *Server) factRemindRoutes(mux *http.ServeMux, a authz) {
	fr, ok := s.backend.(FactReminder)
	if !ok {
		return
	}
	mux.HandleFunc("POST /screen/facts/{id}/remind", a.Require(devices.Chat, func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		said, err := fr.RemindFact(r.Context(), id)
		switch {
		case errors.Is(err, ErrNoOffer):
			s.fail(w, r, http.StatusConflict, apiError{Error: "no_offer", Message: "I can't set that one from here now. Ask me to remind you and I will."})
		case err != nil:
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "not_set", Message: "That reminder couldn't be set just now.", Fix: "Try again, or ask me to remind you."})
		default:
			writeJSON(w, map[string]string{"said": said})
		}
	}))
}
