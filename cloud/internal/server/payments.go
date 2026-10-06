package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/billing"
	"github.com/MavrkAI/Mirrin/cloud/internal/store"
)

// webhook takes the merchant of record's events: POST /v1/billing/webhook.
// A repeated event is a no-op, and an event older than the last one applied
// changes nothing, so retries and reordering are harmless.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	ev, err := s.billing.ParseWebhook(r)
	switch {
	case errors.Is(err, billing.ErrSignature):
		fail(w, http.StatusUnauthorized, "unauthorized", "The webhook signature was not accepted.")
		return
	case err != nil:
		fail(w, http.StatusBadRequest, "bad_request", "The webhook does not parse.")
		return
	}
	if err := s.applyBilling(r.Context(), ev); err != nil {
		internal(w, s, err)
		return
	}
	reply(w, http.StatusOK, map[string]bool{"ok": true})
}

// refused is a payment the control plane will not attach to anything, and
// why. It is undone at the merchant of record (refusePayment): otherwise
// the customer would be billed every month for nothing.
type refused struct {
	why     string // for the log
	account string // the account to note it on, if any
}

// errRefused rolls back the transaction that found a refused payment: its
// event is recorded as applied only once the payment is undone.
var errRefused = errors.New("payment refused")

// applyBilling records one billing event. The first payment for a link
// makes its account, handle and device; later events move paid_through and
// the status, if they happened after the last one applied. A payment that
// attaches to nothing is cancelled and refunded.
func (s *Server) applyBilling(ctx context.Context, ev billing.Event) error {
	now := s.now().UTC().Truncate(time.Second)
	var activated string
	var ref refused
	err := s.store.Update(ctx, func(tx *store.Tx) error {
		seen, err := tx.SeenBillingEvent(ev.ID, ev.Occurred)
		if err != nil || seen || !ev.Relevant() {
			return err
		}
		a, err := tx.AccountByCustomer(ev.Customer)
		switch {
		case errors.Is(err, store.ErrNotFound):
			activated, ref, err = s.activate(tx, ev, now)
		case err == nil:
			ref, err = s.update(tx, a, ev, now)
		}
		if err == nil && ref.why != "" {
			return errRefused
		}
		return err
	})
	if errors.Is(err, errRefused) {
		return s.refusePayment(ctx, ev, ref, now)
	}
	if err == nil && activated != "" {
		if err := s.syncDNS(ctx, activated); err != nil {
			s.log.Warn("dns after link; the sweep retries", "err", err)
		}
	}
	return err
}

// update applies an event to the customer's account.
func (s *Server) update(tx *store.Tx, a store.Account, ev billing.Event, now time.Time) (refused, error) {
	if ev.LinkID != "" {
		l, err := tx.Link(ev.LinkID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return refused{}, err
		}
		if err != nil || l.Account != a.ID {
			// A customer who already has an account paid for another
			// machine's link: another subscription, which must not move this
			// account's dates. The merchant of record matches customers by
			// email, so attaching that machine would let anyone who can check
			// out under an owner's email take the handle.
			return refused{why: "checkout by a customer who has an account", account: a.ID}, nil
		}
	}
	if ev.Occurred.Before(a.BillingAt) {
		return refused{}, nil
	}
	if ev.Status != "" {
		a.BillingStatus = ev.Status
	}
	// A subscription in dunning or paused reports a period it has not paid
	// for; only payments and live subscriptions move paid_through.
	if !ev.PaidThrough.IsZero() && ev.Status != "past_due" && ev.Status != "paused" {
		a.PaidThrough = ev.PaidThrough
	}
	a.BillingAt = ev.Occurred
	if err := tx.UpdateAccount(a); err != nil {
		return refused{}, err
	}
	return refused{}, tx.AddEvent(a.ID, "billing", now)
}

// activate turns the paid link an event names into an account, a handle at
// generation 1 and a device, and returns the handle. A link paid after its
// hour is still honoured, while the week's activations allow it: the money
// came in. A payment for a link that is gone or already paid, from a key
// that is denied or already a device, or late past the weekly cap, is
// refused.
func (s *Server) activate(tx *store.Tx, ev billing.Event, now time.Time) (string, refused, error) {
	if ev.LinkID == "" || ev.PaidThrough.IsZero() || !slices.Contains([]string{"", "active", "trialing"}, ev.Status) {
		return "", refused{}, nil // nothing paid to attach yet; a later event says more
	}
	l, err := tx.Link(ev.LinkID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", refused{why: "payment for an unknown or forgotten link"}, nil
	case err != nil:
		return "", refused{}, err
	case l.Status == linkActive || l.Status == linkRefused:
		return "", refused{why: "payment for a link already settled"}, nil
	}
	if d, err := tx.DeviceByKeyID(l.KeyID); err == nil {
		return "", refused{why: "second payment from a linked device", account: d.Account}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", refused{}, err
	}
	if _, err := tx.DeniedKey(l.KeyID); err == nil {
		return "", refused{why: "payment from a denied key"}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", refused{}, err
	}
	// A link paid in its hour was counted against the weekly cap when it
	// started; one paid later was not.
	if l.Status != linkPending || !now.Before(l.Expires) {
		if full, err := s.activationsFull(tx, now); err != nil {
			return "", refused{}, err
		} else if full {
			return "", refused{why: "late payment past the weekly activation cap"}, nil
		}
	}
	name := l.Handle
	if name != "" {
		if taken, err := s.handleTaken(tx, name, l.KeyID, now); err != nil {
			return "", refused{}, err
		} else if taken {
			name = ""
		}
	}
	if name == "" {
		if name, err = s.pickHandle(tx, now); err != nil {
			return "", refused{}, err
		}
	}
	status := ev.Status
	if status == "" {
		status = "active"
	}
	a := store.Account{ID: "acct_" + randomID(), BillingCustomer: ev.Customer, Plan: s.cfg.Plan, BillingStatus: status,
		PaidThrough: ev.PaidThrough, BillingAt: ev.Occurred, ACMEAccount: l.ACMEAccount, Created: now}
	h := store.Handle{Name: name, Skeleton: skeleton(name), Account: a.ID, Gen: 1, GenAt: now, Created: now, DNSPending: true}
	d := store.Device{Pub: l.DevicePub, KeyID: l.KeyID, Account: a.ID, Gen: 1, Created: now, LastSeenDay: now.Format(time.DateOnly)}
	l.Status, l.Account = linkActive, a.ID
	for _, f := range []func() error{
		func() error { return tx.InsertAccount(a) },
		func() error { return tx.InsertHandle(h) },
		func() error { return tx.InsertDevice(d) },
		func() error { return tx.UpdateLink(l) },
		func() error { return tx.AddEvent(a.ID, "link", now) },
	} {
		if err := f(); err != nil {
			return "", refused{}, err
		}
	}
	return name, refused{}, nil
}

// refusePayment undoes a refused payment: the merchant of record cancels
// the subscription and refunds it, and only then is the event recorded, the
// link marked refused so its daemon hears why, and the refusal noted on the
// account it came near. A link whose key another payment linked is marked
// active instead, so its poll finds the machine linked, but it names no
// account: the refused subscription's later events must never reach one.
// If the merchant of record fails, so does the webhook, and its retry tries
// again.
func (s *Server) refusePayment(ctx context.Context, ev billing.Event, ref refused, now time.Time) error {
	s.log.Warn("payment refused; cancelling and refunding it", "why", ref.why, "link", ev.LinkID, "account", ref.account)
	if err := s.billing.Refuse(ctx, ev.Subscription, ev.Transaction); err != nil {
		return fmt.Errorf("refusing a payment (%s): %w", ref.why, err)
	}
	return s.store.Update(ctx, func(tx *store.Tx) error {
		seen, err := tx.SeenBillingEvent(ev.ID, ev.Occurred)
		if err != nil || seen {
			return err
		}
		l, err := tx.Link(ev.LinkID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case l.Status == linkPending || l.Status == linkExpired:
			if d, err := tx.DeviceByKeyID(l.KeyID); err == nil && !d.Revoked {
				l.Status = linkActive
			} else {
				l.Status = linkRefused
			}
			if err := tx.UpdateLink(l); err != nil {
				return err
			}
		}
		if ref.account == "" {
			return nil
		}
		if _, err := tx.Account(ref.account); errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		return tx.AddEvent(ref.account, "payment_refused", now)
	})
}
