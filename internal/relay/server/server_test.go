package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// clock is a settable test clock that starts at the real time.
type clock struct{ off atomic.Int64 }

func (c *clock) now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *clock) advance(d time.Duration) { c.off.Add(int64(d)) }

// dialName opens a TCP connection to the relay and sends a crypto/tls
// ClientHello for sni. It returns the raw connection.
func dialName(t testing.TB, r *testRelay, sni string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	cfg := &tls.Config{ServerName: sni, InsecureSkipVerify: true}
	if sni == "" {
		cfg.ServerName = ""
	}
	if _, err := c.Write(clientHello(t, cfg)); err != nil {
		t.Fatal(err)
	}
	return c
}

// closedSilently checks that the relay closes c without writing a byte.
func closedSilently(t testing.TB, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := c.Read(make([]byte, 1))
	if n != 0 {
		t.Fatalf("relay wrote %d bytes", n)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("relay left the connection open")
	}
}

// Acceptance: GetCertificate errors for every tenant name (table test). The
// certificate source behind it is never even asked.
func TestGetCertificateRefusesTenantNames(t *testing.T) {
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()}), Options{})
	mustTunnel(t, r, signedHello(t, d, "")) // h.test is live
	r.mu.Lock()
	r.certAsked = nil // the tunnel's own handshake was for the control name
	r.mu.Unlock()
	for _, tc := range []struct {
		name  string
		hello tls.ClientHelloInfo
	}{
		{"live tenant", tls.ClientHelloInfo{ServerName: "h.test"}},
		{"tenant, acme-tls/1", tls.ClientHelloInfo{ServerName: "h.test", SupportedProtos: []string{"acme-tls/1"}}},
		{"tenant, h2", tls.ClientHelloInfo{ServerName: "h.test", SupportedProtos: []string{"h2", "http/1.1"}}},
		{"hosted handle", tls.ClientHelloInfo{ServerName: "ember-otter-42.mirrin.link"}},
		{"upper-case tenant", tls.ClientHelloInfo{ServerName: "H.TEST"}},
		{"no SNI", tls.ClientHelloInfo{}},
		{"IP literal", tls.ClientHelloInfo{ServerName: "127.0.0.1"}},
		{"control name with a trailing dot", tls.ClientHelloInfo{ServerName: controlName + "."}},
		{"under the control name", tls.ClientHelloInfo{ServerName: "x." + controlName}},
		{"control name as a prefix", tls.ClientHelloInfo{ServerName: controlName + ".evil.test"}},
		{"wildcard", tls.ClientHelloInfo{ServerName: "*.test"}},
		{"Kelvin sign", tls.ClientHelloInfo{ServerName: "loca\u212alhost"}},
		{"control name with a NUL", tls.ClientHelloInfo{ServerName: controlName + "\x00"}},
	} {
		if c, err := r.getCertificate(&tc.hello); err == nil || c != nil {
			t.Errorf("%s: got a certificate", tc.name)
		}
	}
	if asked := r.asked(); len(asked) != 0 {
		t.Fatalf("certificate source asked for %q", asked)
	}
	for _, name := range []string{controlName, "LocalHost"} {
		if c, err := r.getCertificate(&tls.ClientHelloInfo{ServerName: name}); err != nil || c == nil {
			t.Fatalf("control name %q: %v", name, err)
		}
	}
}

// The default certificate source is autocert for the control name only;
// its host policy refuses every tenant name before any ACME request.
func TestAutocertHostPolicy(t *testing.T) {
	cfg := selfHostConfig(t, Allow{Hostname: "h.test", Key: newDevice(t).enc()})
	cfg.ACMEDirectory = "https://acme.invalid/directory"
	m := newAutocert(&cfg)
	ctx := context.Background()
	if err := m.HostPolicy(ctx, controlName); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"h.test", "x." + controlName, "ember-otter-42.mirrin.link"} {
		if m.HostPolicy(ctx, h) == nil {
			t.Errorf("autocert would request %s", h)
		}
	}
	if m.Client.DirectoryURL != cfg.ACMEDirectory || m.Cache != autocert.DirCache(filepath.Join(cfg.StateDir, "autocert")) {
		t.Fatal("ACME directory or cache not configured")
	}
}

// One address cannot hold the relay's peek slots: past maxPeekingAddr
// silent connections, the next is closed at once.
func TestPeekSlotsPerAddress(t *testing.T) {
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: "h.test", Key: newDevice(t).enc()}), Options{})
	for range maxPeekingAddr {
		c, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	waitFor(t, 5*time.Second, "the slots to fill", func() bool { return len(r.peeking) == maxPeekingAddr })
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	closedSilently(t, c)
	if time.Since(start) > time.Second || r.m.conns.get(connBusy) != 1 {
		t.Fatalf("closed after %v, busy=%d", time.Since(start), r.m.conns.get(connBusy))
	}
}

// Acceptance: an unknown SNI is closed with no bytes written; so are a
// hello without SNI, and bytes that are not TLS at all.
func TestUnknownNameClosedSilently(t *testing.T) {
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()}), Options{})
	mustTunnel(t, r, signedHello(t, d, ""))
	closedSilently(t, dialName(t, r, "nobody.test"))
	closedSilently(t, dialName(t, r, "h.test.evil"))
	closedSilently(t, dialName(t, r, ""))
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: h.test\r\n\r\n")
	closedSilently(t, c)
	if got := r.m.conns.get(connUnknown); got != 3 {
		t.Fatalf("unknown_name = %d, want 3", got)
	}
}

// hostedRelay is a hosted relay with an issuer, a deny list server, a
// fake clock and no hello limit.
type hostedRelay struct {
	*testRelay
	is    issuer
	deny  *denyServer
	clock *clock
	polls *denyPolls
}

func startHosted(t testing.TB, edit func(*Config, *Options)) *hostedRelay {
	t.Helper()
	h := &hostedRelay{is: newIssuer(t), deny: newDenyServer(t), clock: &clock{}}
	cfg := hostedConfig(t, h.is, h.deny)
	cfg.Limits.HelloPerIPPerMin = 0
	opts := Options{HTTPClient: h.deny.Client(), Now: h.clock.now}
	if edit != nil {
		edit(&cfg, &opts)
	}
	h.polls = newDenyPolls(&opts)
	h.testRelay = startRelay(t, cfg, opts)
	waitSignal(t, 5*time.Second, "initial deny list refresh", h.polls.done)
	return h
}

// Acceptance: the refusal codes.
func TestRefusalCodes(t *testing.T) {
	h := startHosted(t, nil)
	now := time.Now()
	d := newDevice(t)

	cases := []struct {
		name  string
		hello helloFunc
		code  string
	}{
		{"expired", signedHello(t, d, h.is.entitlement(t, d, "ember", 3, now.Add(-40*24*time.Hour))), wire.CodeEntitlementExpired},
		{"not yet valid", signedHello(t, d, h.is.entitlement(t, d, "ember", 3, now.Add(time.Hour))), wire.CodeEntitlementExpired},
		{"host outside the zones", signedHello(t, d, h.is.entitlement(t, d, "ember", 3, now, "ember.other.test")), wire.CodeHostnameNotAllowed},
		{"host of another handle", signedHello(t, d, h.is.entitlement(t, d, "ember", 3, now, "otter.mirrin.test")), wire.CodeHostnameNotAllowed},
		{"control name as a host", signedHello(t, d, h.is.entitlement(t, d, "localhost", 3, now, "localhost")), wire.CodeHostnameNotAllowed},
		{"no entitlement", signedHello(t, d, ""), wire.CodeHostnameNotAllowed},
		{"bound to another key", signedHello(t, d, h.is.entitlement(t, newDevice(t), "ember", 3, now)), wire.CodeBadSignature},
		{"untrusted issuer", signedHello(t, d, newIssuer(t).entitlement(t, d, "ember", 3, now)), wire.CodeBadSignature},
		{"deny list presented as entitlement", signedHello(t, d, h.is.denyList(t, 1, nil)), wire.CodeBadSignature},
		{"wrong exporter", func(ch wire.Challenge, exp []byte) []byte {
			return signedHello(t, d, h.is.entitlement(t, d, "ember", 3, now))(ch, make([]byte, 32))
		}, wire.CodeBadSignature},
		{"signed for another relay", func(ch wire.Challenge, exp []byte) []byte {
			ch.Relay = "r2"
			return signedHello(t, d, h.is.entitlement(t, d, "ember", 3, now))(ch, exp)
		}, wire.CodeBadSignature},
		{"version 2", func(ch wire.Challenge, exp []byte) []byte {
			b := signedHello(t, d, "")(ch, exp)
			return bytes.Replace(b, []byte(`"v":1`), []byte(`"v":2`), 1)
		}, wire.CodeUpgradeRequired},
		{"malformed", func(wire.Challenge, []byte) []byte { return []byte(`{"t":"hello","v":1}`) }, wire.CodeBadSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if e := refusal(t, h.testRelay, tc.hello); e.Code != tc.code {
				t.Fatalf("got %s (%q), want %s", e.Code, e.Message, tc.code)
			}
		})
	}

	// A plan without Reach carries no names.
	tok, err := entitle.Sign(entitle.Claims{Iss: "i", Sub: "s", Aud: entitle.Audience, Iat: now, Nbf: now,
		Exp: now.Add(time.Hour), PaidThrough: now, Gen: 1, Feat: []string{"backup"}, Handle: "ember",
		Hosts: []string{"ember.mirrin.test"}, Cnf: d.enc()}, "ent-test-a", h.is.ent)
	if err != nil {
		t.Fatal(err)
	}
	if e := refusal(t, h.testRelay, signedHello(t, d, tok)); e.Code != wire.CodeHostnameNotAllowed {
		t.Fatalf("backup-only plan: %s", e.Code)
	}
}

// Acceptance: lower gen → superseded; same gen, older iat →
// superseded_retry; a higher gen displaces the holder, which is told so on
// its control stream with the new gen.
func TestSupersede(t *testing.T) {
	h := startHosted(t, nil)
	now := time.Now().Truncate(time.Second)
	old, cur, next := newDevice(t), newDevice(t), newDevice(t)

	holder := mustTunnel(t, h.testRelay, signedHello(t, cur, h.is.entitlement(t, cur, "ember", 3, now)))
	if w := holder.welcome; w.Gen != 3 || w.Hostnames[0] != "ember.mirrin.test" || w.MaxStreams != 64 || w.Keepalive != 25 {
		t.Fatalf("welcome %+v", w)
	}
	if e := refusal(t, h.testRelay, signedHello(t, old, h.is.entitlement(t, old, "ember", 2, now.Add(time.Minute)))); e.Code != wire.CodeSuperseded {
		t.Fatalf("lower gen: %s", e.Code)
	}
	if e := refusal(t, h.testRelay, signedHello(t, cur, h.is.entitlement(t, cur, "ember", 3, now.Add(-time.Minute)))); e.Code != wire.CodeSupersededRetry {
		t.Fatalf("same gen, older iat: %s", e.Code)
	}
	// Still carried by the holder.
	if tt, ok := h.reg.Lookup("ember.mirrin.test"); !ok || tt.Gen != 3 {
		t.Fatal("holder lost the name to a refused tunnel")
	}

	newer := mustTunnel(t, h.testRelay, signedHello(t, next, h.is.entitlement(t, next, "ember", 4, now)))
	c := holder.nextControl(t, 5*time.Second)
	if c.T != wire.ControlSuperseded || c.Gen != 4 {
		t.Fatalf("holder got %+v", c)
	}
	waitSignal(t, 5*time.Second, "the displaced session to close", holder.sess.CloseChan())
	if tt, ok := h.reg.Lookup("ember.mirrin.test"); !ok || tt.Gen != 4 || newer.sess.IsClosed() {
		t.Fatal("the higher gen does not hold the name")
	}
	// A reconnect with the very same entitlement replaces its own tunnel.
	again := mustTunnel(t, h.testRelay, signedHello(t, next, h.is.entitlement(t, next, "ember", 4, now)))
	if c := newer.nextControl(t, 5*time.Second); c.T != wire.ControlSuperseded || c.Gen != 4 {
		t.Fatalf("stale tunnel got %+v", c)
	}
	if again.sess.IsClosed() {
		t.Fatal("reconnect closed")
	}
}

// Self-hosted, the tunnel that connected last holds a name. Another
// machine's key makes the displaced daemon stand by (a higher gen); the
// same key reconnecting just replaces its stale tunnel (the same gen).
func TestSelfHostSupersede(t *testing.T) {
	old, cur := newDevice(t), newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: "h.test", Key: old.enc()}, Allow{Hostname: "h.test", Key: cur.enc()}), Options{})
	a := mustTunnel(t, r, signedHello(t, old, ""))
	b := mustTunnel(t, r, signedHello(t, cur, ""))
	if c := a.nextControl(t, 5*time.Second); c.T != wire.ControlSuperseded || c.Gen <= a.welcome.Gen {
		t.Fatalf("old machine got %+v; it would not stand by", c)
	}
	mustTunnel(t, r, signedHello(t, cur, ""))
	if c := b.nextControl(t, 5*time.Second); c.T != wire.ControlSuperseded || c.Gen != b.welcome.Gen {
		t.Fatalf("reconnect: stale tunnel got %+v", c)
	}
}

// Acceptance: a deny-listed key is refused within 60 s of the list update:
// the relay polls every denyEvery, one refresh (primary and mirror) takes
// at most denyTimeout, a live tunnel is ended at the next poll, and new
// hellos are refused. A stale list (lower seq) is ignored; a newer one can
// lift the entry.
func TestDenyList(t *testing.T) {
	// Keep the production worst-case bound; TestDenyListBudget covers a
	// hanging primary. Drive each poll explicitly here so assertions wait
	// for verification, tunnel eviction and the cache write to finish.
	if worst := denyEvery + denyTimeout; worst > 55*time.Second {
		t.Fatalf("a deny list can take %v to apply", worst)
	}
	h := startHosted(t, nil)
	now := time.Now()
	d, other := newDevice(t), newDevice(t)
	ent := h.is.entitlement(t, d, "ember", 3, now)
	tt := mustTunnel(t, h.testRelay, signedHello(t, d, ent))
	mustTunnel(t, h.testRelay, signedHello(t, other, h.is.entitlement(t, other, "otter", 1, now)))

	h.deny.set(h.is.denyList(t, 5, nil, d))
	h.polls.next(t)
	c := tt.nextControl(t, 5*time.Second)
	if c.T != wire.ControlNotice || !strings.Contains(c.Message, "suspended") {
		t.Fatalf("got %+v", c)
	}
	waitSignal(t, 5*time.Second, "the denied tunnel to close", tt.sess.CloseChan())
	e := refusal(t, h.testRelay, signedHello(t, d, ent))
	if e.Code != wire.CodeDenied || e.RetryAfter != deniedRetry {
		t.Fatalf("hello after deny: %+v", e)
	}
	if _, ok := h.reg.Lookup("otter.mirrin.test"); !ok {
		t.Fatal("an unrelated tunnel was ended")
	}

	// A stale list that would lift the entry is ignored.
	h.deny.set(h.is.denyList(t, 4, nil))
	h.polls.next(t)
	h.polls.next(t)
	if e := refusal(t, h.testRelay, signedHello(t, d, ent)); e.Code != wire.CodeDenied {
		t.Fatalf("stale list applied: %s", e.Code)
	}
	if h.m.denySeq.Load() != 5 {
		t.Fatalf("seq %d", h.m.denySeq.Load())
	}

	// A handle can be denied too, and a newer list lifts the key.
	h.deny.set(h.is.denyList(t, 6, []string{"otter"}))
	h.polls.next(t)
	if h.m.denySeq.Load() != 6 {
		t.Fatalf("seq %d, want 6", h.m.denySeq.Load())
	}
	mustTunnel(t, h.testRelay, signedHello(t, d, ent))
	if e := refusal(t, h.testRelay, signedHello(t, other, h.is.entitlement(t, other, "otter", 1, now))); e.Code != wire.CodeDenied {
		t.Fatalf("denied handle: %s", e.Code)
	}

	// The last good list is kept on disk and applied before any fetch.
	b, err := os.ReadFile(filepath.Join(h.cfg.StateDir, denyCacheFile))
	if err != nil {
		t.Fatal(err)
	}
	l, err := entitle.VerifyDenyList(strings.TrimSpace(string(b)), h.pol.dlKeys, time.Now())
	if err != nil || l.Seq != 6 {
		t.Fatalf("cached list: seq %d, %v", l.Seq, err)
	}
}

// When the primary fails, the mirror is used; a list that does not verify
// is never applied.
func TestDenyListMirror(t *testing.T) {
	var h *hostedRelay
	mirror := newDenyServer(t)
	h = startHosted(t, func(c *Config, o *Options) {
		c.DenylistURL = "https://127.0.0.1:1/v1/denylist" // nothing listens
		c.MirrorURL = mirror.URL + "/denylist.paseto"
		o.HTTPClient = mirror.Client()
	})
	mirror.set(newIssuer(t).denyList(t, 9, []string{"ember"})) // wrong key
	errorsBefore := h.m.denyErrors.Load()
	h.polls.next(t)
	if got := h.m.denyErrors.Load() - errorsBefore; got != 2 {
		t.Fatalf("primary and mirror errors = %d, want 2", got)
	}
	if h.m.denySeq.Load() != 0 {
		t.Fatal("an unverified list was applied")
	}
	mirror.set(h.is.denyList(t, 2, []string{"ember"}))
	h.polls.next(t)
	if h.m.denySeq.Load() != 2 {
		t.Fatalf("seq %d, want 2", h.m.denySeq.Load())
	}
}

// A primary that hangs costs only its share of one refresh's budget; the
// mirror gets the rest, so the 60 s bound holds exactly when the mirror is
// needed.
func TestDenyListBudget(t *testing.T) {
	hang := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(hang.Close)
	mirror := newDenyServer(t)
	const budget = time.Second
	// The time each fetch is given, as the deadline its request carries: a
	// busy machine can take longer to get round to what follows, but not
	// change the deadlines.
	type fetch struct{ at, deadline time.Time }
	var mu sync.Mutex
	fetches := map[string]fetch{}
	client := mirror.Client()
	base := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		d, ok := r.Context().Deadline()
		if !ok {
			t.Errorf("%s was fetched with no deadline", r.URL)
		}
		mu.Lock()
		fetches[r.URL.Path] = fetch{time.Now(), d}
		mu.Unlock()
		return base.RoundTrip(r)
	})
	h := startHosted(t, func(c *Config, o *Options) {
		c.DenylistURL = hang.URL + "/v1/denylist"
		c.MirrorURL = mirror.URL + "/denylist.paseto"
		o.HTTPClient = client
		o.denyTimeout = budget
	})
	mirror.set(h.is.denyList(t, 7, []string{"ember"}))
	start := time.Now()
	h.refreshDenyList()
	if h.m.denySeq.Load() != 7 {
		t.Fatal("the mirror's list was not applied")
	}
	mu.Lock()
	defer mu.Unlock()
	primary := time.Duration(float64(budget) * denyPrimaryShare)
	// A deadline set once the refresh began, at most share before the fetch.
	within := func(name string, f fetch, share time.Duration) {
		if f.deadline.Before(start.Add(share)) || f.deadline.After(f.at.Add(share)) {
			t.Errorf("the %s was fetched %v into the refresh with %v left; its share is %v", name, f.at.Sub(start), f.deadline.Sub(f.at), share)
		}
	}
	within("primary", fetches["/v1/denylist"], primary)
	// The mirror has what is left of the whole budget.
	if m := fetches["/denylist.paseto"]; m.deadline.Before(start.Add(budget)) || m.deadline.After(fetches["/v1/denylist"].at.Add(budget)) {
		t.Errorf("the mirror's deadline is %v after the refresh began; the whole budget is %v", m.deadline.Sub(start), budget)
	}
	// The mirror is asked only once the primary's share has run out.
	if m, p := fetches["/denylist.paseto"], fetches["/v1/denylist"]; m.at.Before(p.deadline) {
		t.Errorf("the mirror was asked %v before the primary's share ran out", p.deadline.Sub(m.at))
	}
}

// A relay that restarts while the deny list is unreachable still refuses
// what it refused before.
func TestDenyListCacheSurvivesRestart(t *testing.T) {
	h := startHosted(t, nil)
	d := newDevice(t)
	h.deny.set(h.is.denyList(t, 3, nil, d))
	h.polls.next(t)
	if h.m.denySeq.Load() != 3 {
		t.Fatalf("seq %d, want 3", h.m.denySeq.Load())
	}
	h.Close()

	h.deny.set("") // down
	cfg := h.cfg
	opts := Options{HTTPClient: h.deny.Client()}
	polls := newDenyPolls(&opts)
	r2 := startRelay(t, cfg, opts)
	waitSignal(t, 5*time.Second, "cached list and initial refresh", polls.done)
	if r2.m.denySeq.Load() != 3 {
		t.Fatalf("cached seq %d, want 3", r2.m.denySeq.Load())
	}
	if e := refusal(t, r2, signedHello(t, d, h.is.entitlement(t, d, "ember", 1, time.Now()))); e.Code != wire.CodeDenied {
		t.Fatalf("got %s", e.Code)
	}
}

// Acceptance: status is online while tunnelled; last_seen is within 1 s
// of the disconnect; a wrong k gets 404.
func TestStatus(t *testing.T) {
	h := startHosted(t, nil)
	d := newDevice(t)
	tt := mustTunnel(t, h.testRelay, signedHello(t, d, h.is.entitlement(t, d, "ember", 3, time.Now())))
	client := h.https()
	k := base64.RawURLEncoding.EncodeToString(d.statusKey)
	get := func(path string) (int, statusJSON, http.Header) {
		t.Helper()
		resp, err := client.Get("https://" + controlName + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var s statusJSON
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 200 {
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, s, resp.Header
	}
	code, s, hdr := get("/v1/status/ember?k=" + k)
	if code != 200 || !s.Online || s.Since == "" || hdr.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("online: %d %+v %v", code, s, hdr)
	}
	if code, s, _ := get("/v1/status/ember.mirrin.test?k=" + k); code != 200 || !s.Online {
		t.Fatalf("by hostname: %d %+v", code, s)
	}
	wrong := make([]byte, 32)
	rand.Read(wrong)
	for _, p := range []string{
		"/v1/status/ember?k=" + base64.RawURLEncoding.EncodeToString(wrong),
		"/v1/status/ember?k=" + k[:42],
		"/v1/status/ember?k=" + k + "A",
		"/v1/status/ember",
		"/v1/status/otter?k=" + k,
	} {
		if code, _, hdr := get(p); code != 404 || hdr.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: %d", p, code)
		}
	}

	done := tunnelDone(t, h.Server, "ember.mirrin.test")
	tt.sess.Close()
	gone := time.Now()
	waitSignal(t, 5*time.Second, "the tunnel to detach", done)
	code, s, _ = get("/v1/status/ember?k=" + k)
	if code != 200 || s.Online {
		t.Fatalf("offline: %d %+v", code, s)
	}
	last, err := time.Parse(time.RFC3339, s.LastSeen)
	if err != nil {
		t.Fatal(err)
	}
	if diff := last.Sub(gone); diff < -time.Second || diff > time.Second {
		t.Fatalf("last_seen %v is %v from the disconnect", last, diff)
	}
}

func TestStatusRateLimited(t *testing.T) {
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()}), Options{})
	client := r.https()
	var last int
	for range statusPerMin + 1 {
		resp, err := client.Get("https://" + controlName + "/v1/status/h.test?k=x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("request %d got %d", statusPerMin+1, last)
	}
}

// Acceptance: 10 hellos per minute per address; the 11th is rate_limited.
func TestHelloRateLimit(t *testing.T) {
	clk := &clock{}
	d := newDevice(t)
	cfg := selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()})
	r := startRelay(t, cfg, Options{Now: clk.now})
	for i := range 10 {
		if _, err := dialTunnel(t, r, signedHello(t, d, "")); err != nil {
			t.Fatalf("hello %d: %v", i+1, err)
		}
	}
	e := refusal(t, r, signedHello(t, d, ""))
	if e.Code != wire.CodeRateLimited || e.RetryAfter != 60 {
		t.Fatalf("11th hello: %+v", e)
	}
	clk.advance(time.Minute)
	mustTunnel(t, r, signedHello(t, d, ""))
}

// Acceptance: the abuse heuristic auto-suspends a handle above the
// threshold (fake clock) and logs only /24 or /48.
func TestAbuseSuspends(t *testing.T) {
	clk := &clock{}
	var logs bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(lockedWriter{&mu, &logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var suspended atomic.Value
	d := newDevice(t)
	cfg := selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()})
	cfg.Abuse.DistinctNetsPerDay = 200
	r := startRelay(t, cfg, Options{Now: clk.now, Log: log, OnSuspend: func(h string, n int) { suspended.Store(h) }})
	tt := mustTunnel(t, r, signedHello(t, d, ""))

	var clients []netip.Addr
	// 199 IPv4 networks, each reached from several addresses in the same
	// /24, and one IPv6 /48 from several /64s: 200 networks, at the limit.
	for i := range 199 {
		for host := range 3 {
			clients = append(clients, netip.AddrFrom4([4]byte{198, byte(i / 250), byte(i % 250), byte(7 + host*50)}))
		}
	}
	for host := range 3 {
		clients = append(clients, netip.MustParseAddr("2001:db8:77:"+string(rune('1'+host))+"::9"))
	}
	for _, a := range clients {
		if tripped, _ := r.abuse.observe("h.test", a, clk.now()); tripped {
			t.Fatalf("suspended at %v, before the limit", a)
		}
	}
	// The next day starts from zero.
	clk.advance(24 * time.Hour)
	for _, a := range clients {
		if tripped, _ := r.abuse.observe("h.test", a, clk.now()); tripped {
			t.Fatal("yesterday's networks still count")
		}
	}
	// Network 201 trips it. (TestAbuseSuspendsThroughSplice drives the
	// same thing with real client connections.)
	last := netip.MustParseAddr("203.0.113.77")
	tripped, n := r.abuse.observe("h.test", last, clk.now())
	if !tripped || n != 201 {
		t.Fatalf("tripped %v at %d networks", tripped, n)
	}
	r.suspend("h.test", n, last)
	if c := tt.nextControl(t, 5*time.Second); c.T != wire.ControlNotice || !strings.Contains(c.Message, "under review") {
		t.Fatalf("tunnel got %+v", c)
	}
	waitSignal(t, 5*time.Second, "the suspended tunnel to end", tt.sess.CloseChan())
	if e := refusal(t, r, signedHello(t, d, "")); e.Code != wire.CodeDenied || !strings.Contains(e.Message, "under review") {
		t.Fatalf("hello while suspended: %+v", e)
	}
	if suspended.Load() != "h.test" || r.m.suspensions.Load() != 1 {
		t.Fatal("operator not told")
	}
	// The suspension lapses.
	clk.advance(cfg.Abuse.SuspendFor)
	mustTunnel(t, r, signedHello(t, d, ""))

	mu.Lock()
	out := logs.String()
	mu.Unlock()
	if !strings.Contains(out, "suspended for review") || !strings.Contains(out, `"latest":"203.0.113.0/24"`) {
		t.Fatalf("no suspension line:\n%s", out)
	}
	clients = append(clients, last, netip.MustParseAddr("127.0.0.1"))
	for _, a := range clients {
		if strings.Contains(out, a.String()) {
			t.Fatalf("log shows a client address %s:\n%s", a, out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		for k, v := range m {
			s, _ := v.(string)
			if a, err := netip.ParseAddr(s); err == nil {
				t.Fatalf("log field %s is a bare address %s", k, a)
			}
			if p, err := netip.ParsePrefix(s); err == nil && p.Bits() != 24 && p.Bits() != 48 {
				t.Fatalf("log field %s is %s, finer than /24 or /48", k, p)
			}
		}
	}
	// The heuristic keys on networks: /24 and /48.
	if netOf(netip.MustParseAddr("203.0.113.77")).String() != "203.0.113.0/24" ||
		netOf(netip.MustParseAddr("2001:db8:77:1::9")).String() != "2001:db8:77::/48" ||
		netOf(netip.MustParseAddr("::ffff:203.0.113.77")).String() != "203.0.113.0/24" {
		t.Fatal("netOf")
	}
}

// The same heuristic driven by real client connections from distinct
// networks: the splice path observes each one, and the connection that
// takes the handle over the limit suspends it. The tunnel is kicked, new
// hellos are refused, the operator is told, and the suspension outlives a
// restart.
func TestAbuseSuspendsThroughSplice(t *testing.T) {
	d := newDevice(t)
	cfg := selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()})
	cfg.Abuse.DistinctNetsPerDay = 2
	told := make(chan string, 1)
	r := startRelayOn(t, cfg, Options{OnSuspend: func(h string, n int) { told <- fmt.Sprint(h, " ", n) }}, manyNetworks)
	tt := mustTunnel(t, r, signedHello(t, d, ""))

	nets := map[netip.Prefix]bool{}
	for range 2 {
		dialName(t, r, "h.test")
		_, st, hdr := tt.accept(t)
		st.Close()
		nets[netOf(hdr.Source.Addr())] = true
	}
	if len(nets) != 2 {
		t.Fatalf("clients from %v", nets)
	}
	closedSilently(t, dialName(t, r, "h.test")) // the third network
	if c := tt.nextControl(t, 5*time.Second); c.T != wire.ControlNotice || !strings.Contains(c.Message, "under review") {
		t.Fatalf("tunnel got %+v", c)
	}
	waitSignal(t, 5*time.Second, "the suspended tunnel to end", tt.sess.CloseChan())
	if e := refusal(t, r, signedHello(t, d, "")); e.Code != wire.CodeDenied || !strings.Contains(e.Message, "under review") {
		t.Fatalf("hello while suspended: %+v", e)
	}
	select {
	case got := <-told:
		if got != "h.test 3" {
			t.Fatalf("operator told %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operator not told")
	}
	if r.m.suspensions.Load() != 1 || r.m.conns.get(connSuspended) != 1 {
		t.Fatalf("suspensions %d, suspended connections %d", r.m.suspensions.Load(), r.m.conns.get(connSuspended))
	}

	r.Close()
	r2 := startRelay(t, cfg, Options{})
	if e := refusal(t, r2, signedHello(t, d, "")); e.Code != wire.CodeDenied {
		t.Fatalf("after a restart: %s", e.Code)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// A tunnel carries at most streams_per_tunnel client connections; the
// next is closed without a byte.
func TestStreamLimit(t *testing.T) {
	d := newDevice(t)
	cfg := selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()})
	cfg.Limits.StreamsPerTunnel = 2
	r := startRelay(t, cfg, Options{})
	tt := mustTunnel(t, r, signedHello(t, d, ""))
	if tt.welcome.MaxStreams != 2 {
		t.Fatalf("welcome max_streams %d", tt.welcome.MaxStreams)
	}
	dialName(t, r, "h.test")
	dialName(t, r, "h.test")
	tt.accept(t)
	tt.accept(t)
	closedSilently(t, dialName(t, r, "h.test"))
	if r.m.conns.get(connFull) != 1 {
		t.Fatal("not counted")
	}
}

// A spliced connection with no bytes either way for limits.idle is cut.
func TestIdleCut(t *testing.T) {
	d := newDevice(t)
	cfg := selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()})
	cfg.Limits.Idle = time.Second
	r := startRelay(t, cfg, Options{})
	tt := mustTunnel(t, r, signedHello(t, d, ""))
	c := dialName(t, r, "h.test")
	br, st, _ := tt.accept(t)
	io.ReadFull(br, make([]byte, 5))
	start := time.Now()
	// Activity keeps it open.
	for range 3 {
		time.Sleep(400 * time.Millisecond)
		st.Write([]byte("x"))
		c.Read(make([]byte, 1))
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.ReadAll(c) // EOF or a reset: either way, cut
	if el := time.Since(start); el < 2*time.Second || el > 4*time.Second {
		t.Fatalf("cut after %v", el)
	}
}

// bps paces a tunnel: 1 Mbit/s moves 250 KB in about 1.5 s after the
// burst.
func TestBandwidthLimit(t *testing.T) {
	d := newDevice(t)
	cfg := selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()})
	cfg.Limits.BPS = 1e6
	r := startRelay(t, cfg, Options{})
	tt := mustTunnel(t, r, signedHello(t, d, ""))
	c := dialName(t, r, "h.test")
	br, st, _ := tt.accept(t)
	go io.Copy(io.Discard, br)
	start := time.Now()
	go st.Write(make([]byte, 250<<10))
	if _, err := io.ReadFull(c, make([]byte, 250<<10)); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < time.Second {
		t.Fatalf("250 KB at 1 Mbit/s took %v", el)
	}
}

// Shutdown tells every daemon to go elsewhere for a while.
func TestShutdownDrains(t *testing.T) {
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()}), Options{})
	tt := mustTunnel(t, r, signedHello(t, d, ""))
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- r.Shutdown(ctx)
	}()
	if c := tt.nextControl(t, 5*time.Second); c.T != wire.ControlDrain || c.RetryAfter != drainRetryAfter {
		t.Fatalf("got %+v", c)
	}
	<-done
	waitSignal(t, 5*time.Second, "the session to end", tt.sess.CloseChan())
}

// A daemon cannot make the relay hold streams it opens: the relay never
// accepts them, parks at most streams_per_tunnel, and resets the rest.
func TestDaemonOpenedStreamsRefused(t *testing.T) {
	d := newDevice(t)
	cfg := selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()})
	cfg.Limits.StreamsPerTunnel = 4
	r := startRelay(t, cfg, Options{})
	tt := mustTunnel(t, r, signedHello(t, d, ""))
	for range 20 {
		st, err := tt.sess.OpenStream()
		if err != nil {
			break
		}
		st.SetWriteDeadline(time.Now().Add(time.Second))
		st.Write([]byte("x"))
	}
	waitFor(t, 5*time.Second, "resets", func() bool { return tt.sess.NumStreams() <= 1+4 })
	if tt.sess.IsClosed() {
		t.Fatal("the tunnel died")
	}
}

// Client connections open their streams in parallel: every one reaches
// the daemon before the daemon has accepted any.
func TestParallelOpens(t *testing.T) {
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()}), Options{})
	tt := mustTunnel(t, r, signedHello(t, d, ""))
	const n = 32
	for range n {
		dialName(t, r, "h.test")
	}
	waitFor(t, 5*time.Second, "32 streams in flight", func() bool { return tt.sess.NumStreams() == 1+n })
}

const examplePlaceholder = "REPLACE-WITH-THE-KEY-FROM-mirrin-reach-use-relay"

func TestConfig(t *testing.T) {
	// The example ships with a placeholder that must be replaced before
	// it validates; with a real key in its place, it is a working config.
	raw, err := os.ReadFile("../../../packaging/relay/relay.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseConfig(raw); err == nil {
		t.Fatal("the example validates with its placeholder key")
	}
	ex, err := ParseConfig(bytes.ReplaceAll(raw, []byte(examplePlaceholder), []byte(newDevice(t).enc())))
	if err != nil {
		t.Fatalf("example: %v", err)
	}
	if !ex.SelfHosted() || ex.Limits.BPS != 20e6 || ex.Limits.Idle != 10*time.Minute || ex.Limits.StreamsPerTunnel != 64 ||
		ex.Limits.HelloPerIPPerMin != 10 || ex.Abuse.DistinctNetsPerDay != 200 || ex.MetricsListen != "127.0.0.1:9100" {
		t.Fatalf("example: %+v", ex)
	}
	key := newDevice(t).enc()
	is := newIssuer(t)
	base := "id: r1\ncontrol_hostname: r1.relay.example.org\nstate_dir: /tmp/x\n"
	self := base + "allow:\n  - {hostname: twin.example.org, key: " + key + "}\n"
	hosted := base + "zones: [mirrin.link]\ndenylist_url: https://cloud.mirrin.app/v1/denylist\nissuer_keys:\n" +
		"  - {kid: ent-2026a, key: " + is.keys()[0].Key + "}\n  - {kid: dl-2026a, key: " + is.keys()[1].Key + "}\n"
	good := map[string]string{
		"self-host":  self,
		"hosted":     hosted,
		"bps as 2e7": self + "limits: {bps: 20e6, idle: 90s}\n",
		"cert files": strings.Replace(self, "state_dir: /tmp/x\n", "cert_file: a.pem\nkey_file: b.pem\n", 1),
	}
	for name, y := range good {
		if _, err := ParseConfig([]byte(y)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]string{
		"unknown field":            self + "denylist: x\n",
		"both modes":               hosted + "allow:\n  - {hostname: t.example.org, key: " + key + "}\n",
		"neither mode":             base,
		"self-host with deny list": self + "denylist_url: https://x.test/\n",
		"hosted without deny list": strings.Replace(hosted, "denylist_url: https://cloud.mirrin.app/v1/denylist\n", "", 1),
		"http deny list":           strings.Replace(hosted, "https://cloud", "http://cloud", 1),
		"control inside a zone":    strings.Replace(hosted, "r1.relay.example.org", "r1.mirrin.link", 1),
		"same key both purposes":   strings.Replace(hosted, is.keys()[1].Key, is.keys()[0].Key, 1),
		"kid without purpose":      strings.Replace(hosted, "dl-2026a", "wk-2026a", 1),
		"allow the control name":   self + "  - {hostname: r1.relay.example.org, key: " + key + "}\n",
		"upper-case hostname":      strings.Replace(self, "twin.example.org", "Twin.example.org", 1),
		"bad key":                  strings.Replace(self, key, key[:40], 1),
		"duplicate allow":          self + "  - {hostname: twin.example.org, key: " + key + "}\n",
		"bad id":                   strings.Replace(self, "id: r1", "id: R1", 1),
		"no state_dir":             strings.Replace(self, "state_dir: /tmp/x\n", "", 1),
		"cert without key":         strings.Replace(self, "state_dir: /tmp/x\n", "cert_file: a.pem\n", 1),
		"tiny bps":                 self + "limits: {bps: 1000}\n",
		"integer idle":             self + "limits: {idle: 600}\n",
		"zero streams":             self + "limits: {streams_per_tunnel: 0}\n",
		"two documents":            self + "---\nid: r2\n",
	}
	for name, y := range bad {
		if _, err := ParseConfig([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Plain HTTP only ever answers with a 308 to https.
func TestRedirect(t *testing.T) {
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()}), Options{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go r.ServeRedirect(ln)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for host, want := range map[string]string{
		"h.test":    "https://h.test/pair?x=1",
		"H.Test:80": "https://h.test/pair?x=1",
		"bad_host":  "",
		"[::1]:80":  "",
	} {
		req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/pair?x=1", nil)
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if want == "" && resp.StatusCode != 400 || want != "" && (resp.StatusCode != 308 || resp.Header.Get("Location") != want) {
			t.Errorf("%s: %d %s", host, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

// Metrics have no per-handle or per-address labels; the connection log
// holds only /24 and /48 networks and is served on loopback only.
func TestMetricsAndConnLog(t *testing.T) {
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()}), Options{})
	tt := mustTunnel(t, r, signedHello(t, d, ""))
	c := dialName(t, r, "h.test")
	br, st, _ := tt.accept(t)
	io.ReadFull(br, make([]byte, 5))
	st.Write([]byte("hello"))
	io.ReadFull(c, make([]byte, 5))
	st.Close()
	c.Close()
	waitFor(t, 5*time.Second, "the splice to end", func() bool { return r.m.splicing.Load() == 0 && r.m.toClient.Load() == 5 })
	closedSilently(t, dialName(t, r, "nobody.test"))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go r.ServeMetrics(ln)
	get := func(p string) string {
		resp, err := http.Get("http://" + ln.Addr().String() + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	m := get("/metrics")
	for _, want := range []string{"mirrin_relay_tunnels 1", `mirrin_relay_connections_total{result="spliced"} 1`,
		`mirrin_relay_connections_total{result="unknown_name"} 1`, `mirrin_relay_hellos_total{result="welcome"} 1`,
		`mirrin_relay_splice_bytes_total{dir="to_client"} 5`, "mirrin_relay_denylist_age_seconds -1"} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	for _, leak := range []string{"h.test", "nobody", "127.0.0"} {
		if strings.Contains(m, leak) {
			t.Errorf("metrics mention %q", leak)
		}
	}
	var log struct {
		Recent []connRecord `json:"recent"`
	}
	if err := json.Unmarshal([]byte(get("/debug/connlog?handle=h.test")), &log); err != nil {
		t.Fatal(err)
	}
	if len(log.Recent) != 1 || log.Recent[0].Client.String() != "127.0.0.0/24" || log.Recent[0].ToClient != 5 || log.Recent[0].ToDaemon == 0 {
		t.Fatalf("connlog %+v", log.Recent)
	}
}

// The connection log keeps raw records for 72 h and hourly totals for 30
// days, within its size bounds.
func TestConnLogRetention(t *testing.T) {
	l := newConnLog(ConnLog{Entries: 3, HourlyEntries: 2})
	t0 := time.Unix(1_790_000_000, 0)
	for i := range 5 {
		l.add(connRecord{At: t0.Add(time.Duration(i) * time.Hour).Unix(), Handle: "h", ToClient: 1})
	}
	recs, hours := l.read("h")
	if len(recs) != 3 || recs[0].At != t0.Add(2*time.Hour).Unix() || len(hours) != 2 {
		t.Fatalf("%d records, %d hours", len(recs), len(hours))
	}
	l.prune(t0.Add(4*time.Hour + rawKeep))
	if recs, _ := l.read(""); len(recs) != 1 {
		t.Fatalf("after 72 h: %d records", len(recs))
	}
	l.prune(t0.Add(5*time.Hour + hourlyKeep))
	if recs, hours := l.read(""); len(recs) != 0 || len(hours) != 0 {
		t.Fatalf("after 30 days: %d, %d", len(recs), len(hours))
	}
}

// The relay's key material is only ever its own: the only tls.Config in
// the package is the control name's, and the only certificate source is
// behind getCertificate.
func TestNoTenantKeyPath(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		s := string(b)
		for _, pat := range []string{"tls.Server(", "tls.Client(", "tls.Dial", "Certificates:", "X509KeyPair(", "ParsePKCS", "ParseECPrivateKey"} {
			n := strings.Count(s, pat)
			allowed := 0
			if f == "control.go" && pat == "X509KeyPair(" {
				allowed = 1 // the control name's own cert_file
			}
			if n != allowed {
				t.Errorf("%s: %d × %s", f, n, pat)
			}
		}
		if f != "control.go" && strings.Contains(s, "tls.Config{") {
			t.Errorf("%s builds a tls.Config", f)
		}
	}
}
