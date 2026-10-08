package api

import (
	"context"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// A long answer given out loud at this computer (a draft, a list, a table)
// is put on the presence screen: the twin says the gist, and the screen
// shows the whole of it. The screen hears of it as a "show" event, and one
// opened for it (/ui#show) asks here for what to show. Only the owner's own
// devices see it: a screen paired only to look gets neither.

// ShownAnswer is the answer last put on the screen.
type ShownAnswer struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// ShowBackend is a Backend that puts long answers on the screen.
type ShowBackend interface {
	// ShownAnswer is the answer last put on the screen, while recent.
	ShownAnswer(ctx context.Context) (ShownAnswer, bool)
}

func (s *Server) showRoutes(mux *http.ServeMux, a authz) {
	b, ok := s.backend.(ShowBackend)
	if !ok {
		return
	}
	owner := []devices.Scope{devices.Chat, devices.Approve}
	mux.HandleFunc("GET /show", a.requireAny(owner, func(w http.ResponseWriter, r *http.Request) {
		got, ok := b.ShownAnswer(r.Context())
		if !ok {
			s.fail(w, r, http.StatusNotFound, apiError{Error: "nothing_shown", Message: "There's nothing on the screen to show right now."})
			return
		}
		writeJSON(w, got)
	}))
}
