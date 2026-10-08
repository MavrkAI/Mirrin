package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// The quick judgments card on Accounts (/accounts#jev): an optional
// TypeSafe key for Jev. Check tests a key without saving it
// (POST /accounts/jev/check); Save checks, saves and switches
// (POST /accounts/jev). A twin without it answers 404 and the card hides.

// JevState is the card. The key itself never leaves the twin.
type JevState struct {
	On      bool   `json:"on"`      // switched on, with a key
	HasKey  bool   `json:"has_key"` // a key is saved or exported
	Masked  string `json:"masked"`  // MaskKey'd
	Env     string `json:"env"`     // the variable it is kept under
	Problem string `json:"problem"` // in plain words, when the last use failed
}

// JevBackend is an AccountsBackend that shows and changes the TypeSafe key.
type JevBackend interface {
	JevState(ctx context.Context) JevState
	// SaveJev checks and saves key (when given), then switches Jev on or
	// off; forget removes the saved key and switches it off.
	SaveJev(ctx context.Context, key string, on, forget bool) error
	// CheckJev tests key, or the saved one when key is "", without saving.
	CheckJev(ctx context.Context, key string) error
}

type jevRequest struct {
	Key    string `json:"key"`
	On     bool   `json:"on"`
	Forget bool   `json:"forget"`
}

// jevRoutes adds the card's JSON when the accounts backend has one.
func (s *Server) jevRoutes(mux *http.ServeMux, a authz) {
	jb, ok := s.accounts.(JevBackend)
	if !ok {
		return
	}
	mux.HandleFunc("GET /accounts/jev", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, jb.JevState(r.Context()))
	}))
	mux.HandleFunc("POST /accounts/jev", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var req jevRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_request", Message: "I couldn't read that.", Fix: "Paste the key again and press Check and save."})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := jb.SaveJev(ctx, req.Key, req.On, req.Forget); err != nil {
			h := welcomeError(err)
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "not_saved", Message: h.Sentence, Fix: h.Fix})
			return
		}
		writeJSON(w, jb.JevState(r.Context()))
	}))
	mux.HandleFunc("POST /accounts/jev/check", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var req struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_request", Message: "I couldn't read that.", Fix: "Paste the key again and press Check."})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := jb.CheckJev(ctx, req.Key); err != nil {
			h := welcomeError(err)
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "not_accepted", Message: h.Sentence, Fix: h.Fix})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))
}
