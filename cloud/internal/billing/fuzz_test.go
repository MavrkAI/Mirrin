package billing

import (
	"bytes"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Nightly CI runs each of these for 60 s (.github/workflows/fuzz.yml).

// A header that parses has one ts of digits and one to four h1 values of
// 64 lowercase hex digits, and nothing else.
func FuzzParseSignature(f *testing.F) {
	for _, s := range []string{
		"ts=1710929255;h1=6c05ef8fa83c44d751be6d259ec955ce5638e2c54095bf128e408e2fce1589c8",
		"ts=1;h1=" + strings.Repeat("0", 64) + ";h1=" + strings.Repeat("f", 64),
		"ts=x;h1=y", "", ";", "ts=1", "h1=" + strings.Repeat("a", 64), "ts=1;ts=2;h1=" + strings.Repeat("a", 64),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, h string) {
		s, err := parseSignature(h)
		if err != nil {
			return
		}
		if s.ts == "" || strings.Trim(s.ts, "0123456789") != "" || len(s.h1s) == 0 || len(s.h1s) > maxSignatures {
			t.Fatalf("%q parsed to %+v", h, s)
		}
		want := "ts=" + s.ts
		for _, b := range s.h1s {
			want += ";h1=" + hex.EncodeToString(b)
		}
		if !strings.Contains(";"+h+";", ";ts="+s.ts+";") || len(want) != len(h) {
			t.Fatalf("%q parsed to %+v", h, s)
		}
	})
}

// ParseEvent never panics, and what it accepts has ids fit for URLs and
// the database, with a subscription whenever it names a customer.
func FuzzParseEvent(f *testing.F) {
	if b, err := os.ReadFile("testdata/paddle-go-sdk-events.json"); err == nil {
		f.Add(b)
	}
	f.Add([]byte(`{"event_id":"evt_1","event_type":"subscription.updated","occurred_at":"2026-09-27T03:30:00Z","data":{"id":"sub_1","transaction_id":"txn_1","status":"active","customer_id":"ctm_1","custom_data":{"link_id":"lk_a"},"current_billing_period":{"ends_at":"2026-10-27T03:30:00Z"}}}`))
	f.Add([]byte(`{"event_id":"evt_1","event_type":"transaction.completed","occurred_at":"2026-09-27T03:30:00Z","data":{"id":"txn_1","customer_id":"ctm_1","subscription_id":"sub_1","billing_period":{"ends_at":"2026-10-27T03:30:00Z"}}}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		ev, err := ParseEvent(b)
		if err != nil {
			return
		}
		if !validID(ev.ID, "evt_") || ev.Customer != "" && !validID(ev.Customer, "ctm_") || ev.LinkID != "" && !validID(ev.LinkID, "lk_") ||
			ev.Customer != "" && !validID(ev.Subscription, "sub_") || ev.Transaction != "" && !validID(ev.Transaction, "txn_") {
			t.Fatalf("%+v", ev)
		}
	})
}

// A webhook verifies only with the right secret over the exact body.
func FuzzVerifyWebhook(f *testing.F) {
	f.Add([]byte(`{"event_id":"evt_1"}`), []byte("secret"), byte(0))
	f.Fuzz(func(t *testing.T, body, secret []byte, flip byte) {
		if len(secret) == 0 || len(body) > MaxWebhookBody {
			return
		}
		now := time.Unix(1790000000, 0)
		sig := SignWebhook(secret, now, body)
		r := httptest.NewRequest("POST", "/", bytes.NewReader(body))
		r.Header.Set(SignatureHeader, sig)
		if _, err := verifyWebhook(r, secret, now, DefaultTolerance); err != nil {
			t.Fatalf("own signature refused: %v", err)
		}
		if len(body) == 0 {
			return
		}
		tampered := bytes.Clone(body)
		tampered[int(flip)%len(tampered)] ^= 1 | flip
		r = httptest.NewRequest("POST", "/", bytes.NewReader(tampered))
		r.Header.Set(SignatureHeader, sig)
		if _, err := verifyWebhook(r, secret, now, DefaultTolerance); err == nil {
			t.Fatal("a tampered body verified")
		}
	})
}
