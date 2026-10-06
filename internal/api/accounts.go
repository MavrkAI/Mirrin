package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

//go:embed accounts.html
var accountsHTML []byte

// Feature is one switchable capability of an account.
type Feature struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	On    bool   `json:"on"`
}

// AccountState is one account as the page shows it.
type AccountState struct {
	Name      string    `json:"name"`
	Label     string    `json:"label"`
	Blurb     string    `json:"blurb"`
	HasClient bool      `json:"has_client"`
	Connected bool      `json:"connected"`
	Email     string    `json:"email,omitempty"`
	Features  []Feature `json:"features"`
	Steps     []Step    `json:"steps"`
	// Notices are what to fix on an account that is connected (an API
	// turned off, a box left unticked), each with a link when there's a
	// page to fix it on.
	Notices []Step `json:"notices,omitempty"`
}

// Step mirrors config.Step for the page.
type Step struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
	Link string `json:"link,omitempty"`
}

// AccountsBackend is what the Accounts page needs.
type AccountsBackend interface {
	AccountStates(ctx context.Context) []AccountState
	SaveGoogleClient(ctx context.Context, clientID, clientSecret, rawJSON string) error
	BeginGoogleBound(ctx context.Context, redirect string) (authURL, binding string, err error)
	FinishGoogleBound(ctx context.Context, state, code, binding string) error
	// ExplainGoogleError puts the error code Google sent back in plain words.
	// It never repeats anything else a link put in the address.
	ExplainGoogleError(code string) string
	SetGoogleFeature(ctx context.Context, key string, on bool) error
	DisconnectGoogle(ctx context.Context) error
}

// WithAccounts enables the Accounts page.
func (s *Server) WithAccounts(b AccountsBackend) *Server { s.accounts = b; return s }

// AccountsURL is the page link.
func (s *Server) AccountsURL() string { return s.localBase() + "/accounts?token=" + s.masterKey() }

// oauthCookie ties a Google sign-in to the browser that started it: it holds
// a secret independent of the URL's state, and the callback needs it.
const oauthCookie = "mirrin_oauth"

// accountRoutes serves the Accounts page: settings, so admin on this computer.
func (s *Server) accountRoutes(mux *http.ServeMux, a authz) {
	mux.HandleFunc("GET /accounts", s.page(a, devices.Admin, accountsHTML, nil))
	s.modelKeyRoutes(mux, a)     // model_key.go
	s.googleClientRoutes(mux, a) // accounts_google.go
	mux.HandleFunc("GET /accounts/list", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		st := s.accounts.AccountStates(ctx)
		if st == nil {
			st = []AccountState{}
		}
		writeJSON(w, st)
	}))
	mux.HandleFunc("POST /accounts/google/client", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			JSON         string `json:"json"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := s.accounts.SaveGoogleClient(r.Context(), req.ClientID, req.ClientSecret, req.JSON); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"saved": "google"})
	}))
	mux.HandleFunc("POST /accounts/google/connect", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		// Google returns the sign-in to 127.0.0.1 on this computer, where the
		// flow's cookie must be too: a page on any other listener (settings
		// opened there with remote admin) couldn't finish it.
		if listenerFrom(r.Context()).kind != kindLoopback {
			s.fail(w, r, http.StatusConflict, apiError{Error: "not_here", Message: "Google only sends a sign-in back to the computer " + s.twinName() + " runs on.",
				Fix: "Open Accounts from " + s.twinName() + "'s menu on that computer, and press Connect there."})
			return
		}
		u, binding, err := s.accounts.BeginGoogleBound(r.Context(), s.oauthRedirect(r))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if binding == "" {
			http.Error(w, "Open Accounts and try connecting Google again.", http.StatusBadRequest)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: binding, Path: "/oauth/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600, Secure: r.TLS != nil})
		writeJSON(w, map[string]string{"url": u})
	}))
	// Google sends the browser here. The state proves it's one of our flows
	// (single use, ten minutes, PKCE-bound); the cookie proves this browser
	// started it, so a link someone else sends can't finish a sign-in here.
	mux.HandleFunc("GET /oauth/google", a.Public(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			// In plain words, and never the address's own text: anyone can
			// send a link to this page (the daemon's ExplainGoogleError).
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "google_refused", Message: s.accounts.ExplainGoogleError(e),
				Fix: "Go back to the Accounts page from " + s.twinName() + "'s menu."})
			return
		}
		c, err := r.Cookie(oauthCookie)
		if err != nil || c.Value == "" {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "other_browser", Message: "This Google sign-in was started in a different browser, or too long ago.",
				Fix: "Open Accounts from " + s.twinName() + "'s menu in the browser you want to use, and press Connect there."})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: "", Path: "/oauth/", MaxAge: -1, HttpOnly: true})
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := s.accounts.FinishGoogleBound(ctx, q.Get("state"), q.Get("code"), c.Value); err != nil {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "google_failed", Message: "Signing in to Google didn't finish: " + err.Error() + ".",
				Fix: "Go back to Accounts and press Connect to try again."})
			return
		}
		http.Redirect(w, r, "/accounts", http.StatusFound)
	}))
	mux.HandleFunc("POST /accounts/google/feature", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key string `json:"key"`
			On  bool   `json:"on"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
			http.Error(w, "bad request", 400)
			return
		}
		if err := s.accounts.SetGoogleFeature(r.Context(), req.Key, req.On); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"ok": req.Key})
	}))
	mux.HandleFunc("POST /accounts/google/disconnect", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		if err := s.accounts.DisconnectGoogle(r.Context()); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"disconnected": "google"})
	}))
}

// oauthRedirect is where Google sends the browser back to. Google's desktop
// clients take only loopback addresses, so it is this computer's loopback
// address, spelt the way the page was opened (127.0.0.1 or localhost) so the
// sign-in cookie set there comes back with it. Never the api.listen address:
// that may be 0.0.0.0 or a Tailscale IP.
func (s *Server) oauthRedirect(r *http.Request) string {
	if listenerFrom(r.Context()).kind == kindLoopback && loopbackHostHeader(r.Host) {
		return "http://" + r.Host + "/oauth/google"
	}
	return s.localBase() + "/oauth/google"
}
