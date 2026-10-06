package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

// denyCacheFor is how long one signed deny list is served: relays poll
// every 45 s, and a new entry changes the seq, which re-signs at once.
const denyCacheFor = 5 * time.Second

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, map[string]string{"api": "v1", "server": "mirrin-cloud " + s.ver})
}

// publicKeys answers GET /v1/keys: every published key, by purpose.
func (s *Server) publicKeys(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, map[string]any{"entitlement": publicKeys(s.keys.EntPublic), "denylist": publicKeys(s.keys.DLPublic)})
}

// denylist answers GET /v1/denylist: the signed list, then a newline.
func (s *Server) denylist(w http.ResponseWriter, r *http.Request) {
	tok, err := s.denyToken(r.Context())
	if err != nil {
		internal(w, s, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, tok+"\n")
}

// denyToken signs the list in force, reusing a token of the same seq for a
// few seconds.
func (s *Server) denyToken(ctx context.Context) (string, error) {
	var seq int64
	var entries []store.DenyEntry
	if err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		seq, entries, err = tx.DenyList()
		return err
	}); err != nil {
		return "", err
	}
	now := s.now()
	c := &s.denyCache
	c.Lock()
	defer c.Unlock()
	if c.tok != "" && c.seq == seq && now.Sub(c.at) < denyCacheFor && !now.Before(c.at) {
		return c.tok, nil
	}
	l := entitle.DenyList{Seq: seq, Iat: now}
	for _, e := range entries {
		switch e.Kind {
		case store.DenyHandle:
			l.Handles = append(l.Handles, entitle.Entry{Value: e.Value, Why: e.Why})
		case store.DenyKey:
			l.Keys = append(l.Keys, entitle.Entry{Value: e.Value, Why: e.Why})
		}
	}
	tok, err := entitle.SignDenyList(l, s.keys.DLKid, s.keys.DL)
	if err != nil {
		return "", err
	}
	c.tok, c.seq, c.at = tok, seq, now
	return tok, nil
}

// DenyHandle puts a handle on the deny list, as an admin suspending it
// would: relays refuse it within a minute and no new entitlement is signed
// for it.
func DenyHandle(ctx context.Context, st *store.Store, handle, why string, now time.Time) error {
	if err := checkHandle(handle); err != nil {
		return fmt.Errorf("%q: %w", handle, err)
	}
	return st.Update(ctx, func(tx *store.Tx) error {
		h, err := tx.Handle(handle)
		if err != nil {
			return fmt.Errorf("no handle %q: %w", handle, err)
		}
		if _, err := tx.Deny(store.DenyEntry{Kind: store.DenyHandle, Value: h.Name, Why: why, Created: now}); err != nil {
			return err
		}
		if h.Account != "" {
			return tx.AddEvent(h.Account, "handle_denied", now)
		}
		return nil
	})
}

// DenyKey revokes a device key and puts it on the deny list. A key the
// control plane never saw is listed too, so relays refuse it.
func DenyKey(ctx context.Context, st *store.Store, pub, why string, now time.Time) error {
	k, err := entitle.ParseKey(pub)
	if err != nil {
		return err
	}
	keyID := httpsig.KeyID(k)
	return st.Update(ctx, func(tx *store.Tx) error {
		d, err := tx.DeviceByKeyID(keyID)
		switch {
		case err == nil:
			return revokeDevice(tx, d, why, now)
		case errors.Is(err, store.ErrNotFound):
			_, err = tx.Deny(store.DenyEntry{Kind: store.DenyKey, Value: pub, KeyID: keyID, Why: why, Created: now})
			return err
		default:
			return err
		}
	})
}
