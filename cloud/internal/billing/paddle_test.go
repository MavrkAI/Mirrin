package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// sdkVector reads the signature, payload and secret key from Paddle's own
// SDK test, stored verbatim in testdata.
func sdkVector(t *testing.T) (sig, payload, secret string) {
	t.Helper()
	b, err := os.ReadFile("testdata/paddle-go-sdk-webhook_verifier_test.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	get := func(name string) string {
		m := regexp.MustCompile(name + "\\s*=\\s*`([^`]*)`").FindSubmatch(b)
		if m == nil {
			t.Fatalf("no %s in the SDK test", name)
		}
		return string(m[1])
	}
	return get("testSignature"), get("testPayload"), get("testSecretKey")
}

func webhookRequest(body, sig string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader(body))
	if sig != "" {
		r.Header.Set(SignatureHeader, sig)
	}
	return r
}

func TestTestdataIsVerbatim(t *testing.T) {
	for file, want := range map[string]string{
		"paddle-go-sdk-webhook_verifier_test.go.txt": "91d7352b8909de8c7d32ddcdae0e3ab9afe1fe3cabc303d4d1910e4f956346c4",
		"paddle-go-sdk-events.json":                  "3f6e4f90b0360920d8691a0b1afe9c87b98b575f576c775e5eb7a0f5d91cbf91",
	} {
		b, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != want {
			t.Errorf("%s changed", file)
		}
	}
}

// Paddle's own signature over Paddle's own notification verifies, at the
// time it was made; anything else does not.
func TestWebhookSignatureVector(t *testing.T) {
	sig, payload, secret := sdkVector(t)
	at := time.Unix(1710929255, 0)
	p := &Paddle{WebhookSecret: secret, Now: func() time.Time { return at.Add(30 * time.Second) }}
	r := webhookRequest(payload, sig)
	ev, err := p.ParseWebhook(r)
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "evt_01hsdn97563968dy0szkmgjwh3" || ev.Type != "price.created" || ev.Relevant() {
		t.Errorf("event %+v: a price.created event names no customer", ev)
	}
	if b, _ := io.ReadAll(r.Body); string(b) != payload {
		t.Error("the body was not put back")
	}

	for name, tc := range map[string]struct {
		body, sig, secret string
		now               time.Time
	}{
		"other body":       {`{}`, sig, secret, at},
		"body plus a byte": {payload + " ", sig, secret, at},
		"wrong secret":     {payload, sig, "abc", at},
		"no signature":     {payload, "", secret, at},
		"bad format":       {payload, "ts=x;h1=y", secret, at},
		"uppercase hex":    {payload, strings.ToUpper(sig[:3]) + sig[3:], secret, at},
		"unknown field":    {payload, sig + ";h2=00", secret, at},
		"two timestamps":   {payload, "ts=1;" + sig, secret, at},
		"too late":         {payload, sig, secret, at.Add(DefaultTolerance + time.Second)},
		"too early":        {payload, sig, secret, at.Add(-DefaultTolerance - time.Second)},
		"empty secret":     {payload, sig, "", at},
	} {
		p := &Paddle{WebhookSecret: tc.secret, Now: func() time.Time { return tc.now }}
		if _, err := p.ParseWebhook(webhookRequest(tc.body, tc.sig)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A second h1, as while a secret rotates, is fine; so is a signature by
	// the rotated-in secret alone.
	two := sig + ";h1=" + strings.Repeat("0", 64)
	if _, err := p.ParseWebhook(webhookRequest(payload, two)); err != nil {
		t.Errorf("two h1 values: %v", err)
	}
	if got := SignWebhook([]byte(secret), at, []byte(payload)); got != sig {
		t.Errorf("SignWebhook = %s, want Paddle's %s", got, sig)
	}
}

// The events in Paddle's SDK test data parse to what the control plane
// keeps.
func TestParseSDKEvents(t *testing.T) {
	b, err := os.ReadFile("testdata/paddle-go-sdk-events.json")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Data []jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	want := map[string]Event{
		"evt_01hywqk7y8qfzj69z3pdvz34qt": {Type: "transaction.completed", Customer: "ctm_01hszn72m8kqyqm8g1jxwxc3t3",
			Subscription: "sub_01hszn787vc5vkkdah3rtd4nft", Transaction: "txn_01hywqfe6yxhxcsfb4ays8mqt3",
			PaidThrough: time.Date(2024, 6, 27, 9, 52, 50, 0, time.UTC)},
		"evt_01hywqfn8b1na40vyarxaxqa9t": {Type: "transaction.updated"},
		"evt_01hv9771tccgcm4y810d8zbceh": {Type: "subscription.created", Customer: "ctm_01hv976dcgq4wmyrp8yq7asfmj", Status: "active",
			Subscription: "sub_01hv9770y40xzc823155s0z4zz", Transaction: "txn_01hv975mbh902hcyb7mks5kt0n",
			PaidThrough: time.Date(2024, 5, 12, 13, 16, 8, 0, time.UTC)},
	}
	seen := 0
	for _, raw := range list.Data {
		ev, err := ParseEvent(raw)
		if err != nil {
			t.Fatal(err)
		}
		w, ok := want[ev.ID]
		if !ok {
			t.Errorf("unexpected event %s %s", ev.ID, ev.Type)
			continue
		}
		seen++
		if ev.Type != w.Type || ev.Customer != w.Customer || ev.Status != w.Status || !ev.PaidThrough.Equal(w.PaidThrough) || ev.LinkID != "" || ev.Occurred.IsZero() ||
			ev.Subscription != w.Subscription || ev.Transaction != w.Transaction {
			t.Errorf("%s: %+v, want %+v", ev.ID, ev, w)
		}
	}
	if seen != len(want) {
		t.Errorf("parsed %d of the %d events", seen, len(want))
	}
}

func TestParseEventRefuses(t *testing.T) {
	base := func(mut func(map[string]any)) []byte {
		ev := map[string]any{"event_id": "evt_1", "event_type": "subscription.updated", "occurred_at": "2026-09-27T03:30:00.5Z",
			"data": map[string]any{"id": "sub_1", "status": "active", "customer_id": "ctm_1", "custom_data": map[string]any{"link_id": "lk_abc"},
				"current_billing_period": map[string]any{"ends_at": "2026-10-27T03:30:00Z"}}}
		mut(ev)
		b, _ := json.Marshal(ev)
		return b
	}
	ev, err := ParseEvent(base(func(map[string]any) {}))
	if err != nil || ev.LinkID != "lk_abc" || ev.Customer != "ctm_1" || ev.Subscription != "sub_1" || ev.Transaction != "" ||
		!ev.PaidThrough.Equal(time.Date(2026, 10, 27, 3, 30, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", ev, err)
	}
	data := func(k string, v any) func(map[string]any) {
		return func(m map[string]any) { m["data"].(map[string]any)[k] = v }
	}
	for name, mut := range map[string]func(map[string]any){
		"no event id":      func(m map[string]any) { delete(m, "event_id") },
		"bad event id":     func(m map[string]any) { m["event_id"] = "evt_../x" },
		"bad time":         func(m map[string]any) { m["occurred_at"] = "yesterday" },
		"bad customer":     data("customer_id", "ctm_A/B"),
		"no customer":      data("customer_id", ""),
		"bad link":         data("custom_data", map[string]any{"link_id": "lk_<script>"}),
		"bad status":       data("status", "free"),
		"bad period end":   data("current_billing_period", map[string]any{"ends_at": "soon"}),
		"no subscription":  func(m map[string]any) { delete(m["data"].(map[string]any), "id") },
		"bad subscription": data("id", "sub_../cancel"),
		"bad transaction":  data("transaction_id", "txn_A"),
	} {
		if _, err := ParseEvent(base(mut)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}
	if _, err := ParseEvent([]byte(`{"event_id":"evt_1","event_id":"evt_2"}`)); err == nil {
		t.Error("duplicate member accepted")
	}
}

// The Paddle API client, against a stand-in for api.paddle.com.
func TestPaddleAPI(t *testing.T) {
	var got []string
	refunded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key_test" || r.Header.Get("Paddle-Version") != "1" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"code":"authentication_malformed","detail":"no"}}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		switch r.Method + " " + r.URL.Path {
		case "POST /transactions":
			io.WriteString(w, `{"data":{"id":"txn_1","checkout":{"url":"https://pay.example/?_ptxn=txn_1"}}}`)
		case "POST /customers/ctm_1/portal-sessions":
			io.WriteString(w, `{"data":{"urls":{"general":{"overview":"https://portal.example/ctm_1"}}}}`)
		case "GET /customers/ctm_1":
			io.WriteString(w, `{"data":{"id":"ctm_1","email":"owner@example.com"}}`)
		case "GET /subscriptions":
			if r.URL.Query().Get("customer_id") != "ctm_1" {
				io.WriteString(w, `{"data":[]}`)
				return
			}
			io.WriteString(w, `{"data":[{"id":"sub_1","status":"active"},{"id":"sub_2","status":"past_due"}]}`)
		case "POST /subscriptions/sub_1/cancel", "POST /subscriptions/sub_2/cancel", "POST /subscriptions/sub_3/cancel":
			io.WriteString(w, `{"data":{"status":"canceled"}}`)
		case "GET /subscriptions/sub_3":
			io.WriteString(w, `{"data":{"id":"sub_3","status":"active"}}`)
		case "GET /subscriptions/sub_4":
			io.WriteString(w, `{"data":{"id":"sub_4","status":"canceled"}}`)
		case "GET /transactions/txn_3":
			io.WriteString(w, `{"data":{"id":"txn_3","status":"completed"}}`)
		case "GET /transactions/txn_4":
			io.WriteString(w, `{"data":{"id":"txn_4","status":"paid"}}`)
		case "GET /adjustments":
			if r.URL.Query().Get("transaction_id") == "txn_3" && r.URL.Query().Get("action") == "refund" && refunded {
				io.WriteString(w, `{"data":[{"id":"adj_1","action":"refund"}]}`)
				return
			}
			io.WriteString(w, `{"data":[]}`)
		case "POST /adjustments":
			refunded = true
			io.WriteString(w, `{"data":{"id":"adj_1","status":"pending_approval"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"not_found","detail":"no"}}`)
		}
	}))
	defer srv.Close()
	p := &Paddle{APIBase: srv.URL, APIKey: "key_test", Prices: map[string]string{"cloud": "pri_1"}}
	ctx := t.Context()
	if u, err := p.Checkout(ctx, "lk_abc", "cloud"); err != nil || u != "https://pay.example/?_ptxn=txn_1" {
		t.Errorf("Checkout: %q %v", u, err)
	}
	if !strings.Contains(got[0], `"price_id":"pri_1"`) || !strings.Contains(got[0], `"custom_data":{"link_id":"lk_abc"}`) {
		t.Errorf("transaction request: %s", got[0])
	}
	if strings.Contains(got[0], "@") {
		t.Error("the checkout request carries an email")
	}
	if u, err := p.Portal(ctx, "ctm_1"); err != nil || u != "https://portal.example/ctm_1" {
		t.Errorf("Portal: %q %v", u, err)
	}
	if e, err := p.Email(ctx, "ctm_1"); err != nil || e != "owner@example.com" {
		t.Errorf("Email: %q %v", e, err)
	}
	got = nil
	if err := p.CancelAll(ctx, "ctm_1"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !strings.HasPrefix(got[1], "POST /subscriptions/sub_1/cancel ") || !strings.Contains(got[2], `"effective_from":"immediately"`) {
		t.Errorf("CancelAll calls: %q", got)
	}
	if err := p.CancelAll(ctx, "ctm_2"); err != nil {
		t.Errorf("CancelAll with nothing to cancel: %v", err)
	}

	// Refuse cancels the subscription and refunds the completed payment,
	// once: doing it again finds the refund and sends nothing.
	got = nil
	if err := p.Refuse(ctx, "sub_3", "txn_3"); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /subscriptions/sub_3 ", `POST /subscriptions/sub_3/cancel {"effective_from":"immediately"}`, "GET /transactions/txn_3 ", "GET /adjustments ",
		`POST /adjustments {"action":"refund","type":"full","transaction_id":"txn_3","reason":"` + refundReason + `"}`}
	if !slices.Equal(got, want) {
		t.Errorf("Refuse calls:\n%q\nwant\n%q", got, want)
	}
	got = nil
	if err := p.Refuse(ctx, "sub_3", "txn_3"); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "POST /adjustments") }) {
		t.Errorf("a second Refuse refunded again: %q", got)
	}
	// A cancelled subscription is not cancelled again, and a payment not
	// yet completed is left for its transaction.completed event.
	got = nil
	if err := p.Refuse(ctx, "sub_4", "txn_4"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"GET /subscriptions/sub_4 ", "GET /transactions/txn_4 "}; !slices.Equal(got, want) {
		t.Errorf("Refuse of a cancelled subscription and a paid transaction: %q", got)
	}
	if err := p.Refuse(ctx, "sub_../x", ""); err == nil {
		t.Error("a subscription id with a path in it was sent")
	}
	if err := p.Refuse(ctx, "sub_9", ""); err == nil {
		t.Error("Refuse hid an API failure")
	}
	if _, err := p.Email(ctx, "ctm_1/../x"); err == nil {
		t.Error("a customer id with a path in it was sent")
	}
	if _, err := p.Checkout(ctx, "lk_abc", "household"); err == nil {
		t.Error("checkout for a plan with no price")
	}
	if _, err := (&Paddle{APIBase: srv.URL, APIKey: "wrong", Prices: p.Prices}).Checkout(ctx, "lk_abc", "cloud"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused call: %v", err)
	}
}

func TestFake(t *testing.T) {
	f := NewFake("http://127.0.0.1:1234")
	now := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
	f.Now = func() time.Time { return now }
	req, err := f.PaymentWebhook(t.Context(), "http://127.0.0.1:1234/v1/billing/webhook", "lk_abc")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := f.ParseWebhook(req)
	if err != nil {
		t.Fatal(err)
	}
	if ev.LinkID != "lk_abc" || !strings.HasPrefix(ev.Customer, "ctm_") || !ev.PaidThrough.Equal(now.Add(30*24*time.Hour)) || ev.Status != "active" {
		t.Errorf("%+v", ev)
	}
	again, _ := f.PaymentWebhook(t.Context(), "http://127.0.0.1:1234/v1/billing/webhook", "lk_abc")
	ev2, _ := f.ParseWebhook(again)
	if ev2.Customer != ev.Customer || ev2.ID == ev.ID {
		t.Error("paying one link twice is not one customer with two events")
	}
	// A real Paddle secret does not verify the fake's webhooks.
	req, _ = f.PaymentWebhook(t.Context(), "http://x/", "lk_abc")
	if _, err := (&Paddle{WebhookSecret: "pdl_ntfset_other", Now: f.Now}).ParseWebhook(req); !errors.Is(err, ErrSignature) {
		t.Errorf("another secret: %v", err)
	}
}
