package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// The protocol store's pins and updates: which commit each installed pack
// is at, what an update would change (POST /protocols/check-updates), moving
// a pack to it (POST /protocols/apply-update), and what a pack would add
// before it is installed (POST /protocols/preview).

// PackUpdate is what checking one installed pack found.
type PackUpdate struct {
	Dir string `json:"dir"`
	// From is the commit installed, To the one available.
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
	Pending bool     `json:"pending"`           // applying would change the pack
	Changes []string `json:"changes,omitempty"` // "changed protocols/news.yaml"
	Note    string   `json:"note,omitempty"`    // why it is left alone (pinned, copied)
	Error   string   `json:"error,omitempty"`
}

// PackPreview is what a pack would add, fetched without installing it.
type PackPreview struct {
	Name      string         `json:"name"`
	Protocols []ProtocolInfo `json:"protocols"` // with Prompt, Schedule and Requires
}

// PackUpdater is a ProtocolsBackend that knows about pins and updates.
type PackUpdater interface {
	// PackCommits is the commit each installed pack (by Dir) is at.
	PackCommits(ctx context.Context) map[string]string
	CheckPackUpdates(ctx context.Context) ([]PackUpdate, error)
	// ApplyPackUpdate moves the pack in dir to the commit a check finds.
	ApplyPackUpdate(ctx context.Context, dir string) error
	PreviewPack(ctx context.Context, nameOrURL string) (PackPreview, error)
}

// packsWithCommits is the installed packs, with their commits when the
// backend knows them.
func (s *Server) packsWithCommits(ctx context.Context) []PackInfo {
	pk := s.protocols.InstalledPacks(ctx)
	if u, ok := s.protocols.(PackUpdater); ok {
		commits := u.PackCommits(ctx)
		for i := range pk {
			if pk[i].Commit == "" {
				pk[i].Commit = commits[pk[i].Dir]
			}
		}
	}
	return pk
}

func (s *Server) packUpdateRoutes(mux *http.ServeMux, a authz) {
	u, ok := s.protocols.(PackUpdater)
	if !ok {
		return
	}
	mux.HandleFunc("POST /protocols/check-updates", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		ups, err := u.CheckPackUpdates(ctx)
		if err != nil {
			s.fail(w, r, http.StatusBadGateway, apiError{Error: "check_failed", Message: "I couldn't check for updates: " + err.Error() + ".", Fix: "Check the internet connection, then try again."})
			return
		}
		byDir := map[string]*PackUpdate{}
		for i := range ups {
			byDir[ups[i].Dir] = &ups[i]
		}
		pk := s.packsWithCommits(r.Context())
		for i := range pk {
			pk[i].Update = byDir[pk[i].Dir]
		}
		if pk == nil {
			pk = []PackInfo{}
		}
		writeJSON(w, map[string]any{"packs": pk})
	}))
	mux.HandleFunc("POST /protocols/apply-update", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Dir string `json:"dir"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Dir == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		if err := u.ApplyPackUpdate(ctx, req.Dir); err != nil {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "not_updated", Message: "I couldn't update " + req.Dir + ": " + err.Error() + "."})
			return
		}
		writeJSON(w, map[string]string{"updated": req.Dir})
	}))
	mux.HandleFunc("POST /protocols/preview", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		pv, err := u.PreviewPack(ctx, req.Name)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "no_preview", Message: "I couldn't look inside that pack: " + err.Error() + "."})
			return
		}
		if pv.Protocols == nil {
			pv.Protocols = []ProtocolInfo{}
		}
		writeJSON(w, pv)
	}))
}
