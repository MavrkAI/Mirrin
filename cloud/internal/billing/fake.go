package billing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Fake is the merchant of record in --dev mode and tests. Its checkout and
// portal pages live on the dev server itself (Origin + /checkout/<link>, +
// /portal/<customer>); paying sends a webhook in Paddle's format, signed
// with Secret the way Paddle signs, so the server's webhook path runs as it
// would in production.
type Fake struct {
	Origin string        // the dev server, http://127.0.0.1:port
	Secret []byte        // signs the fake's webhooks
	Period time.Duration // how long one payment lasts; 30 days if zero
	Now    func() time.Time

	mu        sync.Mutex
	customers map[string]string // link id → customer, so paying twice is one customer
	canceled  []string
	refused   []Refusal
	refuseErr error
}

// Refusal is one call to Fake.Refuse.
type Refusal struct {
	Subscription, Transaction string
}

// NewFake returns a fake merchant of record for the dev server at origin,
// with a random webhook secret.
func NewFake(origin string) *Fake {
	secret := make([]byte, 32)
	rand.Read(secret)
	return &Fake{Origin: strings.TrimSuffix(origin, "/"), Secret: secret}
}

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Checkout implements Provider: the dev server's own checkout page.
func (f *Fake) Checkout(_ context.Context, linkID, _ string) (string, error) {
	if !validID(linkID, "lk_") {
		return "", fmt.Errorf("billing: bad link id %q", clip(linkID))
	}
	return f.Origin + "/checkout/" + linkID, nil
}

// Portal implements Provider: the dev server's own portal page.
func (f *Fake) Portal(_ context.Context, customer string) (string, error) {
	if !validID(customer, "ctm_") {
		return "", fmt.Errorf("billing: bad customer id %q", clip(customer))
	}
	return f.Origin + "/portal/" + customer, nil
}

// Email implements Provider with an address that cannot receive mail.
func (f *Fake) Email(_ context.Context, customer string) (string, error) {
	return "owner+" + customer + "@example.invalid", nil
}

// CancelAll implements Provider by noting the customer.
func (f *Fake) CancelAll(_ context.Context, customer string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, customer)
	return nil
}

// Canceled lists the customers whose subscriptions were canceled.
func (f *Fake) Canceled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.canceled...)
}

// Refuse implements Provider by noting the subscription and transaction.
func (f *Fake) Refuse(_ context.Context, subscription, transaction string) error {
	if !validID(subscription, "sub_") || transaction != "" && !validID(transaction, "txn_") {
		return fmt.Errorf("billing: bad subscription %q or transaction %q", clip(subscription), clip(transaction))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuseErr != nil {
		return f.refuseErr
	}
	f.refused = append(f.refused, Refusal{subscription, transaction})
	return nil
}

// FailRefuse makes Refuse fail with err from now on, as an outage would,
// or succeed again if nil.
func (f *Fake) FailRefuse(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuseErr = err
}

// Refused lists the payments refused, in order.
func (f *Fake) Refused() []Refusal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Refusal(nil), f.refused...)
}

// ParseWebhook implements Provider, checking the signature exactly as
// Paddle's is checked.
func (f *Fake) ParseWebhook(r *http.Request) (Event, error) {
	body, err := verifyWebhook(r, f.Secret, f.now(), DefaultTolerance)
	if err != nil {
		return Event{}, err
	}
	return ParseEvent(body)
}

// PaymentWebhook returns the signed webhook a completed checkout for
// linkID would bring: a subscription.created event naming the link and its
// transaction, paid for Period from now. Paying a link twice names the same
// customer.
func (f *Fake) PaymentWebhook(ctx context.Context, url, linkID string) (*http.Request, error) {
	if !validID(linkID, "lk_") {
		return nil, fmt.Errorf("billing: bad link id %q", clip(linkID))
	}
	f.mu.Lock()
	if f.customers == nil {
		f.customers = map[string]string{}
	}
	customer, ok := f.customers[linkID]
	if !ok {
		customer = "ctm_" + randomID()
		f.customers[linkID] = customer
	}
	f.mu.Unlock()
	period := f.Period
	if period == 0 {
		period = 30 * 24 * time.Hour
	}
	now := f.now().UTC()
	body, err := json.Marshal(map[string]any{
		"event_id":        "evt_" + randomID(),
		"event_type":      "subscription.created",
		"occurred_at":     now.Format(time.RFC3339Nano),
		"notification_id": "ntf_" + randomID(),
		"data": map[string]any{
			"id":             "sub_" + randomID(),
			"transaction_id": "txn_" + randomID(),
			"status":         "active",
			"customer_id":    customer,
			"custom_data":    map[string]string{"link_id": linkID},
			"current_billing_period": map[string]string{
				"starts_at": now.Format(time.RFC3339Nano),
				"ends_at":   now.Add(period).Format(time.RFC3339Nano),
			},
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, SignWebhook(f.Secret, now, body))
	return req, nil
}

func randomID() string {
	b := make([]byte, 15)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
