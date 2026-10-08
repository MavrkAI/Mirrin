package api

import (
	"encoding/json"
	"net/http"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// A presence screen on this computer is counted while its live feed
// (/events) is open, with whether the owner can see it: the page says so as
// it connects (?hidden=1) and whenever that changes (POST /events/seen). A
// page handed over by voice opens a screen unless one here is in sight
// (daemon browserlive.go); the orb, a phone, a wall screen and a background
// tab don't count as in sight.

// localScreen counts the screen r comes from when it is one on this
// computer, or returns nil. Close it when the feed ends.
func localScreen(bus *events.Bus, r *http.Request) *events.Screen {
	p := PeerFrom(r.Context())
	if !p.Loopback || r.URL.Query().Get("view") == "orb" {
		return nil
	}
	if p.Device != nil && p.Device.Kind == devices.KindKiosk {
		return nil // a wall screen that only looks; nobody is sitting at it
	}
	return bus.ScreenHere(r.URL.Query().Get("hidden") != "1")
}

// stateEvent is the first thing /events says: the state now, and the
// screen's own name when it is counted here, to report its sight with.
func stateEvent(state string, sc *events.Screen, at any) []byte {
	m := map[string]any{"kind": "state", "text": state, "at": at}
	if sc != nil {
		m["screen"] = sc.ID()
	}
	b, _ := json.Marshal(m)
	return b
}

// screenSeenRoutes takes a screen's word that it came into sight or left it.
func (s *Server) screenSeenRoutes(mux *http.ServeMux, a authz) {
	mux.HandleFunc("POST /events/seen", a.Require(devices.View, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Screen  string `json:"screen"`
			Visible bool   `json:"visible"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil || in.Screen == "" {
			http.Error(w, "bad request", 400)
			return
		}
		if !s.screen.Events().SeenScreen(in.Screen, in.Visible) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
