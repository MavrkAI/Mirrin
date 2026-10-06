package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// A known path with another method falls through to the catch-all
	// below: 404 not_found, as JSON like every other error.
	handle := func(method, path string, h http.HandlerFunc) { mux.HandleFunc(method+" "+path, h) }
	handle("GET", "/v1/version", s.version)
	handle("GET", "/v1/keys", s.publicKeys)
	handle("GET", "/v1/denylist", s.denylist)
	handle("POST", "/v1/link/start", s.limitPreAuth(s.signed(linkStartKeys, s.linkStart)))
	handle("GET", "/v1/link/{id}", s.limitPreAuth(s.signed(linkPollKeys, s.linkPoll)))
	handle("POST", "/v1/entitlement/refresh", s.signed(deviceKeys, s.device(s.refresh)))
	handle("PUT", "/v1/acme-account", s.signed(deviceKeys, s.holder(s.acmeAccount)))
	handle("GET", "/v1/me", s.signed(deviceKeys, s.holder(s.me)))
	handle("POST", "/v1/billing/portal", s.signed(deviceKeys, s.holder(s.portal)))
	handle("DELETE", "/v1/device", s.signed(deviceKeys, s.device(s.unlink)))
	handle("DELETE", "/v1/account", s.signed(deviceKeys, s.holder(s.deleteAccount)))
	handle("POST", "/v1/account/restore", s.signed(deviceKeys, s.holder(s.restoreAccount)))
	handle("POST", "/v1/billing/webhook", s.webhook)
	s.backupRoutes(handle) // backup.go
	if s.dev {
		s.devRoutes(handle)
		if s.devStorage != nil {
			mux.Handle(s.devStorage.Path(), s.devStorage)
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, "not_found", "No such route.")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rec := &statusRecorder{ResponseWriter: w}
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(rec, r)
		// The log line names the route, never the client: no address, no
		// user agent, no key.
		s.log.Info("request", "route", r.Pattern, "status", rec.status, "ms", s.now().Sub(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// signedHandler handles a request whose signature checked out: keyID
// signed it, and body is the whole body.
type signedHandler func(w http.ResponseWriter, r *http.Request, keyID string, body []byte)

// keyScope is where a signed route looks for the signing key. Every route
// looks among devices (revoked ones too, so they hear 403) and the deny list
// (so a deleted account's keys hear 403 deleted).
type keyScope int

const (
	deviceKeys    keyScope = iota // the device routes: nothing more
	linkPollKeys                  // GET /v1/link/{id}: pending links' keys too
	linkStartKeys                 // POST /v1/link/start: the device_pub the body names too
	recoverKeys                   // POST /v1/recover: likewise, a new machine's key
)

// nonceCache picks a request's nonce cache once its key is known.
type nonceCache func() httpsig.NonceCache

func (f nonceCache) Use(keyID, nonce string, now, until time.Time) error {
	return f().Use(keyID, nonce, now, until)
}

// signed reads the body (at most 64 KiB) and checks the RFC 9421 signature
// against the public origin, with 300 s of skew and a nonce cache: the
// devices' own for a device's key, a separate one for any other key (anyone
// can mint one), so strangers cannot crowd out registered devices.
func (s *Server) signed(scope keyScope, h signedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			fail(w, http.StatusRequestEntityTooLarge, "too_large", "The request body is over 64 KiB.")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var claimed ed25519.PublicKey
		if scope == linkStartKeys || scope == recoverKeys {
			var in struct {
				DevicePub string `json:"device_pub"`
			}
			if json.Unmarshal(body, &in) == nil {
				claimed, _ = entitle.ParseKey(in.DevicePub)
			}
		}
		isDevice := false
		lookup := func(keyID string) (ed25519.PublicKey, error) {
			pub, dev, err := s.lookupKey(r.Context(), keyID, scope == linkPollKeys)
			if errors.Is(err, store.ErrNotFound) && claimed != nil && keyID == httpsig.KeyID(claimed) {
				return claimed, nil
			}
			isDevice = dev
			return pub, err
		}
		cache := nonceCache(func() httpsig.NonceCache {
			if isDevice {
				return s.nonces
			}
			return s.otherNonces
		})
		keyID, err := httpsig.VerifyFor(r, []string{s.origin}, lookup, s.now(), signingSkew, cache)
		switch {
		case errors.Is(err, httpsig.ErrReplay):
			fail(w, http.StatusConflict, "replay", "This signed request was already used.")
		case errors.Is(err, httpsig.ErrNonceCacheFull):
			fail(w, http.StatusServiceUnavailable, "busy", "Try again shortly.")
		case err != nil:
			fail(w, http.StatusUnauthorized, "unauthorized", "The request signature was not accepted.")
		default:
			h(w, r, keyID, body)
		}
	}
}

// lookupKey finds the public key for a key id among devices (reporting
// that it is one), denied keys and, with links, the keys of links.
func (s *Server) lookupKey(ctx context.Context, keyID string, links bool) (ed25519.PublicKey, bool, error) {
	var pub string
	var device bool
	err := s.store.View(ctx, func(tx *store.Tx) error {
		if d, err := tx.DeviceByKeyID(keyID); err == nil {
			pub, device = d.Pub, true
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if e, err := tx.DeniedKey(keyID); err == nil {
			pub = e.Value
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if links {
			ls, err := tx.LinksByKeyID(keyID)
			if err != nil {
				return err
			}
			if len(ls) > 0 {
				pub = ls[0].DevicePub
				return nil
			}
		}
		return store.ErrNotFound
	})
	if err != nil {
		return nil, false, err
	}
	k, err := entitle.ParseKey(pub)
	return k, device, err
}

// deviceHandler handles a request from a device that may act.
type deviceHandler func(w http.ResponseWriter, r *http.Request, d store.Device, body []byte)

// device narrows a signed handler to a registered, unrevoked, undenied
// device of a live account, within its rate limit, and notes the day it was
// seen.
func (s *Server) device(h deviceHandler) signedHandler { return s.deviceRoute(false, h) }

// holder is device for the routes that read or change the account: the
// device must also hold its handle's current generation. A machine
// superseded by a recovery elsewhere, perhaps an old or stolen one, cannot
// rewrite the CAA record, read the account, open the billing portal, or
// delete or restore the account; it hears 409 superseded, as a refresh does.
func (s *Server) holder(h deviceHandler) signedHandler { return s.deviceRoute(true, h) }

func (s *Server) deviceRoute(current bool, h deviceHandler) signedHandler {
	return func(w http.ResponseWriter, r *http.Request, keyID string, body []byte) {
		now := s.now().UTC()
		if !s.perDevice.allow(keyID, now) {
			tooMany(w, s.perDevice.retryAfter(), "Too many requests from this machine; try again shortly.")
			return
		}
		var d store.Device
		var code string
		var sup *answer
		err := s.store.Update(r.Context(), func(tx *store.Tx) error {
			var err error
			code, err = s.refusal(tx, keyID, now)
			if err == nil && code == "revoked" {
				code, sup, err = s.recoveredElsewhere(tx, keyID, now)
			}
			if err != nil || code != "" || sup != nil {
				return err
			}
			if d, err = tx.DeviceByKeyID(keyID); err != nil {
				return err
			}
			if current {
				hd, err := tx.AccountHandle(d.Account)
				if err != nil {
					return err
				}
				if hd.Gen > d.Gen {
					a := superseded(hd)
					sup = &a
					return nil
				}
			}
			if day := now.Format(time.DateOnly); d.LastSeenDay != day {
				d.LastSeenDay = day
				return tx.UpdateDevice(d)
			}
			return nil
		})
		switch {
		case err != nil:
			internal(w, s, err)
		case code == "unauthorized":
			fail(w, http.StatusUnauthorized, "unauthorized", "This machine is not linked.")
		case code != "":
			refuse(w, code)
		case sup != nil:
			reply(w, sup.status, sup.body)
		default:
			h(w, r, d, body)
		}
	}
}

// refusal says why a key may not act, or "". A key on the deny list is
// refused first, whether or not it still has a device row (a recovery
// elsewhere deny-lists the old machine's keys): "deleted" if its account was
// deleted, otherwise "revoked". Then "unauthorized" for a key that is no
// device, "revoked" for a revoked device, and "deleted" once its account's
// deletion is due.
func (s *Server) refusal(tx *store.Tx, keyID string, now time.Time) (string, error) {
	if e, err := tx.DeniedKey(keyID); err == nil {
		if e.Why == "deleted" {
			return "deleted", nil
		}
		return "revoked", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	d, err := tx.DeviceByKeyID(keyID)
	if errors.Is(err, store.ErrNotFound) {
		return "unauthorized", nil
	}
	if err != nil {
		return "", err
	}
	if d.Revoked {
		return "revoked", nil
	}
	a, err := tx.Account(d.Account)
	if err != nil {
		return "", err
	}
	if !a.DeleteAt.IsZero() && !now.Before(a.DeleteAt) {
		return "deleted", nil
	}
	return "", nil
}

// recoveredElsewhere keeps "revoked" for a denied key unless a recovery
// on another machine denied it: its device row stands unrevoked, a
// generation behind its handle. Then every route, whether or not it asks
// for the current generation, answers what a superseded machine hears
// (409 superseded), which makes that machine stand by rather than carry on
// unlinked, and no handler runs for it. An account whose deletion is due
// is "deleted" first.
func (s *Server) recoveredElsewhere(tx *store.Tx, keyID string, now time.Time) (string, *answer, error) {
	d, err := tx.DeviceByKeyID(keyID)
	if errors.Is(err, store.ErrNotFound) {
		return "revoked", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	e, err := tx.DeniedKey(keyID)
	if err != nil {
		return "", nil, err
	}
	if d.Revoked || e.Why != whyRecovered {
		return "revoked", nil, nil
	}
	a, err := tx.Account(d.Account)
	if errors.Is(err, store.ErrNotFound) {
		return "revoked", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if !a.DeleteAt.IsZero() && !now.Before(a.DeleteAt) {
		return "deleted", nil, nil
	}
	h, err := tx.AccountHandle(d.Account)
	if errors.Is(err, store.ErrNotFound) {
		return "revoked", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if h.Gen > d.Gen {
		sup := superseded(h)
		return "", &sup, nil
	}
	return "revoked", nil, nil
}

// refuse answers a key that may not act.
func refuse(w http.ResponseWriter, code string) {
	if code == "deleted" {
		fail(w, http.StatusForbidden, "deleted", "This account was deleted.")
		return
	}
	fail(w, http.StatusForbidden, "revoked", "This machine's link was revoked.")
}

// reply writes v as JSON with status, map keys sorted.
func reply(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

// fail writes the documented error body.
func fail(w http.ResponseWriter, status int, code, message string) {
	reply(w, status, map[string]string{"error": code, "message": message})
}

// internal logs err and answers 503: every failure here is worth a retry.
func internal(w http.ResponseWriter, s *Server, err error) {
	s.log.Error("internal", "err", err)
	fail(w, http.StatusServiceUnavailable, "busy", "Try again shortly.")
}

// decodeStrict reads a request body: one JSON object, no unknown or
// duplicate members.
func decodeStrict(body []byte, v any) error {
	return json.Unmarshal(body, v, json.RejectUnknownMembers(true))
}
