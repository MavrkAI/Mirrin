package billing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Paddle is Paddle Billing (https://developer.paddle.com), the merchant of
// record. Checkout creates a transaction whose checkout URL is Paddle's
// hosted page; the default payment link must be set in the Paddle
// dashboard.
type Paddle struct {
	// APIBase is https://api.paddle.com, or https://sandbox-api.paddle.com.
	APIBase string
	// APIKey authenticates API calls.
	APIKey string
	// WebhookSecret is the notification destination's secret key.
	WebhookSecret string
	// Prices maps a plan to its Paddle price id.
	Prices map[string]string
	// HTTP sends the API calls; nil means a client with a 20 s timeout.
	HTTP *http.Client
	// Now is the clock webhooks are checked against; nil means time.Now.
	Now func() time.Time
	// Tolerance is how far a webhook's timestamp may be from Now; zero
	// means DefaultTolerance.
	Tolerance time.Duration
}

// maxReply bounds what the Paddle API may send back.
const maxReply = 1 << 20

func (p *Paddle) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (p *Paddle) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// call sends one API request and decodes the "data" member of the answer
// into out.
func (p *Paddle) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(p.APIBase, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Paddle-Version", "1")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := *p.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("billing: paddle %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return fmt.Errorf("billing: paddle %s %s: %w", method, path, err)
	}
	if len(b) > maxReply {
		return fmt.Errorf("billing: paddle %s %s: answer over %d bytes", method, path, maxReply)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			} `json:"error"`
		}
		json.Unmarshal(b, &e)
		return fmt.Errorf("billing: paddle %s %s: HTTP %d %s %s", method, path, resp.StatusCode, clip(e.Error.Code), clip(e.Error.Detail))
	}
	if out == nil {
		return nil
	}
	wrap := struct {
		Data any `json:"data"`
	}{out}
	if err := json.Unmarshal(b, &wrap); err != nil {
		return fmt.Errorf("billing: paddle %s %s: %w", method, path, err)
	}
	return nil
}

// Checkout implements Provider: a transaction for the plan's price, with
// the link id as custom data, and Paddle's checkout URL for it.
func (p *Paddle) Checkout(ctx context.Context, linkID, plan string) (string, error) {
	price, ok := p.Prices[plan]
	if !ok {
		return "", fmt.Errorf("billing: no Paddle price for plan %q", plan)
	}
	if !validID(linkID, "lk_") {
		return "", fmt.Errorf("billing: bad link id %q", clip(linkID))
	}
	in := map[string]any{
		"items":       []map[string]any{{"price_id": price, "quantity": 1}},
		"custom_data": map[string]string{"link_id": linkID},
	}
	var out struct {
		Checkout *struct {
			URL string `json:"url"`
		} `json:"checkout"`
	}
	if err := p.call(ctx, http.MethodPost, "/transactions", in, &out); err != nil {
		return "", err
	}
	if out.Checkout == nil || out.Checkout.URL == "" {
		return "", errors.New("billing: Paddle gave no checkout URL; set a default payment link in the dashboard")
	}
	return checkURL(out.Checkout.URL)
}

// Portal implements Provider with a customer portal session.
func (p *Paddle) Portal(ctx context.Context, customer string) (string, error) {
	if !validID(customer, "ctm_") {
		return "", fmt.Errorf("billing: bad customer id %q", clip(customer))
	}
	var out struct {
		URLs struct {
			General struct {
				Overview string `json:"overview"`
			} `json:"general"`
		} `json:"urls"`
	}
	if err := p.call(ctx, http.MethodPost, "/customers/"+customer+"/portal-sessions", map[string]any{}, &out); err != nil {
		return "", err
	}
	return checkURL(out.URLs.General.Overview)
}

// Email implements Provider by reading the customer from Paddle.
func (p *Paddle) Email(ctx context.Context, customer string) (string, error) {
	if !validID(customer, "ctm_") {
		return "", fmt.Errorf("billing: bad customer id %q", clip(customer))
	}
	var out struct {
		Email string `json:"email"`
	}
	if err := p.call(ctx, http.MethodGet, "/customers/"+customer, nil, &out); err != nil {
		return "", err
	}
	return out.Email, nil
}

// CancelAll implements Provider: every subscription of the customer that
// is not already canceled is canceled immediately.
func (p *Paddle) CancelAll(ctx context.Context, customer string) error {
	if !validID(customer, "ctm_") {
		return fmt.Errorf("billing: bad customer id %q", clip(customer))
	}
	var subs []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	q := url.Values{"customer_id": {customer}, "status": {"active,past_due,paused,trialing"}, "per_page": {"50"}}
	if err := p.call(ctx, http.MethodGet, "/subscriptions?"+q.Encode(), nil, &subs); err != nil {
		return err
	}
	for _, sub := range subs {
		if !validID(sub.ID, "sub_") {
			return fmt.Errorf("billing: bad subscription id %q", clip(sub.ID))
		}
		if err := p.call(ctx, http.MethodPost, "/subscriptions/"+sub.ID+"/cancel", map[string]string{"effective_from": "immediately"}, nil); err != nil {
			return err
		}
	}
	return nil
}

// refundReason is what Paddle's dashboard shows for a refused payment.
const refundReason = "Mirrin Cloud could not attach this payment to a machine, so it is cancelled and refunded in full."

// Refuse implements Provider. The subscription is cancelled at once unless
// it already is. The transaction is refunded in full once it is completed
// (Paddle refunds nothing sooner), unless a refund of it already exists; a
// payment not yet completed brings its own transaction.completed event,
// which comes here again.
func (p *Paddle) Refuse(ctx context.Context, subscription, transaction string) error {
	if !validID(subscription, "sub_") {
		return fmt.Errorf("billing: bad subscription id %q", clip(subscription))
	}
	if transaction != "" && !validID(transaction, "txn_") {
		return fmt.Errorf("billing: bad transaction id %q", clip(transaction))
	}
	var sub struct {
		Status string `json:"status"`
	}
	if err := p.call(ctx, http.MethodGet, "/subscriptions/"+subscription, nil, &sub); err != nil {
		return err
	}
	if sub.Status != "canceled" {
		if err := p.call(ctx, http.MethodPost, "/subscriptions/"+subscription+"/cancel", map[string]string{"effective_from": "immediately"}, nil); err != nil {
			return err
		}
	}
	if transaction == "" {
		return nil
	}
	var txn struct {
		Status string `json:"status"`
	}
	if err := p.call(ctx, http.MethodGet, "/transactions/"+transaction, nil, &txn); err != nil {
		return err
	}
	if txn.Status != "completed" {
		return nil
	}
	var refunds []struct {
		ID string `json:"id"`
	}
	q := url.Values{"transaction_id": {transaction}, "action": {"refund"}}
	if err := p.call(ctx, http.MethodGet, "/adjustments?"+q.Encode(), nil, &refunds); err != nil {
		return err
	}
	if len(refunds) > 0 {
		return nil
	}
	refund := struct {
		Action        string `json:"action"`
		Type          string `json:"type"`
		TransactionID string `json:"transaction_id"`
		Reason        string `json:"reason"`
	}{"refund", "full", transaction, refundReason}
	return p.call(ctx, http.MethodPost, "/adjustments", refund, nil)
}

// ParseWebhook implements Provider.
func (p *Paddle) ParseWebhook(r *http.Request) (Event, error) {
	tol := p.Tolerance
	if tol == 0 {
		tol = DefaultTolerance
	}
	body, err := verifyWebhook(r, []byte(p.WebhookSecret), p.now(), tol)
	if err != nil {
		return Event{}, err
	}
	return ParseEvent(body)
}

// checkURL accepts a page for the user's browser: https, printable ASCII,
// at most 2048 bytes, no user info.
func checkURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(s) > 2048 ||
		strings.ContainsFunc(s, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return "", fmt.Errorf("billing: the merchant of record sent a page that is not an https URL: %q", clip(s))
	}
	return s, nil
}
