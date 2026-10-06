package api

import (
	"context"
	"net/http"
)

// VoiceListenBackend is a Backend whose always-on voice can be asked to
// listen now: a click on the orb.
type VoiceListenBackend interface {
	ListenNow(ctx context.Context) error
}

// voiceListenRoutes serves POST /voice/listen. It opens on this computer
// only: it is this computer's microphone that starts listening.
func (s *Server) voiceListenRoutes(mux *http.ServeMux, a authz) {
	b, ok := s.backend.(VoiceListenBackend)
	if !ok {
		return
	}
	mux.HandleFunc("POST /voice/listen", a.Local(func(w http.ResponseWriter, r *http.Request) {
		if err := b.ListenNow(r.Context()); err != nil {
			s.fail(w, r, http.StatusConflict, apiError{Error: "voice_off",
				Message: "Voice isn't listening on this computer.",
				Fix:     "Set up voice from the menu bar, or turn on always-on listening."})
			return
		}
		writeJSON(w, map[string]bool{"listening": true})
	}))
}

// ScreenOpenBackend opens the presence screen in this computer's browser:
// a click on the orb while Mirrin waits for the owner in the browser.
type ScreenOpenBackend interface {
	OpenScreen(ctx context.Context) error
}

// screenOpenRoutes serves POST /screen/open, on this computer only.
func (s *Server) screenOpenRoutes(mux *http.ServeMux, a authz) {
	b, ok := s.backend.(ScreenOpenBackend)
	if !ok {
		return
	}
	mux.HandleFunc("POST /screen/open", a.Local(func(w http.ResponseWriter, r *http.Request) {
		if err := b.OpenScreen(r.Context()); err != nil {
			s.fail(w, r, http.StatusConflict, apiError{Error: "screen_unavailable",
				Message: "The screen couldn't be opened.",
				Fix:     "Open the screen from the menu bar."})
			return
		}
		writeJSON(w, map[string]bool{"opened": true})
	}))
}
