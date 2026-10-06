package server

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

// Link statuses.
const (
	linkPending = "pending"
	linkActive  = "active"
	linkExpired = "expired"
	linkRefused = "refused" // paid, but attached to nothing: cancelled and refunded
)

// refusedMessage is what a machine whose payment was refused is told.
const refusedMessage = "This payment was not attached to this machine: it came from a customer who already has an account, " +
	"or for a key that may not link. It has been cancelled and refunded. To move an account here, contact support; " +
	"mirrin cloud unlink --local clears this checkout."

// linkStart begins a checkout for a device key the signature proves the
// daemon holds: POST /v1/link/start {device_pub, handle?, acme_account?}.
func (s *Server) linkStart(w http.ResponseWriter, r *http.Request, keyID string, body []byte) {
	var in struct {
		DevicePub   string `json:"device_pub"`
		Handle      string `json:"handle"`
		ACMEAccount string `json:"acme_account"`
	}
	if decodeStrict(body, &in) != nil {
		fail(w, http.StatusBadRequest, "bad_request", "The request body is not what link/start takes.")
		return
	}
	pub, err := entitle.ParseKey(in.DevicePub)
	if err != nil || httpsig.KeyID(pub) != keyID {
		fail(w, http.StatusUnauthorized, "unauthorized", "device_pub must be the key that signed the request.")
		return
	}
	if in.Handle != "" {
		if err := allowedHandle(in.Handle); err != nil {
			fail(w, http.StatusBadRequest, "bad_handle", err.Error())
			return
		}
	}
	if in.ACMEAccount != "" {
		if err := s.checkACMEAccount(in.ACMEAccount); err != nil {
			fail(w, http.StatusBadRequest, "bad_acme_account", err.Error())
			return
		}
	}
	now := s.now().UTC().Truncate(time.Second)
	// Counted once the request is signed and well formed, so nobody spends
	// another client's allowance, then in all, which also bounds the
	// checkouts opened at the merchant of record.
	if !s.linkStarts.allow(clientPrefix(s.clientAddr(r)), now) {
		tooMany(w, 60, "Too many link attempts from here; try again in a minute.")
		return
	}
	if !s.linkStartsTotal.allow(struct{}{}, now) {
		tooMany(w, 60, "Many machines are linking right now; try again in a minute.")
		return
	}
	l := store.Link{ID: "lk_" + randomID(), DevicePub: in.DevicePub, KeyID: keyID, Handle: in.Handle, ACMEAccount: in.ACMEAccount,
		Status: linkPending, Expires: now.Add(linkTTL), Created: now}
	var refusal string
	err = s.store.Update(r.Context(), func(tx *store.Tx) error {
		code, err := s.refusal(tx, keyID, now)
		switch {
		case err != nil:
			return err
		case code == "":
			refusal = "already_linked"
			return nil
		case code != "unauthorized":
			refusal = code
			return nil
		}
		if in.Handle != "" {
			if taken, err := s.handleTaken(tx, in.Handle, keyID, now); err != nil || taken {
				refusal = "handle_taken"
				return err
			}
		}
		if full, err := s.activationsFull(tx, now); err != nil || full {
			refusal = "full"
			return err
		}
		return tx.InsertLink(l)
	})
	switch {
	case err != nil:
		internal(w, s, err)
		return
	case refusal == "already_linked":
		fail(w, http.StatusConflict, "already_linked", "This device is already linked.")
		return
	case refusal == "handle_taken":
		fail(w, http.StatusConflict, "handle_taken", "That handle, or one that looks like it, is taken. Handles are never reassigned.")
		return
	case refusal == "full":
		w.Header().Set("Retry-After", "86400")
		reply(w, http.StatusTooManyRequests, map[string]any{"error": "rate_limited", "message": "New sign-ups are full this week. Please try again tomorrow.", "retry_after": 86400})
		return
	case refusal != "":
		refuse(w, refusal)
		return
	}
	checkout, err := s.billing.Checkout(r.Context(), l.ID, s.cfg.Plan)
	if err == nil {
		err = s.checkBrowserURL(checkout)
	}
	if err != nil {
		s.store.Update(context.WithoutCancel(r.Context()), func(tx *store.Tx) error { return tx.DeleteLink(l.ID) })
		internal(w, s, err)
		return
	}
	reply(w, http.StatusOK, map[string]string{"id": l.ID, "checkout_url": checkout})
}

// activationsFull reports whether the week's handles, with the checkouts
// that may still become one, have reached activations_per_week.
func (s *Server) activationsFull(tx *store.Tx, now time.Time) (bool, error) {
	if s.cfg.ActivationsPerWeek <= 0 {
		return false, nil
	}
	made, err := tx.CountHandlesSince(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		return false, err
	}
	pending, err := tx.CountPendingLinks(now)
	if err != nil {
		return false, err
	}
	return made+pending >= s.cfg.ActivationsPerWeek, nil
}

// handleTaken reports whether name, or a handle that looks like it, is held
// (for good: handles are never reassigned) or asked for by another key's
// pending link.
func (s *Server) handleTaken(tx *store.Tx, name, keyID string, now time.Time) (bool, error) {
	sk := skeleton(name)
	if _, err := tx.HandleBySkeleton(sk); err == nil {
		return true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if _, err := tx.Handle(name); err == nil {
		return true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	ls, err := tx.PendingLinks(now)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(ls, func(l store.Link) bool {
		return l.Handle != "" && l.KeyID != keyID && skeleton(l.Handle) == sk
	}), nil
}

// linkPoll answers GET /v1/link/{id}, for the key that started it only.
func (s *Server) linkPoll(w http.ResponseWriter, r *http.Request, keyID string, _ []byte) {
	now := s.now().UTC()
	var status, code string
	var ans answer
	err := s.store.Update(r.Context(), func(tx *store.Tx) error {
		l, err := tx.Link(r.PathValue("id"))
		if errors.Is(err, store.ErrNotFound) || err == nil && l.KeyID != keyID {
			code = "not_found"
			return nil
		}
		if err != nil {
			return err
		}
		if l.Status == linkPending && !now.Before(l.Expires) {
			l.Status = linkExpired
			if err := tx.UpdateLink(l); err != nil {
				return err
			}
		}
		if l.Status == linkRefused {
			code = "payment_refused"
			return nil
		}
		if status = l.Status; status != linkActive {
			return nil
		}
		if code, err = s.refusal(tx, keyID, now); err != nil || code != "" {
			return err
		}
		d, err := tx.DeviceByKeyID(keyID)
		if err != nil {
			return err
		}
		ans, err = s.issue(tx, d, now)
		return err
	})
	switch {
	case err != nil:
		internal(w, s, err)
	case code == "not_found" || code == "unauthorized":
		fail(w, http.StatusNotFound, "not_found", "No such link.")
	case code == "payment_refused":
		fail(w, http.StatusConflict, "payment_refused", refusedMessage)
	case code != "":
		refuse(w, code)
	case status != linkActive:
		reply(w, http.StatusOK, map[string]string{"status": status})
	default:
		if ans.status == http.StatusOK {
			ans.body["status"] = linkActive
		}
		reply(w, ans.status, ans.body)
	}
}

// pickHandle chooses a free random handle.
func (s *Server) pickHandle(tx *store.Tx, now time.Time) (string, error) {
	for range 100 {
		h := randomHandle()
		if allowedHandle(h) != nil {
			continue
		}
		if taken, err := s.handleTaken(tx, h, "", now); err != nil {
			return "", err
		} else if !taken {
			return h, nil
		}
	}
	return "", errors.New("no free random handle in 100 tries")
}

// checkACMEAccount applies the rules of docs/cloud-api.md §5.2: an https
// URL of at most 512 bytes with no user info, query or fragment, and none of
// the characters CAA or DNS text would need escaped; its host must be one
// of acme_account_hosts when that is set.
func (s *Server) checkACMEAccount(uri string) error {
	bad := errors.New("acme_account must be an https ACME account URL fit for a CAA record.")
	if uri == "" || len(uri) > 512 || strings.ContainsAny(uri, "\";\\") {
		return bad
	}
	for i := 0; i < len(uri); i++ {
		if uri[i] < 0x21 || uri[i] > 0x7e {
			return bad
		}
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || strings.Contains(uri, "#") || u.Opaque != "" {
		return bad
	}
	if len(s.cfg.ACMEAccountHosts) > 0 && !slices.Contains(s.cfg.ACMEAccountHosts, u.Host) {
		return fmt.Errorf("acme_account must be an account at %s.", strings.Join(s.cfg.ACMEAccountHosts, " or "))
	}
	return nil
}

// checkBrowserURL checks a page the daemon will open: https, or http from a
// loopback dev server.
func (s *Server) checkBrowserURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" || p.User != nil || len(u) > 2048 ||
		strings.ContainsFunc(u, func(r rune) bool { return r < 0x21 || r > 0x7e }) ||
		!(p.Scheme == "https" || s.dev && p.Scheme == "http" && isLoopback(p.Hostname())) {
		return fmt.Errorf("the merchant of record sent a page that is not a web address: %q", u)
	}
	return nil
}

// randomID is 16 characters of lowercase base32 (80 bits).
func randomID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
