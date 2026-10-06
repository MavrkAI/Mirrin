// Package billing talks to the merchant of record, which holds the email
// address and the card so the control plane never does. Paddle is the one
// provider; Fake stands in for it in --dev mode and tests. Both check
// webhooks the same way (Paddle's signature scheme) and read the same event
// format, so dev mode exercises the webhook path end to end.
package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Provider is a merchant of record.
type Provider interface {
	// Checkout returns the page where the user pays for link linkID. The
	// link id travels as custom data and comes back in the webhooks.
	Checkout(ctx context.Context, linkID, plan string) (string, error)
	// Portal returns the customer's page for receipts, the card and
	// cancelling.
	Portal(ctx context.Context, customer string) (string, error)
	// ParseWebhook checks a webhook's signature and reads its event.
	ParseWebhook(r *http.Request) (Event, error)
	// Email fetches the customer's email address, for GET /v1/me only. It
	// is never stored.
	Email(ctx context.Context, customer string) (string, error)
	// CancelAll ends the customer's subscriptions at once, when a deleted
	// account's undo window has passed; none left is not an error.
	CancelAll(ctx context.Context, customer string) error
	// Refuse undoes a payment the control plane will not attach to any
	// machine: it cancels the subscription at once and refunds the
	// transaction, if one is named, in full. Doing it again is not an error,
	// so a webhook that brings it again, or is retried, is harmless.
	Refuse(ctx context.Context, subscription, transaction string) error
}

// Event is what a webhook says, reduced to what the control plane keeps.
type Event struct {
	ID       string    // the provider's event id; a repeat is a no-op
	Type     string    // the provider's event type, for logs
	Occurred time.Time // when it happened; the latest event wins
	Customer string    // "" for an event about nothing we track
	LinkID   string    // the link a checkout was for, if the event says
	// Subscription is the subscription the event is about; always set when
	// Customer is.
	Subscription string
	// Transaction is the payment the event is about, or "" when it names
	// none.
	Transaction string
	// Status is the subscription's status (active, past_due, canceled,
	// paused, trialing), or "" when the event does not say.
	Status string
	// PaidThrough is the end of the paid period, or zero when the event
	// does not say.
	PaidThrough time.Time
}

// Relevant reports whether the event is about a customer at all.
func (e Event) Relevant() bool { return e.Customer != "" }

var (
	// ErrSignature means the webhook's signature is missing, malformed,
	// wrong or too old.
	ErrSignature = errors.New("billing: webhook signature refused")
	// ErrMalformed means a signed webhook does not parse.
	ErrMalformed = errors.New("billing: malformed webhook")
)

const (
	// MaxWebhookBody bounds a webhook body.
	MaxWebhookBody = 256 << 10
	// SignatureHeader carries a webhook's signature.
	SignatureHeader = "Paddle-Signature"
	// DefaultTolerance is how far a webhook's timestamp may be from our
	// clock. Replays inside it are caught by the event id.
	DefaultTolerance   = 5 * time.Minute
	maxSignatureHeader = 1024
	maxSignatures      = 4
)

// signature is a parsed Paddle-Signature header: ts=<unix>;h1=<hex>, with
// up to four h1 values while a secret is rotated.
type signature struct {
	ts  string
	h1s [][]byte
}

// parseSignature reads a Paddle-Signature header strictly: only ts and h1,
// one ts, one to four h1 of 64 lowercase hex digits.
func parseSignature(h string) (signature, error) {
	var s signature
	if h == "" || len(h) > maxSignatureHeader {
		return s, ErrSignature
	}
	for part := range strings.SplitSeq(h, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return signature{}, ErrSignature
		}
		switch k {
		case "ts":
			if s.ts != "" || v == "" || len(v) > 12 || strings.Trim(v, "0123456789") != "" {
				return signature{}, ErrSignature
			}
			s.ts = v
		case "h1":
			if len(v) != 64 || strings.Trim(v, "0123456789abcdef") != "" || len(s.h1s) == maxSignatures {
				return signature{}, ErrSignature
			}
			b, _ := hex.DecodeString(v)
			s.h1s = append(s.h1s, b)
		default:
			return signature{}, ErrSignature
		}
	}
	if s.ts == "" || len(s.h1s) == 0 {
		return signature{}, ErrSignature
	}
	return s, nil
}

// mac is HMAC-SHA256 of "ts:body" under secret, as Paddle signs.
func mac(secret []byte, ts string, body []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(ts))
	h.Write([]byte(":"))
	h.Write(body)
	return h.Sum(nil)
}

// SignWebhook returns the Paddle-Signature header for body at ts.
func SignWebhook(secret []byte, ts time.Time, body []byte) string {
	t := strconv.FormatInt(ts.Unix(), 10)
	return "ts=" + t + ";h1=" + hex.EncodeToString(mac(secret, t, body))
}

// verifyWebhook reads r's body (at most MaxWebhookBody) and checks its
// signature under secret, within tolerance of now. It returns the body.
func verifyWebhook(r *http.Request, secret []byte, now time.Time, tolerance time.Duration) ([]byte, error) {
	if len(secret) == 0 {
		return nil, errors.New("billing: no webhook secret")
	}
	sig, err := parseSignature(r.Header.Get(SignatureHeader))
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxWebhookBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxWebhookBody {
		return nil, fmt.Errorf("%w: body over %d bytes", ErrMalformed, MaxWebhookBody)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	want := mac(secret, sig.ts, body)
	ok := 0
	for _, h := range sig.h1s {
		ok |= subtleEq(h, want)
	}
	if ok != 1 {
		return nil, ErrSignature
	}
	ts, _ := strconv.ParseInt(sig.ts, 10, 64)
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return nil, fmt.Errorf("%w: timestamp %s is too far from now", ErrSignature, time.Unix(ts, 0).UTC().Format(time.RFC3339))
	}
	return body, nil
}

func subtleEq(a, b []byte) int {
	if hmac.Equal(a, b) {
		return 1
	}
	return 0
}

// paddleEvent is the part of a Paddle notification the control plane reads.
// Unknown members are ignored: Paddle adds fields without notice.
type paddleEvent struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	OccurredAt string `json:"occurred_at"`
	Data       struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		CustomerID     string `json:"customer_id"`
		SubscriptionID string `json:"subscription_id"`
		TransactionID  string `json:"transaction_id"`
		CustomData     *struct {
			LinkID string `json:"link_id"`
		} `json:"custom_data"`
		BillingPeriod        *period `json:"billing_period"`
		CurrentBillingPeriod *period `json:"current_billing_period"`
	} `json:"data"`
}

type period struct {
	StartsAt string `json:"starts_at"`
	EndsAt   string `json:"ends_at"`
}

// ParseEvent reads a Paddle notification (a verified webhook body, or an
// item of the events API). Events other than subscription and completed
// transaction events come back with no customer.
func ParseEvent(body []byte) (Event, error) {
	var p paddleEvent
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if !validID(p.EventID, "evt_") {
		return Event{}, fmt.Errorf("%w: event_id %q", ErrMalformed, clip(p.EventID))
	}
	occurred, err := time.Parse(time.RFC3339Nano, p.OccurredAt)
	if err != nil {
		return Event{}, fmt.Errorf("%w: occurred_at: %v", ErrMalformed, err)
	}
	e := Event{ID: p.EventID, Type: p.EventType, Occurred: occurred.UTC()}
	d := p.Data
	var per *period
	switch {
	case p.EventType == "transaction.completed" || p.EventType == "transaction.paid":
		// A payment. Only one for a subscription extends anything.
		if d.SubscriptionID == "" {
			return e, nil
		}
		per = d.BillingPeriod
		e.Subscription, e.Transaction = d.SubscriptionID, d.ID
	case strings.HasPrefix(p.EventType, "subscription."):
		e.Status = d.Status
		per = d.CurrentBillingPeriod
		e.Subscription, e.Transaction = d.ID, d.TransactionID
	default:
		return e, nil
	}
	if !validID(d.CustomerID, "ctm_") {
		return Event{}, fmt.Errorf("%w: customer_id %q", ErrMalformed, clip(d.CustomerID))
	}
	if !validID(e.Subscription, "sub_") {
		return Event{}, fmt.Errorf("%w: subscription %q", ErrMalformed, clip(e.Subscription))
	}
	if e.Transaction != "" && !validID(e.Transaction, "txn_") {
		return Event{}, fmt.Errorf("%w: transaction %q", ErrMalformed, clip(e.Transaction))
	}
	e.Customer = d.CustomerID
	if d.CustomData != nil && d.CustomData.LinkID != "" {
		if !validID(d.CustomData.LinkID, "lk_") {
			return Event{}, fmt.Errorf("%w: link_id %q", ErrMalformed, clip(d.CustomData.LinkID))
		}
		e.LinkID = d.CustomData.LinkID
	}
	if per != nil && per.EndsAt != "" {
		t, err := time.Parse(time.RFC3339Nano, per.EndsAt)
		if err != nil {
			return Event{}, fmt.Errorf("%w: ends_at: %v", ErrMalformed, err)
		}
		e.PaidThrough = t.UTC().Truncate(time.Second)
	}
	switch e.Status {
	case "", "active", "trialing", "past_due", "paused", "canceled":
	default:
		return Event{}, fmt.Errorf("%w: status %q", ErrMalformed, clip(e.Status))
	}
	return e, nil
}

// validID accepts an id with the given prefix followed by 1 to 64 of a-z,
// 0-9 and '_' or '-'. Ids go into URLs and the database.
func validID(s, prefix string) bool {
	rest, ok := strings.CutPrefix(s, prefix)
	return ok && rest != "" && len(rest) <= 64 && strings.Trim(rest, "abcdefghijklmnopqrstuvwxyz0123456789_-") == ""
}

func clip(s string) string {
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return strconv.Quote(s)[1 : len(strconv.Quote(s))-1]
}
