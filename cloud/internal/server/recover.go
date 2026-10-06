package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

// whyRecovered is the deny-list reason for the keys a recovery moved the
// account away from. Relays refuse them like any denied key; the control
// plane answers them 409 superseded, so the old machine stands by rather
// than carrying on unlinked.
const whyRecovered = "recovered"

// recover answers POST /v1/recover {ns, device_pub, acme_account, ts,
// sig_recovery}, signed by the new machine's device key: the owner typed
// their 12 words there, and the recovery key they give signed this move.
// The account behind the namespace moves to the new key: the handle's
// generation goes up (so every other machine hears 409 superseded), every
// other device key goes on the deny list (so relays drop them within a
// poll), the handle's CAA record is rewritten for acme_account (pinning
// none until the new machine names its account, when that is empty), and
// the answer is an entitlement for the new key. Words that bound no
// namespace here, or a signature they didn't make, get 401.
func (s *Server) recover(w http.ResponseWriter, r *http.Request, keyID string, body []byte) {
	if s.storage == nil {
		fail(w, http.StatusNotFound, "not_found", "This server keeps no backups.")
		return
	}
	var in struct {
		NS          string    `json:"ns"`
		DevicePub   string    `json:"device_pub"`
		ACMEAccount string    `json:"acme_account"`
		TS          time.Time `json:"ts"`
		Sig         string    `json:"sig_recovery"`
	}
	if decodeStrict(body, &in) != nil {
		fail(w, http.StatusBadRequest, "bad_request", "The request body is not what recover takes.")
		return
	}
	pub, err := entitle.ParseKey(in.DevicePub)
	if err != nil || httpsig.KeyID(pub) != keyID {
		fail(w, http.StatusUnauthorized, "unauthorized", "device_pub must be the key that signed the request.")
		return
	}
	if in.ACMEAccount != "" {
		if err := s.checkACMEAccount(in.ACMEAccount); err != nil {
			fail(w, http.StatusBadRequest, "bad_acme_account", err.Error())
			return
		}
	}
	now := s.now().UTC().Truncate(time.Second)
	if d := now.Sub(in.TS); d > signingSkew || d < -signingSkew {
		fail(w, http.StatusUnauthorized, "unauthorized", "The recovery was signed too long ago; check this computer's clock.")
		return
	}
	wrongWords := func() {
		fail(w, http.StatusUnauthorized, "unauthorized", "Those words don't match any backups kept here.")
	}
	var ns store.Namespace
	err = s.store.View(r.Context(), func(tx *store.Tx) error {
		var err error
		ns, err = tx.Namespace(in.NS)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		wrongWords()
		return
	}
	if err != nil {
		internal(w, s, err)
		return
	}
	rpub, err := entitle.ParseKey(ns.RecoveryPub)
	if err != nil || !verifyRecovery(rpub, in.Sig, entitle.RecoverMessage(in.NS, in.DevicePub, in.ACMEAccount, in.TS)) {
		wrongWords()
		return
	}

	var code string
	var handle store.Handle
	var ans answer
	err = s.store.Update(r.Context(), func(tx *store.Tx) error {
		ns, err := tx.Namespace(in.NS)
		if err != nil {
			return err
		}
		a, err := tx.Account(ns.Account)
		if err != nil {
			return err
		}
		if !a.DeleteAt.IsZero() && !now.Before(a.DeleteAt) {
			code = "deleted"
			return nil
		}
		h, err := tx.AccountHandle(a.ID)
		if err != nil {
			return err
		}
		if _, err := tx.DeniedKey(keyID); err == nil {
			code = "revoked"
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		d, err := tx.DeviceByKeyID(keyID)
		switch {
		case err == nil && (d.Account != a.ID || d.Revoked || d.Gen != h.Gen):
			code = "already_linked"
			return nil
		case err == nil:
			// The same recovery again (its answer was lost): nothing moves.
		case errors.Is(err, store.ErrNotFound):
			h.Gen++
			h.GenAt = now
			devs, err := tx.Devices(a.ID)
			if err != nil {
				return err
			}
			for _, old := range devs {
				if old.Revoked {
					continue
				}
				if _, err := tx.Deny(store.DenyEntry{Kind: store.DenyKey, Value: old.Pub, KeyID: old.KeyID, Why: whyRecovered, Created: now}); err != nil {
					return err
				}
			}
			d = store.Device{Pub: in.DevicePub, KeyID: keyID, Account: a.ID, Gen: h.Gen, Created: now, LastSeenDay: now.Format(time.DateOnly)}
			if err := tx.InsertDevice(d); err != nil {
				return err
			}
			a.ACMEAccount = in.ACMEAccount
			if err := tx.UpdateAccount(a); err != nil {
				return err
			}
			h.DNSPending = true
			if err := tx.UpdateHandle(h); err != nil {
				return err
			}
			if err := tx.AddEvent(a.ID, "recovered", now); err != nil {
				return err
			}
		default:
			return err
		}
		handle = h
		if !now.Before(a.PaidThrough) {
			// Lapsed: no entitlement, but the new machine can still fetch its
			// backups for 90 days.
			ans = answer{http.StatusOK, map[string]any{"paid_through": a.PaidThrough.Format(time.RFC3339)}}
			return nil
		}
		ans, err = s.issue(tx, d, now)
		return err
	})
	switch {
	case err != nil:
		internal(w, s, err)
		return
	case code == "already_linked":
		fail(w, http.StatusConflict, "already_linked", "This device is already linked.")
		return
	case code != "":
		refuse(w, code)
		return
	}
	if handle.DNSPending {
		if err := s.syncDNS(r.Context(), handle.Name); err != nil {
			s.log.Warn("recover: dns, the sweep retries", "err", err)
		}
	}
	if ans.status != http.StatusOK {
		reply(w, ans.status, ans.body)
		return
	}
	ans.body["handle"] = handle.Name
	ans.body["gen"] = handle.Gen
	reply(w, http.StatusOK, ans.body)
}
