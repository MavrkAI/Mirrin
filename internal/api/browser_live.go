package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// The live view of the twin's browser: its state and a stream of frames for
// a paired device that may chat or approve, and take-over (input and
// control) on this computer only, since it acts on signed-in sites as you.
// A wall screen paired only to look never sees the page: it may be your
// inbox or your bank.

// BrowserState is whether there is a page to watch and who is driving.
type BrowserState struct {
	Open  bool   `json:"open"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
	Held  bool   `json:"held"`
	// Handover: the twin handed this page to the owner, and Ask is what it
	// needs them to do there (the screen leads with it).
	Handover bool   `json:"handover,omitempty"`
	Ask      string `json:"ask,omitempty"`
	// Active: the twin is using its browser now (the screen offers to
	// watch, or opens to watch, a run). Always sent, false included.
	Active bool `json:"active"`
}

// BrowserFrame is one JPEG of the page (base64) and the page's size.
type BrowserFrame struct {
	Data string  `json:"d"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
}

// BrowserInput is one thing the owner did on the live view, in page pixels.
type BrowserInput struct {
	Type string  `json:"type"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	DX   float64 `json:"dx,omitempty"`
	DY   float64 `json:"dy,omitempty"`
	Key  string  `json:"key,omitempty"`
	Text string  `json:"text,omitempty"`
}

// BrowserLiveBackend is a Backend with a browser to watch and take over.
type BrowserLiveBackend interface {
	BrowserState(ctx context.Context) BrowserState
	BrowserWatch(ctx context.Context) (<-chan BrowserFrame, error)
	BrowserInput(ctx context.Context, in BrowserInput) error
	BrowserTakeOver(ctx context.Context, on bool) error
}

func (s *Server) browserLiveRoutes(mux *http.ServeMux, a authz) {
	b, ok := s.backend.(BrowserLiveBackend)
	if !ok {
		return
	}
	owner := []devices.Scope{devices.Chat, devices.Approve}
	mux.HandleFunc("GET /browser/state", a.requireAny(owner, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.BrowserState(r.Context()))
	}))
	mux.HandleFunc("GET /browser/stream", a.requireAny(owner, func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		frames, err := b.BrowserWatch(r.Context())
		if err != nil {
			s.fail(w, r, http.StatusConflict, apiError{Error: "no_page", Message: "The browser isn't open right now."})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		keep := time.NewTicker(eventsKeepalive)
		defer keep.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-keep.C:
				fmt.Fprint(w, ": still here\n\n")
				fl.Flush()
			case f, ok := <-frames:
				if !ok {
					fmt.Fprint(w, "event: closed\ndata: {}\n\n")
					fl.Flush()
					return
				}
				data, _ := json.Marshal(f)
				fmt.Fprintf(w, "event: frame\ndata: %s\n\n", data)
				fl.Flush()
			}
		}
	}))
	mux.HandleFunc("POST /browser/input", a.Local(func(w http.ResponseWriter, r *http.Request) {
		var in BrowserInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		if err := b.BrowserInput(r.Context(), in); err != nil {
			s.fail(w, r, http.StatusConflict, apiError{Error: "not_driving", Message: "Take over the browser first.", Fix: err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /browser/control", a.Local(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Hold bool `json:"hold"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := b.BrowserTakeOver(r.Context(), in.Hold); err != nil {
			s.fail(w, r, http.StatusConflict, apiError{Error: "no_browser", Message: err.Error()})
			return
		}
		writeJSON(w, b.BrowserState(r.Context()))
	}))
}
