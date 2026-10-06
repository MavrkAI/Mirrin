package server

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/billing"
	"github.com/MavrkAI/Mirrin/cloud/internal/store"
)

// accountOf returns the account holding a handle.
func (e *env) accountOf(t *testing.T, handle string) store.Account {
	t.Helper()
	var a store.Account
	err := e.store.View(t.Context(), func(tx *store.Tx) error {
		h, err := tx.Handle(handle)
		if err != nil {
			return err
		}
		a, err = tx.Account(h.Account)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A customer who already has an account cannot attach another machine by
// paying for its link: the merchant of record matches customers by email,
// so that would let anyone who checks out under an owner's email take the
// handle. The owner relinking a new key after unlinking looks the same.
// Either way the payment is cancelled and refunded at once, the machine
// hears why, and the account's dates do not move.
func TestSecondCheckoutIsRefused(t *testing.T) {
	e := newEnv(t, true, nil)
	now := e.now()
	paid := now.Add(30 * 24 * time.Hour)
	owner := newKey(t)
	first := e.startLink(t, owner, "first-home-11")
	if r := e.webhook(t, withIDs(subscriptionEvent("evt_a", "ctm_owner", first, now, paid, "active"), "sub_a", "txn_a")); r.status != 200 {
		t.Fatalf("first payment: %d %s", r.status, r.body)
	}
	if r := send(t, e.signed(t, owner, "DELETE", "/v1/device", nil)); r.status != http.StatusNoContent {
		t.Fatalf("unlink: %d", r.status)
	}

	fresh := newKey(t)
	second := e.startLink(t, fresh, "")
	ev := withIDs(subscriptionEvent("evt_b", "ctm_owner", second, now.Add(time.Second), now.Add(60*24*time.Hour), "active"), "sub_b", "txn_b")
	if r := e.webhook(t, ev); r.status != http.StatusOK {
		t.Fatalf("second payment: %d %s", r.status, r.body)
	}
	if got := e.fake.Refused(); !slices.Equal(got, []billing.Refusal{{Subscription: "sub_b", Transaction: "txn_b"}}) {
		t.Errorf("refused at the merchant of record: %v, want sub_b and txn_b", got)
	}
	r, _ := e.poll(t, fresh, second)
	if r.status != http.StatusConflict || r.code != "payment_refused" || !strings.Contains(string(r.body), "refunded") {
		t.Errorf("polling the refused link: %d %s, want 409 payment_refused", r.status, r.body)
	}
	if r := send(t, e.signed(t, fresh, "GET", "/v1/me", nil)); r.status != http.StatusUnauthorized {
		t.Errorf("the second machine reads /v1/me: %d", r.status)
	}
	a := e.accountOf(t, "first-home-11")
	if !a.PaidThrough.Equal(paid.Truncate(time.Second)) || a.BillingStatus != "active" {
		t.Errorf("the second payment moved the account: %+v", a)
	}

	// The same event again, and the refused subscription's own later
	// events, change nothing; refusing again is harmless.
	e.webhook(t, ev)
	cancelled := withIDs(subscriptionEvent("evt_c", "ctm_owner", second, now.Add(time.Minute), now.Add(60*24*time.Hour), "canceled"), "sub_b", "")
	if r := e.webhook(t, cancelled); r.status != http.StatusOK {
		t.Fatalf("the refused subscription's cancellation: %d", r.status)
	}
	if got := e.fake.Refused(); len(got) != 2 {
		t.Errorf("refusals after a repeat and a cancellation: %v", got)
	}
	if a := e.accountOf(t, "first-home-11"); a.BillingStatus != "active" || !a.PaidThrough.Equal(paid.Truncate(time.Second)) {
		t.Errorf("the refused subscription's cancellation reached the account: %+v", a)
	}
	var kinds []string
	e.store.View(t.Context(), func(tx *store.Tx) error {
		rows, err := tx.Rows("events", "account", a.ID)
		for _, r := range rows {
			kinds = append(kinds, r["kind"].(string))
		}
		return err
	})
	if !slices.Contains(kinds, "payment_refused") {
		t.Errorf("the account does not note the refusal for support: %v", kinds)
	}
}

// A payment that names a link nobody knows, a link the sweep forgot, a
// denied key or a machine already linked is cancelled and refunded, not left
// charging every month. A link paid after its hour, but before it was
// forgotten, is honoured.
func TestUnattachedPaymentsAreRefused(t *testing.T) {
	e := newEnv(t, true, nil)
	pay := func(evID, customer, link, sub string) resp {
		t.Helper()
		now := e.now()
		return e.webhook(t, withIDs(subscriptionEvent(evID, customer, link, now, now.Add(30*24*time.Hour), "active"), sub, "txn_"+sub[4:]))
	}
	refused := func(sub string) bool {
		return slices.ContainsFunc(e.fake.Refused(), func(r billing.Refusal) bool { return r.Subscription == sub })
	}

	if r := pay("evt_unknown", "ctm_1", "lk_nosuchlink", "sub_unknown"); r.status != http.StatusOK || !refused("sub_unknown") {
		t.Errorf("a payment for an unknown link: %d, refused %v", r.status, e.fake.Refused())
	}

	late := newKey(t)
	lateID := e.startLink(t, late, "")
	forgotten := newKey(t)
	forgottenID := e.startLink(t, forgotten, "")
	e.clock.Add(2 * time.Hour)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r := pay("evt_late", "ctm_late", lateID, "sub_late"); r.status != http.StatusOK || refused("sub_late") {
		t.Errorf("a payment an hour late: %d, refused %v", r.status, e.fake.Refused())
	}
	if _, st := e.poll(t, late, lateID); st != linkActive {
		t.Errorf("the late payer's link is %q, want active", st)
	}
	e.clock.Add(linkForget)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r := pay("evt_forgotten", "ctm_forgotten", forgottenID, "sub_forgotten"); r.status != http.StatusOK || !refused("sub_forgotten") {
		t.Errorf("a payment for a forgotten link: %d, refused %v", r.status, e.fake.Refused())
	}

	// A key denied after it started its link.
	denied := newKey(t)
	deniedID := e.startLink(t, denied, "")
	if err := DenyKey(t.Context(), e.store, pubOf(denied), "abuse", e.now()); err != nil {
		t.Fatal(err)
	}
	if r := pay("evt_denied", "ctm_denied", deniedID, "sub_denied"); r.status != http.StatusOK || !refused("sub_denied") {
		t.Errorf("a payment from a denied key: %d", r.status)
	}

	// One machine that paid three links, the second as the same customer
	// and the third as another: the first links it, the others are refunded,
	// and polling any shows the machine linked. The refunded subscriptions'
	// own later events never reach the account.
	twice := newKey(t)
	one, two, three := e.startLink(t, twice, "twice-paid-01"), e.startLink(t, twice, ""), e.startLink(t, twice, "")
	pay("evt_one", "ctm_twice", one, "sub_one")
	if r := pay("evt_two", "ctm_twice", two, "sub_two"); r.status != http.StatusOK || refused("sub_one") || !refused("sub_two") {
		t.Errorf("a second payment from one machine: %d, refused %v", r.status, e.fake.Refused())
	}
	if r := pay("evt_three", "ctm_other", three, "sub_three"); r.status != http.StatusOK || !refused("sub_three") {
		t.Errorf("a third payment from one machine: %d, refused %v", r.status, e.fake.Refused())
	}
	for _, id := range []string{one, two, three} {
		if _, st := e.poll(t, twice, id); st != linkActive {
			t.Errorf("the machine's link %s is %q, want active", id, st)
		}
	}
	before := e.accountOf(t, "twice-paid-01")
	later := e.now().Add(time.Minute)
	e.webhook(t, withIDs(subscriptionEvent("evt_two_cancelled", "ctm_twice", two, later, later.Add(90*24*time.Hour), "canceled"), "sub_two", ""))
	if a := e.accountOf(t, "twice-paid-01"); a.BillingStatus != "active" || !a.PaidThrough.Equal(before.PaidThrough) {
		t.Errorf("a refunded subscription's cancellation reached the account: %+v", a)
	}

	// If the merchant of record cannot be reached, the webhook fails, so it
	// is sent again, and nothing is recorded meanwhile.
	e.fake.FailRefuse(errors.New("outage"))
	if r := pay("evt_retry", "ctm_2", "lk_nosuchlinkeither", "sub_retry"); r.status != http.StatusServiceUnavailable {
		t.Errorf("a refusal the merchant of record did not take: %d, want 503", r.status)
	}
	e.fake.FailRefuse(nil)
	if r := pay("evt_retry", "ctm_2", "lk_nosuchlinkeither", "sub_retry"); r.status != http.StatusOK || !refused("sub_retry") {
		t.Errorf("the retried webhook: %d, refused %v", r.status, e.fake.Refused())
	}
}

// activations_per_week counts the checkouts that may still become handles,
// so links started under the cap can all be paid; a link paid after its
// hour was not counted, and past the cap it is refunded, not issued.
func TestActivationCapCountsPendingLinks(t *testing.T) {
	e := newEnv(t, true, func(c *Config) { c.ActivationsPerWeek = 2 })
	pay := func(evID, link string) resp {
		now := e.now()
		return e.webhook(t, withIDs(subscriptionEvent(evID, "ctm_"+evID[4:], link, now, now.Add(30*24*time.Hour), "active"), "sub_"+evID[4:], ""))
	}
	a, b, c := newKey(t), newKey(t), newKey(t)
	aID, bID := e.startLink(t, a, ""), e.startLink(t, b, "")
	if r := send(t, e.signed(t, c, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(c)+`"}`))); r.status != http.StatusTooManyRequests {
		t.Errorf("a third link/start with two pending under a cap of two: %d, want 429", r.status)
	}
	pay("evt_a", aID)
	if _, st := e.poll(t, a, aID); st != linkActive {
		t.Errorf("the first link: %q", st)
	}

	e.clock.Add(2 * time.Hour) // b's hour passes unpaid
	cID := e.startLink(t, c, "")
	if r := pay("evt_b", bID); r.status != http.StatusOK || len(e.fake.Refused()) != 1 {
		t.Errorf("a late payment past the cap: %d, refused %v", r.status, e.fake.Refused())
	}
	if r, _ := e.poll(t, b, bID); r.code != "payment_refused" {
		t.Errorf("the late link: %d %s", r.status, r.body)
	}
	pay("evt_c", cID)
	if _, st := e.poll(t, c, cID); st != linkActive {
		t.Errorf("the link started under the cap: %q", st)
	}
}

// PUT /v1/acme-account with the account the records already name writes no
// DNS: one linked machine cannot spend the zone's API quota.
func TestACMEAccountUnchangedWritesNothing(t *testing.T) {
	e := newEnv(t, false, nil)
	key := newKey(t)
	e.link(t, key, "", acme)
	e.dns.SetFail(errors.New("the DNS provider is not to be called"))
	if r := send(t, e.signed(t, key, "PUT", "/v1/acme-account", []byte(`{"uri":"`+acme+`"}`))); r.status != http.StatusNoContent {
		t.Errorf("the same ACME account again: %d %s", r.status, r.body)
	}
	if r := send(t, e.signed(t, key, "PUT", "/v1/acme-account", []byte(`{"uri":"https://acme-v02.api.letsencrypt.org/acme/acct/2"}`))); r.status != http.StatusServiceUnavailable {
		t.Errorf("a new ACME account while DNS fails: %d, want 503", r.status)
	}
}
