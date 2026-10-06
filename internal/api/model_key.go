package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// The model key card on Accounts (/accounts#model): pick a provider, paste a
// key, Test it (POST /accounts/model/check; the welcome page's own is
// POST /welcome/check) and Save it.

// ModelKeyState is the card: the model in use and its key, masked. The key
// itself never leaves the twin.
type ModelKeyState struct {
	Provider  string   `json:"provider"`
	Model     string   `json:"model"`
	Key       string   `json:"key"` // MaskKey'd
	Providers []string `json:"providers"`
}

// ModelKeyBackend is an AccountsBackend that shows and changes the model key.
type ModelKeyBackend interface {
	ModelKey(ctx context.Context) ModelKeyState
	// SaveModelKey makes provider (with key, or the key it has, and model,
	// or its model) the twin's model.
	SaveModelKey(ctx context.Context, provider, key, model string) error
}

// BrainChecker is a WelcomeBackend that tests a key without saving it.
type BrainChecker interface {
	CheckBrain(ctx context.Context, provider, key, model string) error
}

// MaskKey shows enough of a key to recognise it: its first and last few
// characters.
func MaskKey(k string) string {
	switch {
	case k == "":
		return ""
	case len(k) <= 12:
		return "••••"
	}
	return k[:4] + "••••" + k[len(k)-4:]
}

type modelKeyRequest struct {
	Provider string `json:"provider"`
	Key      string `json:"key"`
	Model    string `json:"model"`
}

// modelKeyRoutes adds the card's JSON when the accounts backend has one.
func (s *Server) modelKeyRoutes(mux *http.ServeMux, a authz) {
	mk, ok := s.accounts.(ModelKeyBackend)
	if !ok {
		return
	}
	mux.HandleFunc("GET /accounts/model", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, mk.ModelKey(r.Context()))
	}))
	mux.HandleFunc("POST /accounts/model", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req modelKeyRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.Provider == "" {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_request", Message: "Pick a provider and paste a key."})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if err := mk.SaveModelKey(ctx, req.Provider, req.Key, req.Model); err != nil {
			h := welcomeError(err)
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "not_saved", Message: h.Sentence, Fix: h.Fix})
			return
		}
		writeJSON(w, mk.ModelKey(r.Context()))
	}))
	// Test, on the same mount as Save: an admin device that may save a key
	// may test one too (the welcome route answers on this computer only).
	var c BrainChecker
	if bc, ok := s.accounts.(BrainChecker); ok {
		c = bc
	}
	mux.HandleFunc("POST /accounts/model/check", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := checkModelKey(w, r, c); err != nil {
			h := welcomeError(err)
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_request", Message: h.Sentence, Fix: h.Fix})
		}
	}))
}

// welcomeCheck is POST /welcome/check: does this key work? It answers
// {"ok":true}, or the welcome error in plain words, and never the key.
func welcomeCheck(b WelcomeBackend) func(http.ResponseWriter, *http.Request) error {
	c, _ := b.(BrainChecker)
	return func(w http.ResponseWriter, r *http.Request) error { return checkModelKey(w, r, c) }
}

// checkModelKey answers whether the posted key works, never saving it.
func checkModelKey(w http.ResponseWriter, r *http.Request, c BrainChecker) error {
	if c == nil {
		return &HumanError{Sentence: "Testing a key isn't available here.", Fix: "Save it instead; I check it before using it."}
	}
	var v modelKeyRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return &HumanError{Sentence: "I couldn't read that.", Fix: "Check the fields and try again."}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := c.CheckBrain(ctx, v.Provider, v.Key, v.Model); err != nil {
		// in the settings pages' shape ({error, message, fix}): the Accounts card asks
		h := welcomeError(err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, apiError{Error: "not_accepted", Message: h.Sentence, Fix: h.Fix})
		return nil
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}
