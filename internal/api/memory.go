package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

//go:embed memory.html
var memoryHTML []byte

// Fact is a memory row for the page.
type Fact struct {
	ID        int64     `json:"id"`
	Subject   string    `json:"subject"`
	Content   string    `json:"content"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// AuditEntry is one audit line for the page.
type AuditEntry struct {
	TS      time.Time `json:"ts"`
	Kind    string    `json:"kind"`
	ChatKey string    `json:"chat_key"`
	Detail  string    `json:"detail"`
}

// MemoryBackend is what the memory page needs.
type MemoryBackend interface {
	Facts(ctx context.Context) ([]Fact, error)
	AddFact(ctx context.Context, subject, content string) (int64, error)
	DeleteFact(ctx context.Context, id int64) error
	Audit(ctx context.Context, n int) ([]AuditEntry, error)
	// Portrait is the twin's portrait of its owner. aside is true when there
	// is none because the owner asked the twin to forget something.
	Portrait(ctx context.Context) (text string, updated time.Time, aside bool, err error)
	RefreshPortrait(ctx context.Context) (string, error)
	Twin(ctx context.Context) TwinInfo
}

// TwinInfo is who the twin is, for the identity page.
type TwinInfo struct {
	Name      string   `json:"name"`
	Spoken    string   `json:"spoken"`
	PersonaID string   `json:"persona_id"`
	Persona   string   `json:"persona"`
	Tagline   string   `json:"tagline"`
	Character string   `json:"character"`
	Style     []string `json:"style"`
	Voice     string   `json:"voice"`
	WakeWord  string   `json:"wake_word"`
	User      string   `json:"user"`
}

// memoryRoutes serves the memory page and its JSON. They open on this
// computer only: memory is everything the twin knows about its owner.
func (s *Server) memoryRoutes(mux *http.ServeMux, mem MemoryBackend, a authz) {
	mux.HandleFunc("GET /memory", s.page(a, devices.Admin, memoryHTML, nil))
	mux.HandleFunc("GET /memory/facts", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Has("offset") || q.Has("limit") || q.Has("q") {
			factsPage(w, r, mem) // memory_page.go
			return
		}
		facts, err := mem.Facts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if facts == nil {
			facts = []Fact{}
		}
		writeJSON(w, facts)
	}))
	mux.HandleFunc("POST /memory/facts", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Subject, Content string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Content == "" {
			http.Error(w, "bad request", 400)
			return
		}
		id, err := mem.AddFact(r.Context(), in.Subject, in.Content)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]int64{"id": id})
	}))
	mux.HandleFunc("DELETE /memory/facts/{id}", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", 400)
			return
		}
		if err := mem.DeleteFact(r.Context(), id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("GET /memory/twin", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, mem.Twin(r.Context()))
	}))
	mux.HandleFunc("GET /memory/portrait", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		text, updated, aside, err := mem.Portrait(r.Context())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"text": text, "updated_at": updated, "aside": aside})
	}))
	mux.HandleFunc("POST /memory/portrait", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		text, err := mem.RefreshPortrait(ctx)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"text": text, "updated_at": time.Now()})
	}))
	mux.HandleFunc("GET /memory/audit", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		es, err := mem.Audit(r.Context(), 100)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if es == nil {
			es = []AuditEntry{}
		}
		writeJSON(w, es)
	}))
	s.whyRoutes(mux, mem, a) // why.go
}
