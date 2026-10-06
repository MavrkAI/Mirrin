package server

import (
	"crypto/ed25519"
	"net/http"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

// accountUntouched fails if the handle's CAA record no longer names acme or
// its account is being deleted.
func (e *env) accountUntouched(t *testing.T, handle string) {
	t.Helper()
	recs, _ := e.dns.Records(handle)
	if len(recs.CAA) == 0 || recs.CAA[0].Value != "letsencrypt.org; accounturi="+acme+"; validationmethods=tls-alpn-01" {
		t.Errorf("CAA rewritten: %v", recs.CAA)
	}
	e.store.View(t.Context(), func(tx *store.Tx) error {
		h, err := tx.Handle(handle)
		if err != nil {
			t.Fatal(err)
		}
		a, err := tx.Account(h.Account)
		if err != nil {
			t.Fatal(err)
		}
		if !a.DeleteAt.IsZero() {
			t.Errorf("the account is being deleted, at %s", a.DeleteAt)
		}
		return nil
	})
}

// A machine superseded at its handle, as by a recovery elsewhere, may not
// read or change the account; it may still unlink itself. A key on the deny
// list may do nothing, even while its device row stands unrevoked, which is
// how a recovery leaves the old machine's key.
func TestSupersededOrDeniedDevicesMayNotAct(t *testing.T) {
	const other = `{"uri":"https://acme-v02.api.letsencrypt.org/acme/acct/666"}`
	type route struct{ method, path, body string }
	account := []route{
		{"PUT", "/v1/acme-account", other},
		{"GET", "/v1/me", ""},
		{"POST", "/v1/billing/portal", ""},
		{"DELETE", "/v1/account", ""},
		{"POST", "/v1/account/restore", ""},
	}
	body := func(s string) []byte {
		if s == "" {
			return nil
		}
		return []byte(s)
	}
	for _, rt := range account {
		t.Run("superseded "+rt.method+" "+rt.path, func(t *testing.T) {
			e := newEnv(t, false, nil)
			key := newKey(t)
			e.link(t, key, "old-mac-01", acme)
			if r := e.dev(t, "/dev/supersede", `{"handle":"old-mac-01"}`); r.status != http.StatusOK {
				t.Fatalf("supersede: %d %s", r.status, r.body)
			}
			r := send(t, e.signed(t, key, rt.method, rt.path, body(rt.body)))
			var sup struct {
				Gen int64     `json:"gen"`
				At  time.Time `json:"at"`
			}
			r.json(t, &sup)
			if r.status != http.StatusConflict || r.code != "superseded" || sup.Gen != 2 || sup.At.IsZero() {
				t.Errorf("%d %s, want 409 superseded at generation 2", r.status, r.body)
			}
			e.accountUntouched(t, "old-mac-01")
		})
	}
	t.Run("superseded DELETE /v1/device", func(t *testing.T) {
		e := newEnv(t, false, nil)
		key := newKey(t)
		e.link(t, key, "old-mac-02", acme)
		e.dev(t, "/dev/supersede", `{"handle":"old-mac-02"}`)
		if r := send(t, e.signed(t, key, "DELETE", "/v1/device", nil)); r.status != http.StatusNoContent {
			t.Errorf("a superseded machine unlinking itself: %d %s", r.status, r.body)
		}
	})

	all := append([]route{{"POST", "/v1/entitlement/refresh", ""}, {"DELETE", "/v1/device", ""}, {"GET", "/v1/link/{id}", ""}}, account...)
	for _, rt := range all {
		t.Run("denied "+rt.method+" "+rt.path, func(t *testing.T) {
			e := newEnv(t, false, nil)
			key := newKey(t)
			id, _ := e.link(t, key, "old-mac-03", acme)
			pub := key.Public().(ed25519.PublicKey)
			err := e.store.Update(t.Context(), func(tx *store.Tx) error {
				_, err := tx.Deny(store.DenyEntry{Kind: store.DenyKey, Value: pubOf(key), KeyID: httpsig.KeyID(pub), Why: "recovered", Created: time.Now()})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			path := rt.path
			if path == "/v1/link/{id}" {
				path = "/v1/link/" + id
			}
			if r := send(t, e.signed(t, key, rt.method, path, body(rt.body))); r.status != http.StatusForbidden || r.code != "revoked" {
				t.Errorf("%d %s, want 403 revoked", r.status, r.body)
			}
			e.accountUntouched(t, "old-mac-03")
		})
	}
}

// Keys anyone can mint reach only link/start and link polls, and their
// nonces are kept apart from devices': strangers who fill theirs leave every
// linked machine working.
func TestStrangersCannotStarveDevices(t *testing.T) {
	e := newEnvWith(t, false, nil, func(s *Server) { s.otherNonces = httpsig.NewMemoryNonceCache(20, 400) })
	key := newKey(t)
	e.link(t, key, "", "") // one nonce of a stranger's: its link/start
	pending := newKey(t)
	id := e.startLink(t, pending, "")
	if r := send(t, e.signed(t, pending, "GET", "/v1/me", nil)); r.status != http.StatusUnauthorized {
		t.Errorf("a pending link's key on a device route: %d, want 401", r.status)
	}
	devices := e.s.nonces.Len()
	for range 30 {
		k := newKey(t)
		send(t, e.signed(t, k, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(k)+`","handle":"x"}`)))
		send(t, e.signed(t, pending, "GET", "/v1/link/"+id, nil))
	}
	k := newKey(t)
	if r := send(t, e.signed(t, k, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(k)+`"}`))); r.status != http.StatusServiceUnavailable {
		t.Errorf("link/start once strangers filled their cache: %d, want 503", r.status)
	}
	if n := e.s.nonces.Len(); n != devices {
		t.Errorf("strangers put %d nonces in the devices' cache", n-devices)
	}
	for range 3 {
		if r := send(t, e.signed(t, key, "POST", "/v1/entitlement/refresh", nil)); r.status != http.StatusOK {
			t.Errorf("a linked machine's refresh while strangers fill their cache: %d %s", r.status, r.body)
		}
	}
}

// link/start is limited per client once its signature checks out, so junk
// costs nobody else anything; clients behind a trusted proxy are told apart
// by X-Forwarded-For; everyone together has a cap; and the link routes have
// a looser limit before signatures are checked.
func TestLinkStartLimits(t *testing.T) {
	e := newEnv(t, false, func(c *Config) {
		c.LinkStartsPerMinute, c.LinkStartsTotalPerMinute = 3, 5
		c.TrustedProxies = []string{"127.0.0.1", "::1"}
	})
	from := func(req *http.Request, client string) *http.Request {
		req.Header.Set("X-Forwarded-For", client)
		return req
	}
	start := func(client string) resp {
		k := newKey(t)
		return send(t, from(e.signed(t, k, "POST", "/v1/link/start", []byte(`{"device_pub":"`+pubOf(k)+`"}`)), client))
	}
	for range 10 {
		junk := from(e.request(t, "POST", "/v1/link/start", []byte(`{"device_pub":"x"}`)), "198.51.100.1")
		if r := send(t, junk); r.status != http.StatusUnauthorized {
			t.Fatalf("unsigned link/start: %d", r.status)
		}
	}
	for i := range 3 {
		if r := start("198.51.100.1"); r.status != http.StatusOK {
			t.Errorf("link/start %d after junk from the same client: %d %s", i, r.status, r.body)
		}
	}
	if r := start("198.51.100.1"); r.status != http.StatusTooManyRequests || r.code != "rate_limited" {
		t.Errorf("a fourth link/start in a minute: %d %q", r.status, r.code)
	}
	if r := start("198.51.100.2"); r.status != http.StatusOK {
		t.Errorf("another client behind the proxy: %d", r.status)
	}
	if r := start("2001:db8:1:1::1"); r.status != http.StatusOK {
		t.Errorf("an IPv6 client: %d", r.status)
	}
	if r := start("2001:db8:2::1"); r.status != http.StatusTooManyRequests {
		t.Errorf("link/start past the cap for everyone: %d", r.status)
	}

	// Before any signature is checked, a client may send the link routes
	// what linking takes, and no more.
	e = newEnv(t, false, func(c *Config) { c.LinkStartsPerMinute = 3; c.TrustedProxies = []string{"127.0.0.1"} })
	var last resp
	for range preAuthPerMinute + 1 {
		last = send(t, from(e.request(t, "GET", "/v1/link/lk_nosuchlink", nil), "203.0.113.50"))
	}
	if last.status != http.StatusTooManyRequests {
		t.Errorf("request %d from one client: %d, want 429", preAuthPerMinute+1, last.status)
	}
	if r := start("203.0.113.51"); r.status != http.StatusOK {
		t.Errorf("another client: %d", r.status)
	}
}

// The signed device routes are limited per device key.
func TestDeviceRequestLimit(t *testing.T) {
	e := newEnv(t, false, func(c *Config) { c.DeviceRequestsPerMinute = 3 })
	a, b := newKey(t), newKey(t)
	e.link(t, a, "", "")
	e.link(t, b, "", "")
	for i := range 3 {
		if r := send(t, e.signed(t, a, "GET", "/v1/me", nil)); r.status != http.StatusOK {
			t.Errorf("request %d: %d", i, r.status)
		}
	}
	if r := send(t, e.signed(t, a, "PUT", "/v1/acme-account", []byte(`{"uri":"`+acme+`"}`))); r.status != http.StatusTooManyRequests || r.code != "rate_limited" {
		t.Errorf("a fourth request in a minute: %d %q", r.status, r.code)
	}
	if r := send(t, e.signed(t, b, "GET", "/v1/me", nil)); r.status != http.StatusOK {
		t.Errorf("another machine: %d", r.status)
	}
}
