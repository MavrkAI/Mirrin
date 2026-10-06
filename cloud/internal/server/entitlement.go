package server

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// answer is a status and a JSON body.
type answer struct {
	status int
	body   map[string]any
}

// refresh answers POST /v1/entitlement/refresh: a new entitlement, or why
// not (docs/cloud-api.md §5.3).
func (s *Server) refresh(w http.ResponseWriter, r *http.Request, d store.Device, _ []byte) {
	var ans answer
	err := s.store.View(r.Context(), func(tx *store.Tx) error {
		var err error
		ans, err = s.issue(tx, d, s.now().UTC())
		return err
	})
	if err != nil {
		internal(w, s, err)
		return
	}
	reply(w, ans.status, ans.body)
}

// issue signs d's entitlement as of now, unless its handle is denied (403),
// another machine holds the handle at a later generation (409 superseded),
// or payment has lapsed (402).
func (s *Server) issue(tx *store.Tx, d store.Device, now time.Time) (answer, error) {
	now = now.Truncate(time.Second)
	a, err := tx.Account(d.Account)
	if err != nil {
		return answer{}, err
	}
	h, err := tx.AccountHandle(a.ID)
	if err != nil {
		return answer{}, err
	}
	if _, err := tx.Denied(store.DenyHandle, h.Name); err == nil {
		return answer{http.StatusForbidden, map[string]any{"error": "revoked", "message": "This handle is suspended. Contact support."}}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return answer{}, err
	}
	switch {
	case h.Gen > d.Gen:
		return superseded(h), nil
	case !now.Before(a.PaidThrough):
		return answer{http.StatusPaymentRequired, map[string]any{"error": "lapsed", "message": "Payment has lapsed.",
			"paid_through": a.PaidThrough.UTC().Format(time.RFC3339)}}, nil
	}
	tok, err := entitle.Sign(s.claims(a, h, d, now), s.keys.EntKid, s.keys.Ent)
	if err != nil {
		return answer{}, err
	}
	return answer{http.StatusOK, map[string]any{"entitlement": tok}}, nil
}

// superseded is 409 superseded: another machine holds h at a later
// generation.
func superseded(h store.Handle) answer {
	return answer{http.StatusConflict, map[string]any{"error": "superseded", "message": "Another machine took over this handle.",
		"gen": h.Gen, "at": h.GenAt.UTC().Format(time.RFC3339)}}
}

// claims are what an entitlement for d says. It lasts until the earlier of
// 35 days from now and 7 days past paid_through, so the control plane can
// be down for 35 days before anyone notices, and a lapse ends service a
// week after the paid period.
func (s *Server) claims(a store.Account, h store.Handle, d store.Device, now time.Time) entitle.Claims {
	return entitle.Claims{
		Iss: s.iss, Sub: a.ID, Aud: entitle.Audience,
		Iat: now, Nbf: now, Exp: expiry(now, a.PaidThrough), PaidThrough: a.PaidThrough,
		Gen: h.Gen, Plan: a.Plan, Feat: slices.Clone(s.cfg.Features),
		Handle: h.Name, Hosts: []string{h.Name + "." + s.cfg.TenantZone}, Cnf: d.Pub,
		Relays: slices.Clone(s.relays), BackupQuota: s.cfg.BackupQuota,
	}
}

// expiry is exp = min(now+35d, paid_through+7d).
func expiry(now, paidThrough time.Time) time.Time {
	exp := now.Add(entMax)
	if g := paidThrough.Add(entGrace); g.Before(exp) {
		exp = g
	}
	return exp
}
