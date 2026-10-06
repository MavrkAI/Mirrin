package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// The Backup page: where encrypted backups go, when the last one was, and
// the Recovery Kit. The 12 words are made here, shown once (the answer to
// POST /backup/kit) and kept only in memory until the owner types word 7
// back (POST /backup/kit/finish): then only their public keys are saved and
// the words are forgotten. A page reloaded before that starts again with
// new words.

// BackupWhere is a destination the page chose. An S3 key pair given with it
// is saved in secrets.env, never in config.yaml, and never sent back.
type BackupWhere struct {
	Target    string `json:"target"` // icloud, folder or s3
	Path      string `json:"path,omitempty"`
	S3URL     string `json:"s3_url,omitempty"` // s3://bucket/folder
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
}

// BackupStatus is what the Backup page shows.
type BackupStatus struct {
	On     bool   `json:"on"`
	Target string `json:"target,omitempty"` // icloud, folder or s3
	Where  string `json:"where,omitempty"`  // in the owner's words
	Path   string `json:"path,omitempty"`
	S3URL  string `json:"s3_url,omitempty"`
	// Endpoint and Region are the S3 store's; KeySaved says whether its
	// key pair can be found (the key itself is never shown).
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`
	KeySaved bool   `json:"key_saved,omitempty"`
	KitID    string `json:"kit_id,omitempty"`

	LastGood    time.Time `json:"last_good,omitzero"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	Seq         int64     `json:"seq,omitempty"`
	Size        int64     `json:"size,omitempty"`
	LeftOut     []string  `json:"left_out,omitempty"`
	Running     bool      `json:"running,omitempty"`
	// Standby names the machine the twin moved to, when this one stands by.
	Standby string `json:"standby,omitempty"`
	// Health is the self-check's verdict: ok, warn, fail or off.
	Health string `json:"health"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
	// ICloud says whether iCloud Drive is there to back up to, and if not
	// why (ICloudNote).
	ICloud     bool   `json:"icloud"`
	ICloudNote string `json:"icloud_note,omitempty"`
}

// BackupBackend runs backups for the page.
type BackupBackend interface {
	BackupStatus(ctx context.Context) BackupStatus
	// BackupCheck checks a destination before any words are made (a folder
	// outside the twin's own; an S3 bucket that answers with its key,
	// saving a key given with it) and returns the settings for it.
	BackupCheck(ctx context.Context, w BackupWhere) (config.Backup, error)
	// BackupSetup turns backups on with p's public keys, to where, and
	// starts the first backup.
	BackupSetup(ctx context.Context, p backup.Phrase, where config.Backup) error
	// BackupMove changes where backups go, keeping the words.
	BackupMove(ctx context.Context, where config.Backup) error
	// BackupNow starts a backup now, in the background.
	BackupNow(ctx context.Context) error
}

// RestoreReview is the state of the device review after a restore.
type RestoreReview struct {
	// Pending: this machine was restored and the owner hasn't reviewed its
	// devices yet.
	Pending    bool      `json:"pending"`
	RestoredAt time.Time `json:"restored_at,omitzero"`
	Checklist  []string  `json:"checklist,omitempty"`
}

// ReviewBackend keeps the device review's state.
type ReviewBackend interface {
	RestoreReview(ctx context.Context) RestoreReview
	FinishRestoreReview(ctx context.Context) error
}

// kitTTL is how long words wait for their check before the page must start
// again.
const kitTTL = 30 * time.Minute

// kitTries is how many wrong check words a kit takes.
const kitTries = 5

type kit struct {
	phrase  backup.Phrase
	where   config.Backup
	expires time.Time
	tries   int
}

// kitHub holds Recovery Kits shown and not yet checked.
type kitHub struct {
	mu   sync.Mutex
	kits map[string]*kit
	now  func() time.Time
}

func (h *kitHub) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (h *kitHub) add(k *kit) string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	id := "kit_" + base64.RawURLEncoding.EncodeToString(b)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.kits == nil {
		h.kits = map[string]*kit{}
	}
	now := h.clock()
	for id, old := range h.kits {
		if now.After(old.expires) {
			delete(h.kits, id)
		}
	}
	k.expires = now.Add(kitTTL)
	h.kits[id] = k
	return id
}

func (h *kitHub) get(id string) *kit {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := h.kits[id]
	if k != nil && h.clock().After(k.expires) {
		delete(h.kits, id)
		return nil
	}
	return k
}

func (h *kitHub) drop(id string) {
	h.mu.Lock()
	delete(h.kits, id)
	h.mu.Unlock()
}

// plainSentence makes an error a sentence a page can show.
func plainSentence(err error) string {
	s := strings.TrimSpace(err.Error())
	if s == "" {
		return "That didn't work."
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "?") && !strings.HasSuffix(s, "!") {
		s += "."
	}
	return s
}

func (s *Server) backupRoutes(mux *http.ServeMux, a Authz) {
	b := s.pages
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(v) != nil {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_request", Message: "I couldn't read that.", Fix: "Reload the page and try again."})
			return false
		}
		return true
	}
	refuse := func(w http.ResponseWriter, r *http.Request, err error) {
		var h *HumanError
		if e, ok := err.(*HumanError); ok {
			h = e
		}
		if h != nil {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "backup", Message: h.Sentence, Fix: h.Fix})
			return
		}
		s.fail(w, r, http.StatusBadRequest, apiError{Error: "backup", Message: plainSentence(err)})
	}
	mux.HandleFunc("GET /backup/status", a.Local(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.BackupStatus(r.Context()))
	}))
	mux.HandleFunc("POST /backup/now", a.Local(func(w http.ResponseWriter, r *http.Request) {
		if err := b.BackupNow(r.Context()); err != nil {
			refuse(w, r, err)
			return
		}
		writeJSON(w, b.BackupStatus(r.Context()))
	}))
	mux.HandleFunc("POST /backup/target", a.Local(func(w http.ResponseWriter, r *http.Request) {
		var req BackupWhere
		if !decode(w, r, &req) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		where, err := b.BackupCheck(ctx, req)
		if err == nil {
			err = b.BackupMove(ctx, where)
		}
		if err != nil {
			refuse(w, r, err)
			return
		}
		writeJSON(w, b.BackupStatus(r.Context()))
	}))
	// The Recovery Kit: new words, shown in this one answer.
	mux.HandleFunc("POST /backup/kit", a.Local(func(w http.ResponseWriter, r *http.Request) {
		var req BackupWhere
		if !decode(w, r, &req) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		where, err := b.BackupCheck(ctx, req) // before the words: nobody writes down words for nothing
		if err != nil {
			refuse(w, r, err)
			return
		}
		p, err := backup.NewPhrase()
		if err != nil {
			refuse(w, r, err)
			return
		}
		id := s.kits.add(&kit{phrase: p, where: where})
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{
			"kit": id, "words": p.Words(), "kit_id": backup.KitID(p), "check": backup.CheckWord,
			"where": backup.Where(where), "twin": s.twinName(), "made": time.Now().Format("2 January 2006"),
			"replacing": b.BackupStatus(r.Context()).On,
		})
	}))
	mux.HandleFunc("POST /backup/kit/finish", a.Local(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kit  string `json:"kit"`
			Word string `json:"word"`
		}
		if !decode(w, r, &req) {
			return
		}
		k := s.kits.get(req.Kit)
		if k == nil {
			s.fail(w, r, http.StatusGone, apiError{Error: "kit_gone", Message: "These words were never saved: the page waited too long, or was reloaded.",
				Fix: "Start again to get new words. Cross out the old ones: they won't open anything."})
			return
		}
		if !backup.CheckTyped(k.phrase, backup.CheckWord, req.Word) {
			s.kits.mu.Lock()
			k.tries++
			burned := k.tries >= kitTries
			s.kits.mu.Unlock()
			if burned {
				s.kits.drop(req.Kit)
				s.fail(w, r, http.StatusGone, apiError{Error: "kit_gone", Message: "That was wrong too many times, so these words were thrown away and backups stay as they were.",
					Fix: "Start again with paper ready; you'll get new words."})
				return
			}
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "wrong_word", Message: fmt.Sprintf("That isn't word %d.", backup.CheckWord), Fix: fmt.Sprintf("Look at word %d on your paper and type it again.", backup.CheckWord)})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		if err := b.BackupSetup(ctx, k.phrase, k.where); err != nil {
			refuse(w, r, err)
			return
		}
		s.kits.drop(req.Kit) // the words are forgotten here
		writeJSON(w, b.BackupStatus(r.Context()))
	}))
	mux.HandleFunc("POST /backup/kit/cancel", a.Local(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kit string `json:"kit"`
		}
		if decode(w, r, &req) {
			s.kits.drop(req.Kit)
			writeJSON(w, map[string]bool{"ok": true})
		}
	}))
	mux.HandleFunc("GET /restore/review/state", a.Local(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.RestoreReview(r.Context()))
	}))
	mux.HandleFunc("POST /restore/review/done", a.Local(func(w http.ResponseWriter, r *http.Request) {
		if err := b.FinishRestoreReview(r.Context()); err != nil {
			refuse(w, r, err)
			return
		}
		writeJSON(w, b.RestoreReview(r.Context()))
	}))
}
