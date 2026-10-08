// Package cloudtest is a fake Mirrin Cloud control plane, and the contract
// suite that the fake and the real control plane (cloud/, in --dev mode)
// must both pass. The fake implements docs/cloud-api.md in memory on a
// loopback httptest server, so the whole paid journey can be tested without
// a network, a card or a DNS provider.
package cloudtest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

// The fake signs with the public development keys: SHA-256("mirrin dev key
// " + kid) as the Ed25519 seed. Builds tagged mirrin_devkeys trust them;
// release builds never do.
const (
	EntitlementKid = "ent-dev-a"
	DenyListKid    = "dl-dev-a"
)

// Lifetimes the fake uses, as the real control plane does.
const (
	linkTTL      = time.Hour
	entMax       = 35 * 24 * time.Hour // an entitlement lasts at most this
	entGrace     = 7 * 24 * time.Hour  // and at most this past paid_through
	deleteWait   = 7 * 24 * time.Hour
	maxBody      = 64 << 10
	signingSkew  = 300 * time.Second
	defaultQuota = 20 << 30
)

// Request is one request the fake answered.
type Request struct {
	Method, Path string
	KeyID        string // the signature's key id; "" when unsigned or refused
	Status       int
}

// Fake is an in-memory control plane. Its methods drive what a real one
// would learn from the merchant of record or an admin: payment, lapse,
// another machine taking over a handle.
type Fake struct {
	// URL is the fake's origin, http://127.0.0.1:port. Pass it to cloud.New.
	URL string

	srv    *httptest.Server
	nonces *httpsig.MemoryNonceCache

	mu       sync.Mutex
	now      func() time.Time
	period   time.Duration
	relays   []entitle.Relay
	tamper   func(*entitle.Claims)
	links    map[string]*link
	linkKeys map[string]ed25519.PublicKey // key id → key, for links not yet paid
	devices  map[string]*device           // key id → device
	handles  map[string]*handle
	denied   []entitle.Entry
	denySeq  int64
	requests []Request
	down     atomic.Bool // every request gets 503
}

type account struct {
	id, customer string
	paidThrough  time.Time
	created      time.Time
	acme         string
	deleteAt     time.Time
	handle       *handle
	events       []event
}

type event struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
}

type device struct {
	pub     ed25519.PublicKey
	keyID   string
	account *account
	gen     int64 // the handle generation this device linked or recovered at
	created time.Time
	seen    time.Time
	revoked bool
}

type handle struct {
	name    string
	gen     int64
	genAt   time.Time
	created time.Time
}

type link struct {
	id      string
	pub     ed25519.PublicKey
	handle  string
	acme    string
	status  string
	expires time.Time
	account *account
}

// NewFake starts a fake control plane that closes when t ends.
func NewFake(t testing.TB) *Fake {
	f := &Fake{
		nonces:   httpsig.NewMemoryNonceCache(0, 0),
		now:      time.Now,
		period:   30 * 24 * time.Hour,
		links:    map[string]*link{},
		linkKeys: map[string]ed25519.PublicKey{},
		devices:  map[string]*device{},
		handles:  map[string]*handle{},
	}
	f.srv = httptest.NewServer(f.routes())
	f.URL = f.srv.URL
	f.relays = []entitle.Relay{
		{ID: "r1", URL: "wss://r1.relay." + cloud.OperatorZone + "/v1/tunnel", IPs: []string{"192.0.2.1", "2001:db8::1"}},
		{ID: "r2", URL: "wss://r2.relay." + cloud.OperatorZone + "/v1/tunnel", IPs: []string{"198.51.100.1", "2001:db8::2"}},
	}
	t.Cleanup(f.srv.Close)
	return f
}

// Handler is the fake's HTTP handler, for answering requests without a
// socket, as a test with a fake clock (testing/synctest) must. Requests must
// name f.URL's host, which signatures cover.
func (f *Fake) Handler() http.Handler { return f.srv.Config.Handler }

// DevKey is the development private key for kid.
func DevKey(kid string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("mirrin dev key " + kid))
	return ed25519.NewKeyFromSeed(seed[:])
}

// Keys are the entitlement keys the fake signs with, for cloud.New.
func (f *Fake) Keys() map[string]ed25519.PublicKey {
	return map[string]ed25519.PublicKey{EntitlementKid: DevKey(EntitlementKid).Public().(ed25519.PublicKey)}
}

// DenyListKeys are the deny-list keys the fake signs with.
func (f *Fake) DenyListKeys() map[string]ed25519.PublicKey {
	return map[string]ed25519.PublicKey{DenyListKid: DevKey(DenyListKid).Public().(ed25519.PublicKey)}
}

// SetClock replaces the fake's clock, which dates entitlements and checks
// signatures.
func (f *Fake) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// SetRelays sets the relays named in entitlements from now on.
func (f *Fake) SetRelays(rs []entitle.Relay) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relays = slices.Clone(rs)
}

// Tamper changes the claims of every entitlement signed from now on, as a
// broken or hostile control plane might. Nil stops it.
func (f *Fake) Tamper(fn func(*entitle.Claims)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tamper = fn
}

// Pay completes the checkout for link id, as the merchant of record's
// webhook would. A GET of the link's checkout_url does the same.
func (f *Fake) Pay(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pay(id)
}

// ExpireLink ends a pending link unpaid, as its hour running out would.
func (f *Fake) ExpireLink(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.links[id]
	if !ok || l.status != cloud.LinkPending {
		return errors.New("cloudtest: no pending link " + id)
	}
	l.status = cloud.LinkExpired
	return nil
}

// SetPaidThrough moves the end of the paid period for handle's account.
func (f *Fake) SetPaidThrough(name string, t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.devices {
		if d.account.handle != nil && d.account.handle.name == name {
			d.account.paidThrough = t.UTC()
			return nil
		}
	}
	return fmt.Errorf("cloudtest: no account holds %q", name)
}

// Supersede raises handle's generation, as a recovery on another machine
// would, and returns the new generation. Devices at the old one get 409.
func (f *Fake) Supersede(name string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.handles[name]
	if !ok {
		return 0, fmt.Errorf("cloudtest: no handle %q", name)
	}
	h.gen++
	h.genAt = f.now().UTC().Truncate(time.Second)
	return h.gen, nil
}

// Revoke revokes a device key, as an admin or a recovery would, and puts
// it on the deny list.
func (f *Fake) Revoke(pub ed25519.PublicKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[httpsig.KeyID(pub)]
	if !ok {
		return errors.New("cloudtest: unknown device")
	}
	f.revoke(d, "revoked")
	return nil
}

// Requests returns every request answered so far, oldest first.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// ACMEAccount is the ACME account the handle's CAA record would pin: the
// one given at link, or the last PUT /v1/acme-account. It reports false
// when no account holds the handle.
func (f *Fake) ACMEAccount(handle string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.devices {
		if h := d.account.handle; h != nil && h.name == handle && !d.revoked {
			return d.account.acme, true
		}
	}
	return "", false
}

// SetDown makes the fake answer every request 503 (still recorded), as a
// control plane that can't be reached for a while, or answer again.
func (f *Fake) SetDown(down bool) { f.down.Store(down) }

func (f *Fake) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", f.version)
	mux.HandleFunc("GET /v1/keys", f.keys)
	mux.HandleFunc("GET /v1/denylist", f.denylist)
	mux.HandleFunc("POST /v1/link/start", f.signed(f.linkStart))
	mux.HandleFunc("GET /v1/link/{id}", f.signed(f.linkPoll))
	mux.HandleFunc("POST /v1/entitlement/refresh", f.signed(f.device(f.refresh)))
	mux.HandleFunc("PUT /v1/acme-account", f.signed(f.device(f.acmeAccount)))
	mux.HandleFunc("GET /v1/me", f.signed(f.device(f.me)))
	mux.HandleFunc("POST /v1/billing/portal", f.signed(f.device(f.portal)))
	mux.HandleFunc("DELETE /v1/device", f.signed(f.device(f.unlink)))
	mux.HandleFunc("DELETE /v1/account", f.signed(f.device(f.deleteAccount)))
	mux.HandleFunc("POST /v1/account/restore", f.signed(f.device(f.restore)))
	// Dev mode: the checkout and billing pages the browser would open, and
	// the controls the contract suite drives (docs/cloud-api.md §7).
	mux.HandleFunc("GET /checkout/{id}", f.checkout)
	mux.HandleFunc("GET /portal/{customer}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Fake billing portal.\n")
	})
	mux.HandleFunc("POST /dev/paid-through", f.devPaidThrough)
	mux.HandleFunc("POST /dev/supersede", f.devSupersede)
	mux.HandleFunc("POST /dev/revoke", f.devRevoke)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "not_found", "No such route.") })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		if f.down.Load() {
			fail(rec, http.StatusServiceUnavailable, "unavailable", "The service is down for a moment.")
		} else {
			mux.ServeHTTP(rec, r)
		}
		f.mu.Lock()
		f.requests = append(f.requests, Request{Method: r.Method, Path: r.URL.Path, KeyID: rec.keyID, Status: rec.status})
		f.mu.Unlock()
	})
}

// recorder notes the status, and the key id a signed handler accepted.
type recorder struct {
	http.ResponseWriter
	status int
	keyID  string
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// signedHandler handles a request whose signature has been checked. body
// is the request body, keyID the key that signed it.
type signedHandler func(w http.ResponseWriter, r *http.Request, keyID string, body []byte)

// signed checks the RFC 9421 signature, then calls h holding f.mu. The key
// comes from a registered device, a pending link, or, for link/start only,
// the device_pub in the body (proof of possession).
func (f *Fake) signed(h signedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			fail(w, http.StatusRequestEntityTooLarge, "too_large", "The request body is too large.")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var claimed ed25519.PublicKey
		if r.URL.Path == "/v1/link/start" {
			var in struct {
				DevicePub string `json:"device_pub"`
			}
			if json.Unmarshal(body, &in) == nil {
				claimed, _ = entitle.ParseKey(in.DevicePub)
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		lookup := func(id string) (ed25519.PublicKey, error) {
			if d, ok := f.devices[id]; ok {
				return d.pub, nil
			}
			if k, ok := f.linkKeys[id]; ok {
				return k, nil
			}
			if claimed != nil && id == httpsig.KeyID(claimed) {
				return claimed, nil
			}
			return nil, errors.New("unknown key")
		}
		keyID, err := httpsig.VerifyFor(r, []string{f.URL}, lookup, f.now(), signingSkew, f.nonces)
		switch {
		case errors.Is(err, httpsig.ErrReplay):
			fail(w, http.StatusConflict, "replay", "This signed request was already used.")
			return
		case errors.Is(err, httpsig.ErrNonceCacheFull):
			fail(w, http.StatusServiceUnavailable, "busy", "Try again shortly.")
			return
		case err != nil:
			fail(w, http.StatusUnauthorized, "unauthorized", "The request signature was not accepted.")
			return
		}
		if rec, ok := w.(*recorder); ok {
			rec.keyID = keyID
		}
		h(w, r, keyID, body)
	}
}

// device narrows a signed handler to registered devices, and records the
// day each was last seen (the only activity the control plane keeps).
func (f *Fake) device(h func(w http.ResponseWriter, r *http.Request, d *device, body []byte)) signedHandler {
	return func(w http.ResponseWriter, r *http.Request, keyID string, body []byte) {
		d, ok := f.devices[keyID]
		if !ok {
			fail(w, http.StatusUnauthorized, "unauthorized", "This device is not linked.")
			return
		}
		if d.revoked {
			fail(w, http.StatusForbidden, "revoked", "This machine's link was revoked.")
			return
		}
		if a := d.account; !a.deleteAt.IsZero() && !f.now().Before(a.deleteAt) {
			fail(w, http.StatusForbidden, "deleted", "This account was deleted.")
			return
		}
		d.seen = f.now()
		h(w, r, d, body)
	}
}

func (f *Fake) version(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, map[string]string{"api": "v1", "server": "cloudtest"})
}

func (f *Fake) keys(w http.ResponseWriter, _ *http.Request) {
	enc := func(m map[string]ed25519.PublicKey) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = entitle.EncodeKey(v)
		}
		return out
	}
	reply(w, http.StatusOK, map[string]any{"entitlement": enc(f.Keys()), "denylist": enc(f.DenyListKeys())})
}

func (f *Fake) denylist(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	l := entitle.DenyList{Seq: f.denySeq + 1, Iat: f.now(), Keys: slices.Clone(f.denied)}
	f.mu.Unlock()
	tok, err := entitle.SignDenyList(l, DenyListKid, DevKey(DenyListKid))
	if err != nil {
		fail(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, tok+"\n")
}

// reserved are names no handle may take.
var reserved = []string{"admin", "api", "abuse", "cloud", "mail", "relay", "security", "status", "support", "www"}

func (f *Fake) linkStart(w http.ResponseWriter, _ *http.Request, keyID string, body []byte) {
	var in struct {
		DevicePub   string `json:"device_pub"`
		Handle      string `json:"handle"`
		ACMEAccount string `json:"acme_account"`
	}
	if err := json.Unmarshal(body, &in, json.RejectUnknownMembers(true)); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "The request body is not what link/start takes.")
		return
	}
	pub, err := entitle.ParseKey(in.DevicePub)
	if err != nil || httpsig.KeyID(pub) != keyID {
		fail(w, http.StatusUnauthorized, "unauthorized", "device_pub must be the key that signed the request.")
		return
	}
	if _, ok := f.devices[keyID]; ok {
		fail(w, http.StatusConflict, "already_linked", "This device is already linked.")
		return
	}
	if in.Handle != "" {
		if cloud.CheckHandle(in.Handle) != nil {
			fail(w, http.StatusBadRequest, "bad_handle", "A handle is 3 to 32 of a-z, 0-9 and single hyphens inside.")
			return
		}
		if slices.Contains(reserved, in.Handle) {
			fail(w, http.StatusBadRequest, "bad_handle", "That handle is reserved.")
			return
		}
		if f.handleTaken(in.Handle) {
			fail(w, http.StatusConflict, "handle_taken", "That handle is taken. Handles are never reassigned.")
			return
		}
	}
	if in.ACMEAccount != "" && cloud.CheckACMEAccount(in.ACMEAccount) != nil {
		fail(w, http.StatusBadRequest, "bad_acme_account", "acme_account must be an https URL fit for a CAA record.")
		return
	}
	l := &link{id: "lk_" + randomID(), pub: pub, handle: in.Handle, acme: in.ACMEAccount, status: cloud.LinkPending, expires: f.now().Add(linkTTL)}
	f.links[l.id] = l
	f.linkKeys[keyID] = pub
	reply(w, http.StatusOK, map[string]string{"id": l.id, "checkout_url": f.URL + "/checkout/" + l.id})
}

// handleTaken reports whether a handle is held, or asked for by a link
// still pending.
func (f *Fake) handleTaken(name string) bool {
	if _, ok := f.handles[name]; ok {
		return true
	}
	for _, l := range f.links {
		if l.handle == name && l.status == cloud.LinkPending && f.now().Before(l.expires) {
			return true
		}
	}
	return false
}

func (f *Fake) linkPoll(w http.ResponseWriter, r *http.Request, keyID string, _ []byte) {
	l, ok := f.links[r.PathValue("id")]
	if !ok || httpsig.KeyID(l.pub) != keyID {
		fail(w, http.StatusNotFound, "not_found", "No such link.")
		return
	}
	if l.status == cloud.LinkPending && !f.now().Before(l.expires) {
		l.status = cloud.LinkExpired
	}
	if l.status != cloud.LinkActive {
		reply(w, http.StatusOK, map[string]string{"status": l.status})
		return
	}
	d := f.devices[keyID]
	if d == nil || d.revoked {
		fail(w, http.StatusForbidden, "revoked", "This machine's link was revoked.")
		return
	}
	f.issue(w, d, map[string]string{"status": cloud.LinkActive})
}

func (f *Fake) checkout(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	err := f.pay(r.PathValue("id"))
	f.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	io.WriteString(w, "Paid (fake checkout). You can close this tab.\n")
}

// pay turns a pending link into an account, a handle and a device.
func (f *Fake) pay(id string) error {
	l, ok := f.links[id]
	if !ok {
		return errors.New("cloudtest: no such link")
	}
	now := f.now().UTC().Truncate(time.Second)
	if l.status == cloud.LinkActive {
		return nil
	}
	if l.status != cloud.LinkPending || !now.Before(l.expires) {
		return errors.New("cloudtest: link expired")
	}
	name := l.handle
	for name == "" || f.handles[name] != nil {
		name = randomHandle()
	}
	h := &handle{name: name, gen: 1, genAt: now, created: now}
	a := &account{id: "acct_" + randomID(), customer: "ctm_" + randomID(), paidThrough: now.Add(f.period), created: now, acme: l.acme, handle: h,
		events: []event{{"link", now}}}
	keyID := httpsig.KeyID(l.pub)
	f.handles[name] = h
	f.devices[keyID] = &device{pub: l.pub, keyID: keyID, account: a, gen: h.gen, created: now, seen: now}
	delete(f.linkKeys, keyID)
	l.status, l.account = cloud.LinkActive, a
	return nil
}

// entitlement signs d's current entitlement.
func (f *Fake) entitlement(d *device) (string, error) {
	a, h := d.account, d.account.handle
	now := f.now().UTC().Truncate(time.Second)
	u, _ := url.Parse(f.URL)
	exp := now.Add(entMax)
	if g := a.paidThrough.Add(entGrace); g.Before(exp) {
		exp = g
	}
	c := entitle.Claims{
		Iss: u.Host, Sub: a.id, Aud: entitle.Audience,
		Iat: now, Nbf: now, Exp: exp, PaidThrough: a.paidThrough,
		Gen: h.gen, Plan: "cloud", Feat: []string{"reach", "backup"},
		Handle: h.name, Hosts: []string{h.name + "." + cloud.TenantZone}, Cnf: entitle.EncodeKey(d.pub),
		Relays: slices.Clone(f.relays), BackupQuota: defaultQuota,
	}
	if f.tamper != nil {
		f.tamper(&c)
	}
	return entitle.Sign(c, EntitlementKid, DevKey(EntitlementKid))
}

func (f *Fake) refresh(w http.ResponseWriter, _ *http.Request, d *device, _ []byte) {
	f.issue(w, d, map[string]string{})
}

// issue answers with a new entitlement for d, added to out, unless another
// machine holds the handle now (409) or payment has lapsed (402).
func (f *Fake) issue(w http.ResponseWriter, d *device, out map[string]string) {
	a, h := d.account, d.account.handle
	switch {
	case h.gen > d.gen:
		reply(w, http.StatusConflict, map[string]any{"error": "superseded", "message": "Another machine took over this handle.", "gen": h.gen, "at": h.genAt})
	case !f.now().Before(a.paidThrough):
		reply(w, http.StatusPaymentRequired, map[string]any{"error": "lapsed", "message": "Payment has lapsed.", "paid_through": a.paidThrough})
	default:
		tok, err := f.entitlement(d)
		if err != nil {
			fail(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		out["entitlement"] = tok
		reply(w, http.StatusOK, out)
	}
}

func (f *Fake) acmeAccount(w http.ResponseWriter, _ *http.Request, d *device, body []byte) {
	var in struct {
		URI string `json:"uri"`
	}
	if json.Unmarshal(body, &in, json.RejectUnknownMembers(true)) != nil || cloud.CheckACMEAccount(in.URI) != nil {
		fail(w, http.StatusBadRequest, "bad_acme_account", "uri must be an https URL fit for a CAA record.")
		return
	}
	d.account.acme = in.URI
	d.account.events = append(d.account.events, event{"acme_account", f.now().UTC().Truncate(time.Second)})
	w.WriteHeader(http.StatusNoContent)
}

// me answers with every field the fake stores about the account. The email
// is fetched from the merchant of record on each call and never stored.
func (f *Fake) me(w http.ResponseWriter, _ *http.Request, d *device, _ []byte) {
	a := d.account
	status := "active"
	switch {
	case !a.deleteAt.IsZero():
		status = "deleting"
	case !f.now().Before(a.paidThrough):
		status = "lapsed"
	}
	var devices []map[string]any
	for _, x := range f.devices {
		if x.account == a {
			devices = append(devices, map[string]any{"pub": entitle.EncodeKey(x.pub), "created": x.created,
				"last_seen_day": x.seen.UTC().Format(time.DateOnly), "revoked": x.revoked})
		}
	}
	slices.SortFunc(devices, func(x, y map[string]any) int { return strings.Compare(x["pub"].(string), y["pub"].(string)) })
	var links []map[string]any
	for _, l := range f.links {
		if l.account == a {
			links = append(links, map[string]any{"id": l.id, "status": l.status, "acme_account": l.acme, "expires": l.expires.UTC().Truncate(time.Second)})
		}
	}
	slices.SortFunc(links, func(x, y map[string]any) int { return strings.Compare(x["id"].(string), y["id"].(string)) })
	h := a.handle
	var deletion any
	if !a.deleteAt.IsZero() {
		deletion = map[string]any{"delete_at": a.deleteAt}
	}
	reply(w, http.StatusOK, map[string]any{
		"account": map[string]any{"id": a.id, "billing_customer": a.customer, "plan": "cloud", "status": status,
			"paid_through": a.paidThrough, "created": a.created, "acme_account": a.acme},
		"email":      "owner@example.invalid",
		"devices":    devices,
		"handles":    []map[string]any{{"name": h.name, "gen": h.gen, "byod": false, "created": h.created, "released": nil}},
		"links":      links,
		"namespaces": []any{},
		"events":     a.events,
		"deletion":   deletion,
	})
}

func (f *Fake) portal(w http.ResponseWriter, _ *http.Request, d *device, _ []byte) {
	reply(w, http.StatusOK, map[string]string{"url": f.URL + "/portal/" + d.account.customer})
}

func (f *Fake) unlink(w http.ResponseWriter, _ *http.Request, d *device, _ []byte) {
	f.revoke(d, "unlinked")
	w.WriteHeader(http.StatusNoContent)
}

// revoke refuses d from now on and puts its key on the deny list, so relays
// drop it before its entitlement expires.
func (f *Fake) revoke(d *device, why string) {
	if d.revoked {
		return
	}
	d.revoked = true
	d.account.events = append(d.account.events, event{"device_" + why, f.now().UTC().Truncate(time.Second)})
	f.denied = append(f.denied, entitle.Entry{Value: entitle.EncodeKey(d.pub), Why: why})
	f.denySeq++
}

func (f *Fake) deleteAccount(w http.ResponseWriter, _ *http.Request, d *device, _ []byte) {
	a := d.account
	if a.deleteAt.IsZero() {
		a.deleteAt = f.now().UTC().Truncate(time.Second).Add(deleteWait)
		a.events = append(a.events, event{"delete_requested", f.now().UTC().Truncate(time.Second)})
	}
	reply(w, http.StatusAccepted, map[string]any{"delete_at": a.deleteAt})
}

func (f *Fake) restore(w http.ResponseWriter, _ *http.Request, d *device, _ []byte) {
	a := d.account
	if a.deleteAt.IsZero() {
		fail(w, http.StatusConflict, "not_deleting", "This account is not being deleted.")
		return
	}
	a.deleteAt = time.Time{}
	a.events = append(a.events, event{"delete_cancelled", f.now().UTC().Truncate(time.Second)})
	w.WriteHeader(http.StatusNoContent)
}

// devPaidThrough moves the end of a handle's paid period, as the merchant
// of record's webhooks would; a time in the past lapses it.
func (f *Fake) devPaidThrough(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Handle      string    `json:"handle"`
		PaidThrough time.Time `json:"paid_through"`
	}
	if !devRequest(w, r, &in) {
		return
	}
	if err := f.SetPaidThrough(in.Handle, in.PaidThrough.Truncate(time.Second)); err != nil {
		fail(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// devSupersede raises a handle's generation, as a recovery on another
// machine would.
func (f *Fake) devSupersede(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Handle string `json:"handle"`
	}
	if !devRequest(w, r, &in) {
		return
	}
	gen, err := f.Supersede(in.Handle)
	if err != nil {
		fail(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	reply(w, http.StatusOK, map[string]int64{"gen": gen})
}

// devRevoke revokes a device key and deny-lists it, as an admin would.
func (f *Fake) devRevoke(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DevicePub string `json:"device_pub"`
	}
	if !devRequest(w, r, &in) {
		return
	}
	pub, err := entitle.ParseKey(in.DevicePub)
	if err == nil {
		err = f.Revoke(pub)
	}
	if err != nil {
		fail(w, http.StatusNotFound, "not_found", "No such device.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// devRequest reads a dev route's JSON body strictly into in.
func devRequest(w http.ResponseWriter, r *http.Request, in any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err == nil {
		err = json.Unmarshal(body, in, json.RejectUnknownMembers(true))
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "The request body is not what this route takes.")
		return false
	}
	return true
}

// reply writes v as JSON with status.
func reply(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b)
}

// fail writes the documented error body.
func fail(w http.ResponseWriter, status int, code, message string) {
	reply(w, status, map[string]string{"error": code, "message": message})
}

func randomID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

var (
	adjectives = []string{"amber", "brisk", "copper", "ember", "lunar", "quiet", "velvet", "willow"}
	nouns      = []string{"finch", "fox", "heron", "lynx", "moth", "otter", "wren", "yak"}
)

func randomHandle() string {
	b := make([]byte, 3)
	rand.Read(b)
	return fmt.Sprintf("%s-%s-%02d", adjectives[int(b[0])%len(adjectives)], nouns[int(b[1])%len(nouns)], int(b[2])%100)
}
