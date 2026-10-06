package server

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

// devRoutes are the routes of --dev mode (docs/cloud-api.md §7): the fake
// merchant of record's pages, and controls standing in for its webhooks, a
// recovery on another machine and an admin. They take no signature, which
// is why a production server never has them.
func (s *Server) devRoutes(handle func(method, path string, h http.HandlerFunc)) {
	handle("GET", "/checkout/{id}", s.devCheckout)
	handle("GET", "/portal/{customer}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "Fake billing portal (mirrin-cloud --dev).\n")
	})
	handle("POST", "/dev/paid-through", s.devPaidThrough)
	handle("POST", "/dev/supersede", s.devSupersede)
	handle("POST", "/dev/revoke", s.devRevoke)
}

// devCheckout pays for a link: the fake merchant of record signs the
// webhook a real checkout would bring, and it goes through the server's own
// handler for POST /v1/billing/webhook.
func (s *Server) devCheckout(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var l store.Link
	err := s.store.View(r.Context(), func(tx *store.Tx) error {
		var err error
		l, err = tx.Link(id)
		return err
	})
	if err != nil || s.devBilling == nil {
		fail(w, http.StatusNotFound, "not_found", "No such checkout.")
		return
	}
	if l.Status == linkPending {
		req, err := s.devBilling.PaymentWebhook(r.Context(), s.origin+"/v1/billing/webhook", id)
		if err != nil {
			internal(w, s, err)
			return
		}
		rec := &discard{h: http.Header{}}
		s.handler.ServeHTTP(rec, req)
		if rec.status != http.StatusOK {
			fail(w, http.StatusBadGateway, "busy", "The fake webhook was refused.")
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "Paid (mirrin-cloud --dev fake checkout). You can close this tab.\n")
}

// discard is a ResponseWriter that keeps only the status.
type discard struct {
	h      http.Header
	status int
}

func (d *discard) Header() http.Header { return d.h }
func (d *discard) Write(b []byte) (int, error) {
	if d.status == 0 {
		d.status = http.StatusOK
	}
	return len(b), nil
}
func (d *discard) WriteHeader(code int) {
	if d.status == 0 {
		d.status = code
	}
}

// devRequest reads a dev route's JSON body strictly into in.
func devRequest(w http.ResponseWriter, r *http.Request, in any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err == nil {
		err = decodeStrict(body, in)
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "The request body is not what this route takes.")
		return false
	}
	return true
}

var errNoSuch = errors.New("no such handle or device")

// devPaidThrough moves the end of a handle's paid period, as a billing
// event would; a time in the past lapses it.
func (s *Server) devPaidThrough(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Handle      string    `json:"handle"`
		PaidThrough time.Time `json:"paid_through"`
	}
	if !devRequest(w, r, &in) {
		return
	}
	now := s.now().UTC()
	err := s.store.Update(r.Context(), func(tx *store.Tx) error {
		h, err := tx.Handle(in.Handle)
		if err != nil || h.Account == "" {
			return errNoSuch
		}
		a, err := tx.Account(h.Account)
		if err != nil {
			return err
		}
		a.PaidThrough = in.PaidThrough.UTC().Truncate(time.Second)
		if err := tx.UpdateAccount(a); err != nil {
			return err
		}
		return tx.AddEvent(a.ID, "billing", now)
	})
	devAnswer(w, s, err, http.StatusNoContent, nil)
}

// devSupersede raises a handle's generation, as a recovery on another
// machine would.
func (s *Server) devSupersede(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Handle string `json:"handle"`
	}
	if !devRequest(w, r, &in) {
		return
	}
	now := s.now().UTC().Truncate(time.Second)
	var gen int64
	err := s.store.Update(r.Context(), func(tx *store.Tx) error {
		h, err := tx.Handle(in.Handle)
		if err != nil || h.Account == "" {
			return errNoSuch
		}
		h.Gen++
		h.GenAt = now
		gen = h.Gen
		if err := tx.UpdateHandle(h); err != nil {
			return err
		}
		return tx.AddEvent(h.Account, "superseded", now)
	})
	devAnswer(w, s, err, http.StatusOK, map[string]int64{"gen": gen})
}

// devRevoke revokes a device key and deny-lists it, as an admin would.
func (s *Server) devRevoke(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DevicePub string `json:"device_pub"`
	}
	if !devRequest(w, r, &in) {
		return
	}
	pub, err := entitle.ParseKey(strings.TrimSpace(in.DevicePub))
	if err != nil {
		fail(w, http.StatusNotFound, "not_found", "No such device.")
		return
	}
	err = s.store.Update(r.Context(), func(tx *store.Tx) error {
		d, err := tx.DeviceByKeyID(httpsig.KeyID(pub))
		if err != nil {
			return errNoSuch
		}
		return revokeDevice(tx, d, "revoked", s.now().UTC())
	})
	devAnswer(w, s, err, http.StatusNoContent, nil)
}

func devAnswer(w http.ResponseWriter, s *Server, err error, status int, body any) {
	switch {
	case errors.Is(err, errNoSuch):
		fail(w, http.StatusNotFound, "not_found", "No such handle or device.")
	case err != nil:
		internal(w, s, err)
	case body == nil:
		w.WriteHeader(status)
	default:
		reply(w, status, body)
	}
}
