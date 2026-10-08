package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

//go:embed ui.html
var uiHTML []byte

// charactersJS draws the characters for the screen and the welcome page.
// It is code only, served to anyone (pwa.go), like the app's icons.
//
//go:embed characters.js
var charactersJS []byte

// screenRoutes serves the presence screen and its live feed, on every
// listener: this is what a paired phone or wall screen shows.
// eventsKeepalive is how often /events says it is still there, well inside
// the idle limits of proxies and phones. A variable for tests.
var eventsKeepalive = 20 * time.Second

func (s *Server) screenRoutes(mux *http.ServeMux, a authz) {
	mux.HandleFunc("GET /ui", s.page(a, devices.View, injectHead(uiHTML, s.twinName()), nil))
	mux.HandleFunc("GET /screen", a.Require(devices.View, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		writeJSON(w, withScopes(s.screen.Screen(ctx), PeerFrom(r.Context())))
	}))
	// A screenshot of what an approval will submit is for a device that may
	// talk to the twin or decide, not a wall screen that only looks.
	mux.HandleFunc("GET /screen/shot", a.requireAny([]devices.Scope{devices.Chat, devices.Approve}, func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.screen.ScreenshotPath(r.URL.Query().Get("path"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, p)
	}))
	mux.HandleFunc("POST /approvals/{id}/{decision}", a.Require(devices.Approve, func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", 400)
			return
		}
		decision := r.PathValue("decision")
		if decision != "approve" && decision != "deny" {
			http.Error(w, "decision must be approve or deny", 400)
			return
		}
		// The approved action runs detached, with its own time limit: if this
		// page closes or the phone locks, only the reply to it is lost.
		// Another device may need a passkey first (approve.go).
		s.decideApproval(w, r, id, decision)
	}))
	// Drop a task the owner no longer wants (a "Needs you" card's Drop).
	if tc, ok := s.backend.(TaskCanceller); ok {
		mux.HandleFunc("POST /tasks/{id}/cancel", a.Require(devices.Approve, func(w http.ResponseWriter, r *http.Request) {
			if err := tc.CancelTask(r.Context(), r.PathValue("id")); err != nil {
				s.fail(w, r, http.StatusConflict, apiError{Error: "not_cancelled", Message: err.Error()})
				return
			}
			writeJSON(w, map[string]bool{"cancelled": true})
		}))
	}
	s.tipsRoutes(mux, a)         // screen_tips.go: a tip card's "No more tips"
	s.factUndoRoutes(mux, a)     // screen_memory.go: a noted fact's Undo
	s.reminderDoneRoutes(mux, a) // screen_memory.go: a reminder's tick
	s.factRemindRoutes(mux, a)   // screen_factremind.go: a noted date's "Remind me…?"
	s.portraitAckRoutes(mux, a)  // screen_memory.go: the portrait's "That's you"

	s.browserRememberRoutes(mux, a) // browser_remember.go: "Remember this page"
	s.screenSeenRoutes(mux, a)      // screen_seen.go: a screen in sight or not
	mux.HandleFunc("GET /events", a.Require(devices.View, func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		bus := s.screen.Events()
		look := viewOnly(PeerFrom(r.Context())) // screen_private.go
		ch, stop := bus.Subscribe()
		defer stop()
		sc := localScreen(bus, r) // screen_seen.go: a screen on this computer, not the orb
		if sc != nil {
			defer sc.Close()
		}
		// current state first
		fmt.Fprintf(w, "data: %s\n\n", stateEvent(bus.State(), sc, time.Now()))
		fl.Flush()
		keep := time.NewTicker(eventsKeepalive)
		defer keep.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-keep.C:
				fmt.Fprint(w, ": keepalive\n\n")
				fl.Flush()
			case ev := <-ch:
				if look {
					var ok bool
					if ev, ok = viewOnlyEvent(ev); !ok {
						continue
					}
				}
				b, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(string(b), "\n", " "))
				fl.Flush()
			}
		}
	}))
}

// withScopes adds to the screen's data what the device asking may do
// ("can": its scopes), so the page offers no text box to a wall screen
// paired to look, and no Approve/Deny to a device that may not decide. A
// screen that may only look gets only what anyone walking past may see
// (screen_private.go).
func withScopes(data any, p Peer) any {
	var m map[string]any
	b, err := json.Marshal(data)
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	if err != nil || m == nil {
		if !viewOnly(p) {
			return data
		}
		m = map[string]any{} // what can't be read can't be cut down
	}
	can := []devices.Scope{}
	for _, sc := range devices.AllScopes {
		if p.Has(sc) {
			can = append(can, sc)
		}
	}
	m["can"] = can
	if viewOnly(p) {
		return redactScreen(m)
	}
	return m
}

// TaskCanceller is a Backend whose background tasks the owner can drop.
type TaskCanceller interface {
	CancelTask(ctx context.Context, id string) error
}
