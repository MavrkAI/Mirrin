package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/billing"
	"github.com/MavrkAI/Mirrin/cloud/internal/dns"
	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
)

const acme = "https://acme-v02.api.letsencrypt.org/acme/acct/123456789"

// The whole paid journey in --dev mode: link/start, the fake checkout, its
// webhook, a poll that returns an entitlement internal/entitle verifies
// against /v1/keys, and the handle's records in the fake DNS.
func TestDevJourney(t *testing.T) {
	e := newEnv(t, false, nil)
	key := newKey(t)
	_, tok := e.link(t, key, "ember-otter-42", acme)

	ent, dl := e.fetchKeys(t)
	cl, err := entitle.Verify(tok, ent, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entitle.Verify(tok, dl, time.Now()); err == nil {
		t.Error("the entitlement verifies under a deny-list key")
	}
	if k, _ := cl.Key(); !k.Equal(key.Public()) {
		t.Error("cnf is not the device key")
	}
	if cl.Handle != "ember-otter-42" || !slices.Equal(cl.Hosts, []string{"ember-otter-42.mirrin.link"}) || cl.Gen != 1 ||
		cl.Iss != "127.0.0.1" || cl.Aud != entitle.Audience || !cl.Has("reach") || !cl.Has("backup") || len(cl.Relays) != 2 || cl.BackupQuota != 20<<30 {
		t.Errorf("claims: %+v", cl)
	}

	recs, ok := e.dns.Records("ember-otter-42")
	if !ok {
		t.Fatal("no records for the handle")
	}
	want4 := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("198.51.100.1")}
	want6 := []netip.Addr{netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::2")}
	if !slices.Equal(recs.A, want4) || !slices.Equal(recs.AAAA, want6) {
		t.Errorf("A %v AAAA %v", recs.A, recs.AAAA)
	}
	wantCAA := []dns.CAA{
		{Tag: "issue", Value: "letsencrypt.org; accounturi=" + acme + "; validationmethods=tls-alpn-01"},
		{Tag: "issuewild", Value: ";"},
		{Tag: "iodef", Value: "mailto:security@mirrin.app"},
	}
	if !slices.Equal(recs.CAA, wantCAA) {
		t.Errorf("CAA %v, want %v", recs.CAA, wantCAA)
	}

	// A new ACME account rewrites the CAA record.
	other := "https://acme-v02.api.letsencrypt.org/acme/acct/987"
	if r := send(t, e.signed(t, key, "PUT", "/v1/acme-account", []byte(`{"uri":"`+other+`"}`))); r.status != http.StatusNoContent {
		t.Fatalf("PUT /v1/acme-account: %d %s", r.status, r.body)
	}
	recs, _ = e.dns.Records("ember-otter-42")
	if recs.CAA[0].Value != "letsencrypt.org; accounturi="+other+"; validationmethods=tls-alpn-01" {
		t.Errorf("CAA after the change: %v", recs.CAA)
	}
}

// A link without an ACME account gets a CAA record that forbids issuance
// until the daemon names its account.
func TestCAAForbidsIssuanceUntilNamed(t *testing.T) {
	e := newEnv(t, false, nil)
	e.link(t, newKey(t), "quiet-heron-07", "")
	recs, _ := e.dns.Records("quiet-heron-07")
	if recs.CAA[0] != (dns.CAA{Tag: "issue", Value: ";"}) || recs.CAA[1] != (dns.CAA{Tag: "issuewild", Value: ";"}) {
		t.Errorf("CAA %v", recs.CAA)
	}
}

// flipSignature corrupts one bit of a signed request's signature.
func flipSignature(t *testing.T, req *http.Request) {
	t.Helper()
	h := req.Header.Get("Signature")
	label, rest, ok := strings.Cut(h, "=:")
	if !ok {
		t.Fatalf("Signature %q", h)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(rest, ":"))
	if err != nil {
		t.Fatal(err)
	}
	b[0] ^= 1
	req.Header.Set("Signature", label+"=:"+base64.StdEncoding.EncodeToString(b)+":")
}

// Every device route answers 401 unsigned, 401 on a bad signature and 409
// on a replay.
func TestDeviceRoutesRefuseBadSignatures(t *testing.T) {
	type route struct {
		method, path string
		body         func(key ed25519.PrivateKey) []byte
	}
	none := func(ed25519.PrivateKey) []byte { return nil }
	routes := []route{
		{"POST", "/v1/link/start", func(k ed25519.PrivateKey) []byte { return []byte(`{"device_pub":"` + pubOf(k) + `"}`) }},
		{"GET", "/v1/link/{id}", none},
		{"POST", "/v1/entitlement/refresh", none},
		{"PUT", "/v1/acme-account", func(ed25519.PrivateKey) []byte { return []byte(`{"uri":"` + acme + `"}`) }},
		{"GET", "/v1/me", none},
		{"POST", "/v1/billing/portal", none},
		{"DELETE", "/v1/device", none},
		{"DELETE", "/v1/account", none},
		{"POST", "/v1/account/restore", none},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			e := newEnv(t, false, nil)
			key := newKey(t)
			id, _ := e.link(t, key, "", "")
			if rt.path == "/v1/link/start" {
				key = newKey(t) // a new machine
			}
			if rt.path == "/v1/account/restore" {
				send(t, e.signed(t, key, "DELETE", "/v1/account", nil))
			}
			path := strings.Replace(rt.path, "{id}", id, 1)

			if r := send(t, e.request(t, rt.method, path, rt.body(key))); r.status != http.StatusUnauthorized || r.code != "unauthorized" {
				t.Errorf("unsigned: %d %q, want 401 unauthorized", r.status, r.code)
			}
			bad := e.signed(t, key, rt.method, path, rt.body(key))
			flipSignature(t, bad)
			if r := send(t, bad); r.status != http.StatusUnauthorized || r.code != "unauthorized" {
				t.Errorf("bad signature: %d %q, want 401 unauthorized", r.status, r.code)
			}
			stranger := e.signed(t, newKey(t), rt.method, path, rt.body(newKey(t)))
			if r := send(t, stranger); r.status != http.StatusUnauthorized {
				t.Errorf("signed by a key nobody knows: %d, want 401", r.status)
			}
			good := e.signed(t, key, rt.method, path, rt.body(key))
			again := good.Clone(context.Background())
			if good.GetBody != nil {
				again.Body, _ = good.GetBody()
			}
			if r := send(t, good); r.status/100 != 2 {
				t.Fatalf("signed: %d %s", r.status, r.body)
			}
			if r := send(t, again); r.status != http.StatusConflict || r.code != "replay" {
				t.Errorf("replay: %d %q, want 409 replay", r.status, r.code)
			}
		})
	}
}

// webhook sends a Paddle-format event signed with the fake merchant of
// record's secret.
func (e *env) webhook(t *testing.T, ev map[string]any) resp {
	t.Helper()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	req := e.request(t, "POST", "/v1/billing/webhook", body)
	req.Header.Set(billing.SignatureHeader, billing.SignWebhook(e.fake.Secret, e.now(), body))
	return send(t, req)
}

// withIDs sets the subscription and transaction an event names.
func withIDs(ev map[string]any, sub, txn string) map[string]any {
	d := ev["data"].(map[string]any)
	d["id"] = sub
	if txn != "" {
		d["transaction_id"] = txn
	}
	return ev
}

func subscriptionEvent(id, customer, link string, occurred, ends time.Time, status string) map[string]any {
	data := map[string]any{"id": "sub_x", "status": status, "customer_id": customer,
		"current_billing_period": map[string]string{"starts_at": occurred.Format(time.RFC3339Nano), "ends_at": ends.Format(time.RFC3339Nano)}}
	if link != "" {
		data["custom_data"] = map[string]string{"link_id": link}
	}
	return map[string]any{"event_id": id, "event_type": "subscription.updated", "occurred_at": occurred.Format(time.RFC3339Nano), "data": data}
}

// The merchant of record's webhook: its signature is checked, a replayed
// event is a no-op, events apply in the order they happened whatever order
// they arrive in, and the entitlement lasts until min(now+35d,
// paid_through+7d).
func TestWebhook(t *testing.T) {
	e := newEnv(t, true, nil)
	now := e.now()
	key := newKey(t)

	// A checkout paid by webhook, not the dev page.
	r := send(t, e.signed(t, key, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(key)+`","handle":"brisk-wren-11"}`)))
	var ls struct {
		ID string `json:"id"`
	}
	r.json(t, &ls)
	paid := now.Add(30 * 24 * time.Hour)
	ev := subscriptionEvent("evt_first", "ctm_owner", ls.ID, now, paid, "active")

	body, _ := json.Marshal(ev)
	forged := e.request(t, "POST", "/v1/billing/webhook", body)
	forged.Header.Set(billing.SignatureHeader, billing.SignWebhook([]byte("not the secret"), now, body))
	if r := send(t, forged); r.status != http.StatusUnauthorized {
		t.Errorf("forged webhook: %d, want 401", r.status)
	}
	stale := e.request(t, "POST", "/v1/billing/webhook", body)
	stale.Header.Set(billing.SignatureHeader, billing.SignWebhook(e.fake.Secret, now.Add(-time.Hour), body))
	if r := send(t, stale); r.status != http.StatusUnauthorized {
		t.Errorf("webhook signed an hour ago: %d, want 401", r.status)
	}
	unsigned := e.request(t, "POST", "/v1/billing/webhook", body)
	if r := send(t, unsigned); r.status != http.StatusUnauthorized {
		t.Errorf("unsigned webhook: %d, want 401", r.status)
	}
	if r := e.webhook(t, ev); r.status != http.StatusOK {
		t.Fatalf("webhook: %d %s", r.status, r.body)
	}

	claims := func() (entitle.Claims, resp) {
		t.Helper()
		r := send(t, e.signed(t, key, "POST", "/v1/entitlement/refresh", nil))
		if r.status != http.StatusOK {
			return entitle.Claims{}, r
		}
		var out struct {
			Entitlement string `json:"entitlement"`
		}
		r.json(t, &out)
		ent, _ := e.fetchKeys(t)
		cl, err := entitle.Verify(out.Entitlement, ent, e.now())
		if err != nil {
			t.Fatal(err)
		}
		return cl, r
	}
	cl, r := claims()
	if r.status != http.StatusOK || !cl.PaidThrough.Equal(paid.Truncate(time.Second)) || !cl.Exp.Equal(now.Add(35*24*time.Hour)) {
		t.Fatalf("after paying: %d %+v", r.status, cl)
	}

	// A replay of the same event changes nothing, even after a newer one.
	later := now.Add(time.Minute)
	if r := e.webhook(t, subscriptionEvent("evt_second", "ctm_owner", "", later, now.Add(10*24*time.Hour), "canceled")); r.status != 200 {
		t.Fatal(r.status)
	}
	if r := e.webhook(t, ev); r.status != http.StatusOK {
		t.Fatalf("replayed webhook: %d", r.status)
	}
	cl, _ = claims()
	if want := now.Add(10 * 24 * time.Hour); !cl.PaidThrough.Equal(want) || !cl.Exp.Equal(want.Add(7*24*time.Hour)) {
		t.Errorf("after a cancel: paid_through %s exp %s, want %s and a week later", cl.PaidThrough, cl.Exp, want)
	}

	// An older event arriving late does not undo a newer one.
	if r := e.webhook(t, subscriptionEvent("evt_older", "ctm_owner", "", now.Add(30*time.Second), now.Add(60*24*time.Hour), "active")); r.status != 200 {
		t.Fatal(r.status)
	}
	cl, _ = claims()
	if !cl.PaidThrough.Equal(now.Add(10 * 24 * time.Hour)) {
		t.Errorf("an older event moved paid_through to %s", cl.PaidThrough)
	}

	// A subscription in dunning reports a period it has not paid for.
	if r := e.webhook(t, subscriptionEvent("evt_dunning", "ctm_owner", "", now.Add(2*time.Minute), now.Add(40*24*time.Hour), "past_due")); r.status != 200 {
		t.Fatal(r.status)
	}
	cl, _ = claims()
	if !cl.PaidThrough.Equal(now.Add(10 * 24 * time.Hour)) {
		t.Errorf("a past_due event moved paid_through to %s", cl.PaidThrough)
	}

	// exp = min(now+35d, paid_through+7d) as the clock moves.
	e.clock.Add(5 * 24 * time.Hour)
	cl, _ = claims()
	if !cl.Exp.Equal(now.Add(17 * 24 * time.Hour)) {
		t.Errorf("exp %s, want paid_through+7d", cl.Exp)
	}
	e.clock.Add(6 * 24 * time.Hour) // past paid_through
	if _, r := claims(); r.status != http.StatusPaymentRequired || r.code != "lapsed" {
		t.Errorf("refresh after paid_through: %d %q, want 402 lapsed", r.status, r.code)
	}
}

// Handles: the character rules, reserved names and look-alikes, and a
// released handle is never given to anyone else.
func TestHandleRules(t *testing.T) {
	for _, h := range []string{"abc", "ember-otter-42", "a-b", strings.Repeat("x", 32), "0ne-2-three"} {
		if err := allowedHandle(h); err != nil {
			t.Errorf("%q refused: %v", h, err)
		}
	}
	for _, h := range []string{"ab", strings.Repeat("x", 33), "-abc", "abc-", "a--b", "xn--abc", "ABC", "a_b", "a.b", "a b", "ünï", ""} {
		if err := checkHandle(h); err == nil {
			t.Errorf("%q passes the character rules", h)
		}
	}
	for _, h := range []string{"www", "admin", "adm1n", "w-w-w", "5upport", "g00gle-fan", "my-paypa1", "rnavrk", "mirrin-help", "5ecur1ty", "vvww"} {
		if err := allowedHandle(h); !errors.Is(err, errReservedHandle) {
			t.Errorf("%q: %v, want reserved", h, err)
		}
	}
	for _, a := range adjectives {
		for _, n := range nouns {
			if h := a + "-" + n + "-00"; allowedHandle(h) != nil {
				t.Errorf("random handle %q breaks the rules", h)
			}
		}
	}
	for range 100 {
		if h := randomHandle(); allowedHandle(h) != nil {
			t.Errorf("random handle %q breaks the rules", h)
		}
	}

	e := newEnv(t, true, nil)
	// A pending link holds its handle, and its look-alikes, from others.
	pending := newKey(t)
	if r := send(t, e.signed(t, pending, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(pending)+`","handle":"coral-seal-55"}`))); r.status != 200 {
		t.Fatalf("link/start: %d", r.status)
	}
	for _, h := range []string{"coral-seal-55", "cora1-seal-55", "coral-sea1-5-5"} {
		k := newKey(t)
		r := send(t, e.signed(t, k, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(k)+`","handle":"`+h+`"}`)))
		if r.status != http.StatusConflict || r.code != "handle_taken" {
			t.Errorf("%q beside a pending coral-seal-55: %d %q, want 409 handle_taken", h, r.status, r.code)
		}
	}
	key := newKey(t)
	e.link(t, key, "ember-otter-42", "")
	for _, h := range []string{"ember-otter-42", "ember-0tter-42", "emberotter42", "ember-otter-4-2"} {
		k := newKey(t)
		r := send(t, e.signed(t, k, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(k)+`","handle":"`+h+`"}`)))
		if r.status != http.StatusConflict || r.code != "handle_taken" {
			t.Errorf("%q beside ember-otter-42: %d %q, want 409 handle_taken", h, r.status, r.code)
		}
	}

	// Delete the account, let the week pass, and the name is still taken.
	send(t, e.signed(t, key, "DELETE", "/v1/account", nil))
	e.clock.Add(deleteWait + time.Minute)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	k := newKey(t)
	r := send(t, e.signed(t, k, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(k)+`","handle":"ember-otter-42"}`)))
	if r.status != http.StatusConflict || r.code != "handle_taken" {
		t.Errorf("a released handle: %d %q, want 409 handle_taken", r.status, r.code)
	}
	err := e.store.View(t.Context(), func(tx *store.Tx) error {
		taken, err := e.s.handleTaken(tx, "ember-otter-42", "", e.now())
		if !taken {
			t.Error("the random picker would reuse a released handle")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// DELETE /v1/account waits seven days; then the rows, the DNS records and
// the keys' standing go, and the keys and handle are on the deny list.
func TestAccountDeletion(t *testing.T) {
	e := newEnv(t, true, nil)
	key := newKey(t)
	e.link(t, key, "sandy-moth-19", acme)
	r := send(t, e.signed(t, key, "DELETE", "/v1/account", nil))
	if r.status != http.StatusAccepted {
		t.Fatalf("DELETE /v1/account: %d", r.status)
	}

	e.clock.Add(deleteWait - time.Minute)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r := send(t, e.signed(t, key, "POST", "/v1/entitlement/refresh", nil)); r.status != http.StatusOK {
		t.Errorf("refresh inside the undo window: %d", r.status)
	}
	if _, ok := e.dns.Records("sandy-moth-19"); !ok {
		t.Error("records gone inside the undo window")
	}
	var account string
	e.store.View(t.Context(), func(tx *store.Tx) error {
		h, err := tx.Handle("sandy-moth-19")
		account = h.Account
		return err
	})

	e.clock.Add(2 * time.Minute)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.dns.Records("sandy-moth-19"); ok {
		t.Error("DNS records remain after deletion")
	}
	err := e.store.View(t.Context(), func(tx *store.Tx) error {
		tables, err := tx.Schema()
		if err != nil {
			return err
		}
		for _, tb := range tables {
			if !store.AccountLinked(tb) {
				continue
			}
			col := "account"
			if tb.Name == "accounts" {
				col = "id"
			}
			rows, err := tx.Rows(tb.Name, col, account)
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				t.Errorf("%s still holds %d rows of the deleted account", tb.Name, len(rows))
			}
		}
		h, err := tx.Handle("sandy-moth-19")
		if err != nil || h.Released.IsZero() || h.Account != "" {
			t.Errorf("handle after deletion: %+v %v", h, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	l := e.denyList(t)
	if !l.DeniesKey(key.Public().(ed25519.PublicKey)) || !l.DeniesHandle("sandy-moth-19") {
		t.Errorf("deny list after deletion: %+v", l)
	}
	if c := e.fake.Canceled(); len(c) != 1 || !strings.HasPrefix(c[0], "ctm_") {
		t.Errorf("billing stopped for %v, want the account's customer", c)
	}
	for _, rt := range [][2]string{{"POST", "/v1/entitlement/refresh"}, {"GET", "/v1/me"}, {"POST", "/v1/account/restore"}} {
		if r := send(t, e.signed(t, key, rt[0], rt[1], nil)); r.status != http.StatusForbidden || r.code != "deleted" {
			t.Errorf("%s %s after deletion: %d %q, want 403 deleted", rt[0], rt[1], r.status, r.code)
		}
	}
}

// A DNS outage during a link leaves the records pending; the sweep writes
// them once the provider is back, and the link works meanwhile.
func TestDNSRetry(t *testing.T) {
	e := newEnv(t, false, nil)
	e.dns.SetFail(errors.New("outage"))
	e.link(t, newKey(t), "misty-lynx-03", "")
	if _, ok := e.dns.Records("misty-lynx-03"); ok {
		t.Fatal("records written during an outage")
	}
	if err := e.s.Sweep(t.Context()); err == nil {
		t.Error("sweep reports no failure during the outage")
	}
	e.dns.SetFail(nil)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.dns.Records("misty-lynx-03"); !ok {
		t.Error("records not written after the outage")
	}
}

// Only a dev server has the dev routes.
func TestProductionHasNoDevRoutes(t *testing.T) {
	e := newEnv(t, false, nil)
	cfg := DevConfig()
	cfg.PublicURL = "https://cloud.example"
	prod, err := New(cfg, Options{Store: e.store, Billing: e.fake, DNS: e.dns, Keys: e.s.keys})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, Options{Store: e.store, Billing: e.fake, DevBilling: e.fake, DNS: e.dns, Keys: e.s.keys}); err == nil {
		t.Error("a production server took the fake merchant of record")
	}
	if _, err := New(DevConfig(), Options{Store: e.store, Billing: e.fake, DNS: e.dns, Keys: e.s.keys}); err == nil {
		t.Error("a production server took a plain-http origin")
	}
	for _, p := range []string{"/dev/revoke", "/dev/supersede", "/dev/paid-through", "/checkout/lk_x", "/portal/ctm_x"} {
		for _, m := range []string{"GET", "POST"} {
			req, _ := http.NewRequest(m, "https://cloud.example"+p, strings.NewReader("{}"))
			rec := &discard{h: http.Header{}}
			prod.Handler().ServeHTTP(rec, req)
			if rec.status != http.StatusNotFound {
				t.Errorf("%s %s on a production server: %d, want 404", m, p, rec.status)
			}
		}
	}
}

// link/start is limited per client address, and new handles per week.
func TestLimits(t *testing.T) {
	e := newEnv(t, false, func(c *Config) { c.LinkStartsPerMinute = 3 })
	for i := range 4 {
		k := newKey(t)
		r := send(t, e.signed(t, k, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(k)+`"}`)))
		if want := i < 3; (r.status == 200) != want {
			t.Errorf("link/start %d: %d", i, r.status)
		}
		if i == 3 && (r.status != http.StatusTooManyRequests || r.code != "rate_limited") {
			t.Errorf("fourth link/start: %d %q, want 429 rate_limited", r.status, r.code)
		}
	}

	e = newEnv(t, false, func(c *Config) { c.ActivationsPerWeek = 1 })
	e.link(t, newKey(t), "", "")
	k := newKey(t)
	if r := send(t, e.signed(t, k, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(k)+`"}`))); r.status != http.StatusTooManyRequests {
		t.Errorf("link/start past the weekly cap: %d, want 429", r.status)
	}
}

// A dev server's deny list and keys: purposes kept apart, seq only rises.
func TestDenyListSeq(t *testing.T) {
	e := newEnv(t, true, nil)
	first := e.denyList(t)
	key := newKey(t)
	e.link(t, key, "", "")
	send(t, e.signed(t, key, "DELETE", "/v1/device", nil))
	e.clock.Add(10 * time.Second)
	second := e.denyList(t)
	if second.Seq <= first.Seq || !second.DeniesKey(key.Public().(ed25519.PublicKey)) {
		t.Errorf("after an unlink: seq %d → %d, %+v", first.Seq, second.Seq, second)
	}
	// Entries leave the published list once no entitlement they could
	// cancel is left, and the seq rises again.
	e.clock.Add(denyKeep + time.Hour)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	third := e.denyList(t)
	if third.Seq <= second.Seq || third.DeniesKey(key.Public().(ed25519.PublicKey)) {
		t.Errorf("after the window: seq %d → %d, %+v", second.Seq, third.Seq, third)
	}
	// The key still hears why it is refused.
	if r := send(t, e.signed(t, key, "GET", "/v1/me", nil)); r.status != http.StatusForbidden || r.code != "revoked" {
		t.Errorf("an unlinked key: %d %q", r.status, r.code)
	}
}

// Admin deny: a denied handle gets no new entitlement and is on the list;
// a denied key is revoked.
func TestAdminDeny(t *testing.T) {
	e := newEnv(t, false, nil)
	a, b := newKey(t), newKey(t)
	e.link(t, a, "tawny-owl-31", "")
	e.link(t, b, "", "")
	if err := DenyHandle(t.Context(), e.store, "tawny-owl-31", "abuse", time.Now()); err != nil {
		t.Fatal(err)
	}
	if r := send(t, e.signed(t, a, "POST", "/v1/entitlement/refresh", nil)); r.status != http.StatusForbidden {
		t.Errorf("refresh for a denied handle: %d", r.status)
	}
	if err := DenyKey(t.Context(), e.store, pubOf(b), "abuse", time.Now()); err != nil {
		t.Fatal(err)
	}
	if r := send(t, e.signed(t, b, "POST", "/v1/entitlement/refresh", nil)); r.status != http.StatusForbidden || r.code != "revoked" {
		t.Errorf("refresh for a denied key: %d %q", r.status, r.code)
	}
	l := e.denyList(t)
	if !l.DeniesHandle("tawny-owl-31") || !l.DeniesKey(b.Public().(ed25519.PublicKey)) {
		t.Errorf("deny list: %+v", l)
	}
	doc, err := Show(t.Context(), e.store, "tawny-owl-31", time.Now())
	if err != nil || doc["deny"] == nil || doc["account"] == nil {
		t.Errorf("admin show: %v %v", doc, err)
	}
	if _, has := doc["account"].(map[string]any)["email"]; has {
		t.Error("admin show fetched the email")
	}
}
