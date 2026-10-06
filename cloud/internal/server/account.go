package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
)

// acmeAccount answers PUT /v1/acme-account {uri}: the handle's CAA record
// will name this ACME account. The records are rewritten before the answer,
// unless they already name it; if the DNS provider fails, the answer is 503
// and the sweep keeps trying.
func (s *Server) acmeAccount(w http.ResponseWriter, r *http.Request, d store.Device, body []byte) {
	var in struct {
		URI string `json:"uri"`
	}
	if decodeStrict(body, &in) != nil {
		fail(w, http.StatusBadRequest, "bad_acme_account", "The body must be {\"uri\": \"https://…\"}.")
		return
	}
	if err := s.checkACMEAccount(in.URI); err != nil {
		fail(w, http.StatusBadRequest, "bad_acme_account", err.Error())
		return
	}
	now := s.now().UTC()
	var handle string
	changed := false
	err := s.store.Update(r.Context(), func(tx *store.Tx) error {
		a, err := tx.Account(d.Account)
		if err != nil {
			return err
		}
		h, err := tx.AccountHandle(a.ID)
		if err != nil {
			return err
		}
		handle = h.Name
		if a.ACMEAccount == in.URI && !h.DNSPending {
			return nil // the records already say so
		}
		changed = true
		a.ACMEAccount = in.URI
		h.DNSPending = true
		if err := tx.UpdateAccount(a); err != nil {
			return err
		}
		if err := tx.UpdateHandle(h); err != nil {
			return err
		}
		return tx.AddEvent(a.ID, "acme_account", now)
	})
	if err == nil && changed {
		err = s.syncDNS(r.Context(), handle)
	}
	if err != nil {
		internal(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// portal answers POST /v1/billing/portal with the merchant of record's page
// for this account.
func (s *Server) portal(w http.ResponseWriter, r *http.Request, d store.Device, _ []byte) {
	var customer string
	err := s.store.View(r.Context(), func(tx *store.Tx) error {
		a, err := tx.Account(d.Account)
		customer = a.BillingCustomer
		return err
	})
	var u string
	if err == nil {
		u, err = s.billing.Portal(r.Context(), customer)
	}
	if err == nil {
		err = s.checkBrowserURL(u)
	}
	if err != nil {
		internal(w, s, err)
		return
	}
	reply(w, http.StatusOK, map[string]string{"url": u})
}

// unlink answers DELETE /v1/device: the calling key is revoked and denied;
// the account, handle and subscription stay.
func (s *Server) unlink(w http.ResponseWriter, r *http.Request, d store.Device, _ []byte) {
	err := s.store.Update(r.Context(), func(tx *store.Tx) error {
		return revokeDevice(tx, d, "unlinked", s.now().UTC())
	})
	if err != nil {
		internal(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokeDevice refuses a device from now on and puts its key on the deny
// list, so relays drop it before its entitlement expires.
func revokeDevice(tx *store.Tx, d store.Device, why string, now time.Time) error {
	if !d.Revoked {
		d.Revoked = true
		if err := tx.UpdateDevice(d); err != nil {
			return err
		}
		if err := tx.AddEvent(d.Account, "device_"+why, now); err != nil {
			return err
		}
	}
	_, err := tx.Deny(store.DenyEntry{Kind: store.DenyKey, Value: d.Pub, KeyID: d.KeyID, Why: why, Created: now})
	return err
}

// deleteAccount answers DELETE /v1/account: deletion in seven days, which
// POST /v1/account/restore undoes until then.
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, d store.Device, _ []byte) {
	now := s.now().UTC().Truncate(time.Second)
	var at time.Time
	err := s.store.Update(r.Context(), func(tx *store.Tx) error {
		a, err := tx.Account(d.Account)
		if err != nil {
			return err
		}
		if a.DeleteAt.IsZero() {
			a.DeleteAt = now.Add(deleteWait)
			if err := tx.UpdateAccount(a); err != nil {
				return err
			}
			if err := tx.AddEvent(a.ID, "delete_requested", now); err != nil {
				return err
			}
		}
		at = a.DeleteAt
		return nil
	})
	if err != nil {
		internal(w, s, err)
		return
	}
	reply(w, http.StatusAccepted, map[string]string{"delete_at": at.Format(time.RFC3339)})
}

var errNotDeleting = errors.New("not deleting")

// restoreAccount answers POST /v1/account/restore.
func (s *Server) restoreAccount(w http.ResponseWriter, r *http.Request, d store.Device, _ []byte) {
	now := s.now().UTC()
	err := s.store.Update(r.Context(), func(tx *store.Tx) error {
		a, err := tx.Account(d.Account)
		if err != nil {
			return err
		}
		if a.DeleteAt.IsZero() {
			return errNotDeleting
		}
		a.DeleteAt = time.Time{}
		if err := tx.UpdateAccount(a); err != nil {
			return err
		}
		return tx.AddEvent(a.ID, "delete_cancelled", now)
	})
	switch {
	case errors.Is(err, errNotDeleting):
		fail(w, http.StatusConflict, "not_deleting", "This account is not being deleted.")
	case err != nil:
		internal(w, s, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
