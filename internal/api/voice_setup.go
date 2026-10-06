package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// One-click voice setup: the Set up voice page (GET /voice/setup) and the
// setup itself (POST /voice/setup), which streams each step as it goes.
// They open on this computer only: setup installs software here.

//go:embed voice_setup.html
var voiceSetupHTML []byte

// VoiceStep is one step of voice setup: while it runs, then how it went.
type VoiceStep struct {
	Label   string `json:"label"`
	Running bool   `json:"running,omitempty"`
	OK      bool   `json:"ok"`
	Notes   string `json:"notes,omitempty"`
}

// VoiceSetupBackend is a Backend that can set voice up.
type VoiceSetupBackend interface {
	// VoiceReady reports whether voice is set up already.
	VoiceReady(ctx context.Context) bool
	// SetupVoice runs every step, telling progress as each starts and ends,
	// saves what worked and has the twin hear with it. It returns every
	// step's outcome.
	SetupVoice(ctx context.Context, progress func(VoiceStep)) ([]VoiceStep, error)
}

func (s *Server) voiceSetupRoutes(mux *http.ServeMux, a authz) {
	b, ok := s.backend.(VoiceSetupBackend)
	if !ok {
		return
	}
	var voiceSetupBusy atomic.Bool // one setup at a time
	mux.HandleFunc("GET /voice/setup", s.localPage(voiceSetupHTML))
	mux.HandleFunc("GET /voice/setup/status", a.Local(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ready": b.VoiceReady(r.Context()), "running": voiceSetupBusy.Load()})
	}))
	mux.HandleFunc("POST /voice/setup", a.Local(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		if !voiceSetupBusy.CompareAndSwap(false, true) {
			s.fail(w, r, http.StatusConflict, apiError{Error: "busy", Message: "Voice setup is already running.", Fix: "Wait for it to finish; this page shows it when it does."})
			return
		}
		defer voiceSetupBusy.Store(false)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		var mu sync.Mutex
		emit := func(event string, v any) {
			mu.Lock()
			defer mu.Unlock()
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			fl.Flush()
		}
		// A closed page doesn't stop a download half-way: setup finishes and
		// is saved, and the page shows it next time.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Hour)
		defer cancel()
		steps, err := b.SetupVoice(ctx, func(st VoiceStep) { emit("step", st) })
		if err != nil {
			emit("error", map[string]string{"error": "Voice setup stopped: " + err.Error() + ". Press Set up voice to pick up where it left off."})
			return
		}
		ok = true
		for _, st := range steps {
			ok = ok && st.OK
		}
		emit("done", map[string]any{"ok": ok, "steps": steps})
	}))
}
