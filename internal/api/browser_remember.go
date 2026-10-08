package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// "Remember this page" on the browser sheet: the page the twin's browser is
// on, kept in memory with where it came from, as "remember this page" said
// in a chat would. Saving to the owner's memory is part of talking to the
// twin, so it needs a device that may chat, not a wall screen that only
// looks.

// BrowserRememberer is a Backend that can keep the browser's page in memory.
type BrowserRememberer interface {
	// BrowserRemember saves the page open now. already says it had been
	// saved before; ErrNoWebPage says there's no web page to save.
	BrowserRemember(ctx context.Context) (already bool, err error)
}

// ErrNoWebPage means the browser has no web page open to remember.
var ErrNoWebPage = errors.New("no web page open")

// browserRememberRoutes serves POST /browser/remember, when the backend can.
func (s *Server) browserRememberRoutes(mux *http.ServeMux, a authz) {
	br, ok := s.backend.(BrowserRememberer)
	if !ok {
		return
	}
	mux.HandleFunc("POST /browser/remember", a.Require(devices.Chat, func(w http.ResponseWriter, r *http.Request) {
		already, err := br.BrowserRemember(r.Context())
		switch {
		case errors.Is(err, ErrNoWebPage):
			s.fail(w, r, http.StatusConflict, apiError{Error: "no_page", Message: "There's no web page open to remember.", Fix: "Open the page first, then press Remember this page."})
		case err != nil:
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "not_saved", Message: "That page couldn't be saved just now.", Fix: "Try again, or say \"remember this page\"."})
		case already:
			writeJSON(w, map[string]any{"saved": true, "already": true, "message": "Already saved. Ask " + s.twinName() + " for it any time."})
		default:
			writeJSON(w, map[string]any{"saved": true, "already": false, "message": "Saved. Ask " + s.twinName() + " for it any time."})
		}
	}))
}
