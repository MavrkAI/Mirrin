package api

import (
	"context"
	"net/http"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// A first-week tip under Left for you has "No more tips", which ends the
// tour from the screen as the words "no more tips" do in a chat. Turning
// the tips off is something said to the twin, so it needs a device that
// may chat with it, not a wall screen that only looks.

// TipsStopper is a Backend whose first-week tips the owner can turn off.
type TipsStopper interface {
	StopTips(ctx context.Context) error
}

// tipsRoutes serves POST /screen/tips/off, when the backend can stop tips.
func (s *Server) tipsRoutes(mux *http.ServeMux, a authz) {
	ts, ok := s.backend.(TipsStopper)
	if !ok {
		return
	}
	mux.HandleFunc("POST /screen/tips/off", a.Require(devices.Chat, func(w http.ResponseWriter, r *http.Request) {
		if err := ts.StopTips(r.Context()); err != nil {
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "tips_not_stopped", Message: "The tips couldn't be turned off.", Fix: "Try again, or say \"no more tips\" in a message."})
			return
		}
		writeJSON(w, map[string]bool{"off": true})
	}))
}
