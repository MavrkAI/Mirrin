package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

//go:embed protocols.html
var protocolsHTML []byte

// ProtocolInfo is one protocol as the store shows it.
type ProtocolInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Schedule    string   `json:"schedule"`
	Pack        string   `json:"pack"`
	Author      string   `json:"author"`
	Version     string   `json:"version"`
	Tags        []string `json:"tags"`
	Requires    []string `json:"requires"`
	Missing     []string `json:"missing"`
	Enabled     bool     `json:"enabled"`
	// Prompt is the instructions, in a preview of a pack not yet installed.
	Prompt string `json:"prompt,omitempty"`
	// When is the schedule in words ("weekdays at 8:30"), and Skipping
	// that its next scheduled run is being skipped, as the owner asked.
	When     string `json:"when,omitempty"`
	Skipping bool   `json:"skipping,omitempty"`
}

// PackInfo is a registry or installed pack.
type PackInfo struct {
	Dir         string   `json:"dir,omitempty"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Repo        string   `json:"repo"`
	Tags        []string `json:"tags"`
	Version     string   `json:"version"`
	Installed   bool     `json:"installed"`
	// Commit is the commit an installed pack is pinned to (from git).
	Commit string `json:"commit,omitempty"`
	// Update is what POST /protocols/check-updates found for it.
	Update *PackUpdate `json:"update,omitempty"`
}

// ProtocolsBackend is what the store needs.
type ProtocolsBackend interface {
	InstalledProtocols(ctx context.Context) []ProtocolInfo
	InstalledPacks(ctx context.Context) []PackInfo
	SearchRegistry(ctx context.Context, term string) ([]PackInfo, error)
	InstallPack(ctx context.Context, nameOrURL string) (string, error)
	RemovePack(ctx context.Context, name string) error
	RunProtocol(ctx context.Context, name string) error
}

// WithProtocols enables the protocol store.
func (s *Server) WithProtocols(b ProtocolsBackend) *Server { s.protocols = b; return s }

// ProtocolsURL is the store page link.
func (s *Server) ProtocolsURL() string { return s.localBase() + "/protocols?token=" + s.masterKey() }

// protocolRoutes serves the protocol store (and "Run now", POST
// /protocols/run, in api.go): installing standing orders is a setting, so
// admin on this computer.
func (s *Server) protocolRoutes(mux *http.ServeMux, a authz) {
	mux.HandleFunc("GET /protocols", s.page(a, devices.Admin, protocolsHTML, nil))
	s.packUpdateRoutes(mux, a) // protocols_updates.go
	s.protocolEditRoutes(mux, a)
	mux.HandleFunc("GET /protocols/installed", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		ps, pk := s.protocols.InstalledProtocols(r.Context()), s.packsWithCommits(r.Context())
		if ps == nil {
			ps = []ProtocolInfo{}
		}
		if pk == nil {
			pk = []PackInfo{}
		}
		writeJSON(w, map[string]any{"protocols": ps, "packs": pk})
	}))
	mux.HandleFunc("GET /protocols/search", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		hits, err := s.protocols.SearchRegistry(ctx, r.URL.Query().Get("q"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if hits == nil {
			hits = []PackInfo{}
		}
		writeJSON(w, hits)
	}))
	mux.HandleFunc("POST /protocols/install", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "bad request", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		name, err := s.protocols.InstallPack(ctx, req.Name)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"installed": name})
	}))
	mux.HandleFunc("POST /protocols/remove", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "bad request", 400)
			return
		}
		if err := s.protocols.RemovePack(r.Context(), req.Name); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"removed": req.Name})
	}))
}

// ProtocolEditor is a ProtocolsBackend that changes a protocol from the
// Routines page: on or off, skip its next run, or a new schedule.
type ProtocolEditor interface {
	SetProtocolEnabled(ctx context.Context, name string, on bool) error
	SkipNextProtocol(ctx context.Context, name string) error
	RescheduleProtocol(ctx context.Context, name, schedule string) error
}

// protocolEditRoutes are the switch, Skip next and the time field on the
// Routines page. Like Run now they change standing orders, so they are
// admin on this computer. Each answers with the protocol as it is now.
func (s *Server) protocolEditRoutes(mux *http.ServeMux, a authz) {
	e, ok := s.protocols.(ProtocolEditor)
	if !ok {
		return
	}
	mux.HandleFunc("POST /protocols/{name}/enabled", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		s.protocolChanged(w, r, e.SetProtocolEnabled(r.Context(), r.PathValue("name"), *req.Enabled))
	}))
	mux.HandleFunc("POST /protocols/{name}/skip", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		s.protocolChanged(w, r, e.SkipNextProtocol(r.Context(), r.PathValue("name")))
	}))
	mux.HandleFunc("POST /protocols/{name}/schedule", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Schedule string `json:"schedule"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Schedule) == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		s.protocolChanged(w, r, e.RescheduleProtocol(r.Context(), r.PathValue("name"), req.Schedule))
	}))
}

// protocolChanged answers a change with the protocol as it is now, or says
// in words why it couldn't be made.
func (s *Server) protocolChanged(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	for _, p := range s.protocols.InstalledProtocols(r.Context()) {
		if strings.EqualFold(p.Name, strings.TrimSpace(name)) {
			writeJSON(w, p)
			return
		}
	}
	writeJSON(w, map[string]string{"ok": name})
}
