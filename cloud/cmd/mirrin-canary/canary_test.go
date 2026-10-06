package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/billing"
	"github.com/MavrkAI/Mirrin/cloud/internal/dns"
	"github.com/MavrkAI/Mirrin/cloud/internal/keys"
	"github.com/MavrkAI/Mirrin/cloud/internal/server"
	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	relayserver "github.com/MavrkAI/Mirrin/internal/relay/server"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr/acmetest"
)

// The canary end to end, on a laptop: the control plane wired exactly as
// `mirrin-cloud serve --dev` wires it (fake merchant of record, fake DNS,
// development keys, a real SQLite store), two hosted mirrin-relays in this
// process, and a fake ACME CA that issues only to the account the handle's
// CAA record names. The canary links, pins its ACME account, gets its own
// certificate by TLS-ALPN-01 through the relays, and a probe reaches it
// through each relay on its own. Then the relays are stopped one at a time
// and together, and the probe's real rounds go through the paging rule.

const canaryHandle = "canary-one"

var canaryHost = canaryHandle + "." + cloud.TenantZone

type rig struct {
	t      *testing.T
	origin string
	dns    *dns.Fake
	ent    map[string]ed25519.PublicKey
	dl     map[string]ed25519.PublicKey
	relays []*hostedRelay
	rca    *relayCA
	ca     *acmetest.Server
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &rig{t: t, rca: newRelayCA(t)}

	// The relays' ports first: the control plane names them in every
	// entitlement, and the relays need the control plane's keys.
	var addrs []string
	for range 2 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addrs = append(addrs, ln.Addr().String())
		ln.Close()
	}
	cln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := server.DevConfig()
	cfg.Listen = cln.Addr().String()
	cfg.PublicURL = "http://" + cfg.Listen
	cfg.Relays = nil
	for i, a := range addrs {
		_, port, _ := net.SplitHostPort(a)
		cfg.Relays = append(cfg.Relays, server.Relay{ID: fmt.Sprintf("r%d", i+1), URL: "wss://localhost:" + port + "/v1/tunnel", IPs: []string{"127.0.0.1"}})
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "cloud.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake := billing.NewFake(cfg.PublicURL)
	r.dns = dns.NewFake()
	s, err := server.New(cfg, server.Options{Store: st, Billing: fake, DevBilling: fake, DNS: r.dns, Keys: keys.Dev(), Dev: true,
		Log: slog.New(slog.DiscardHandler), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.Serve(ctx, cln) }()
	t.Cleanup(func() { cancel(); <-done })
	r.origin = s.Origin()

	var ks struct {
		Entitlement map[string]string `json:"entitlement"`
		DenyList    map[string]string `json:"denylist"`
	}
	res, err := http.Get(r.origin + "/v1/keys")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(res.Body).Decode(&ks)
	res.Body.Close()
	r.ent, r.dl = parseKeys(t, ks.Entitlement), parseKeys(t, ks.DenyList)

	// Relays fetch the deny list only over https: a TLS front.
	front := httptest.NewTLSServer(&httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		u, _ := url.Parse(r.origin)
		pr.SetURL(u)
	}})
	t.Cleanup(front.Close)
	for i, a := range addrs {
		h := startHostedRelay(t, fmt.Sprintf("r%d", i+1), r.rca, r.ent, r.dl, front)
		h.stop()
		h.start(a)
		r.relays = append(r.relays, h)
	}

	// The CA validates through a relay, as Let's Encrypt's validators
	// arrive through whichever relay DNS names, and issues only to the
	// ACME account the handle's CAA record names.
	r.ca = acmetest.New()
	t.Cleanup(r.ca.Close)
	r.ca.Dial = func(ctx context.Context, _ string) (net.Conn, error) {
		for _, h := range r.relays {
			if h.running() {
				return h.dial(ctx, "")
			}
		}
		return nil, fmt.Errorf("no relay up")
	}
	r.ca.CAA = func(name, account string) error {
		rec, ok := r.dns.Records(strings.TrimSuffix(name, "."+cloud.TenantZone))
		if !ok {
			return fmt.Errorf("%s has no records", name)
		}
		for _, c := range rec.CAA {
			if c.Tag == "issue" && strings.Contains(c.Value, "accounturi="+account+";") {
				return nil
			}
		}
		return fmt.Errorf("CAA for %s doesn't name %s: %v", name, account, rec.CAA)
	}
	return r
}

func parseKeys(t *testing.T, in map[string]string) map[string]ed25519.PublicKey {
	out := map[string]ed25519.PublicKey{}
	for kid, s := range in {
		k, err := entitle.ParseKey(s)
		if err != nil {
			t.Fatal(err)
		}
		out[kid] = k
	}
	if len(out) == 0 {
		t.Fatal("/v1/keys published no keys")
	}
	return out
}

// config is canary.yaml for this rig.
func (r *rig) config() *Config {
	enc := func(m map[string]ed25519.PublicKey) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = entitle.EncodeKey(v)
		}
		return out
	}
	c := &Config{
		API: r.origin, DataDir: r.t.TempDir(),
		EntitlementKeys: enc(r.ent), DenyListKeys: enc(r.dl),
		Twin: TwinConfig{Handle: canaryHandle, ACMEDirectory: r.ca.Directory(), StatusListen: "127.0.0.1:0", PinSettle: time.Nanosecond},
		Probe: ProbeConfig{Region: "here", Host: canaryHost, Timeout: 5 * time.Second,
			Relays: []ProbeRelay{{ID: "r1", Addrs: []string{r.relays[0].addr}}, {ID: "r2", Addrs: []string{r.relays[1].addr}}}},
	}
	c.defaults()
	return c
}

func TestCanaryEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a control plane, two relays and a CA")
	}
	r := newRig(t)
	cfg := r.config()
	if err := cfg.check(roleTwin, roleProbe); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := twinDeps{ACMEClient: r.ca.Client(), RelayRoots: r.rca.pool, Log: testLog(t)}

	// Link: the fake merchant of record's checkout page pays when opened.
	var out strings.Builder
	open := func(u string) error {
		res, err := http.Get(u)
		if err != nil {
			return err
		}
		res.Body.Close()
		return nil
	}
	if err := link(ctx, cfg, deps, &out, open, 50*time.Millisecond, 30*time.Second); err != nil {
		t.Fatalf("link: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Linked as "+canaryHandle) {
		t.Fatalf("link said:\n%s", out.String())
	}
	rec, ok := r.dns.Records(canaryHandle)
	if !ok || len(rec.A) != 2 || rec.A[0].String() != "127.0.0.1" || len(rec.CAA) != 3 {
		t.Fatalf("the canary's DNS records: %+v", rec)
	}
	// Linking again is a no-op.
	out.Reset()
	if err := link(ctx, cfg, deps, &out, nil, time.Millisecond, time.Second); err != nil || !strings.Contains(out.String(), "Already linked") {
		t.Fatalf("second link: %v %q", err, out.String())
	}

	statusAddr := make(chan string, 1)
	deps.Status = func(a string) { statusAddr <- a }
	twinDone := make(chan error, 1)
	go func() { twinDone <- runTwin(ctx, cfg, deps) }()
	var status string
	select {
	case status = <-statusAddr:
	case err := <-twinDone:
		t.Fatalf("the twin stopped: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the twin didn't start")
	}
	waitFor(t, 60*time.Second, "the canary healthy (both tunnels, a certificate)", func() bool {
		res, err := http.Get("http://" + status + "/healthz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})
	if n := len(r.ca.Issued()); n != 1 {
		t.Fatalf("%d certificates issued; the first order should pass CAA", n)
	}

	p := &prober{cfg: cfg.Probe, roots: r.ca.Roots, log: slog.New(slog.DiscardHandler), now: time.Now}
	rd := p.round(ctx)
	for _, x := range rd.Relays {
		if !x.OK || x.NotAfter.IsZero() {
			t.Fatalf("through %s: %+v", x.ID, x)
		}
	}
	if !rd.Relays[0].NotAfter.Equal(r.ca.Issued()[0].NotAfter) {
		t.Fatal("the probe saw another certificate than the canary's own")
	}

	// A name nobody holds gets nothing from a relay: zero bytes, no
	// certificate.
	stranger := &prober{cfg: cfg.Probe, roots: r.ca.Roots, now: time.Now}
	stranger.cfg.Host = "nobody-holds-this." + cloud.TenantZone
	if x := stranger.round(ctx); x.Relays[0].OK || x.Relays[1].OK {
		t.Fatalf("an unbound name answered: %+v", x)
	}

	// From here the path runs as production runs it: two regions' probes
	// serve their rounds over HTTP, the watcher reads them each minute
	// and pages a webhook and emails an SMTP server. The clock is shared
	// and moves a minute per round.
	clock := time.Now()
	now := func() time.Time { return clock }
	var probes []*prober
	var regions []Region
	for _, name := range []string{"a", "b"} {
		pc := cfg.Probe
		pc.Region = name
		pr := &prober{cfg: pc, roots: r.ca.Roots, log: slog.New(slog.DiscardHandler), now: now}
		srv := httptest.NewServer(pr.handler())
		t.Cleanup(srv.Close)
		probes = append(probes, pr)
		regions = append(regions, Region{Name: name, URL: srv.URL + "/v1/results"})
	}
	pager := newFakePager(t)
	mail := newFakeSMTP(t)
	wc := &Config{Probe: cfg.Probe, Watch: WatchConfig{Regions: regions,
		Page:  PageConfig{Format: "webhook", URL: pager.URL},
		Email: EmailConfig{SMTP: mail.addr, From: "canary@example.org", To: []string{"ops@example.org"}}}}
	wc.defaults()
	if err := wc.check(roleWatch); err != nil {
		t.Fatal(err)
	}
	w, err := newWatcher(wc, newNotifier(wc.Watch), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	w.now = now
	// minute runs one probe round in each region and one watcher round.
	minute := func() Round {
		var rd Round
		for _, pr := range probes {
			rd = pr.round(ctx)
		}
		w.tick(ctx)
		clock = clock.Add(time.Minute)
		return rd
	}
	pagerCounts := func() (int, int) {
		pager.mu.Lock()
		defer pager.mu.Unlock()
		return pager.triggers, pager.resolves
	}
	mailed := func(subject string) int {
		n := 0
		for _, m := range mail.messages() {
			if strings.Contains(m, "Subject: [mirrin ops] "+subject) {
				n++
			}
		}
		return n
	}

	// r1 stops: r2 still carries the canary at once, nobody is paged,
	// and relay-r1 is emailed.
	r.relays[0].stop()
	for range 4 {
		if rd := minute(); rd.ok("r1") || !rd.ok("r2") {
			t.Fatalf("with r1 stopped: %+v", rd)
		}
	}
	if tr, _ := pagerCounts(); tr != 0 || mailed("alert: Relay r1") != 1 {
		t.Fatalf("r1 down: %d pages, %d relay-r1 emails", tr, mailed("alert: Relay r1"))
	}
	// r1 comes back and the twin's tunnel returns within seconds.
	r.relays[0].start(r.relays[0].addr)
	waitFor(t, 30*time.Second, "r1 carrying the canary again", func() bool { return p.round(ctx).ok("r1") })

	// Game day: both relays down for six rounds pages exactly once, by
	// the pager and by email.
	r.relays[0].stop()
	r.relays[1].stop()
	for range 6 {
		if rd := minute(); rd.ok("r1") || rd.ok("r2") {
			t.Fatalf("with both stopped: %+v", rd)
		}
	}
	if tr, _ := pagerCounts(); tr != 1 || mailed("PAGE:") != 1 {
		t.Fatalf("both relays down for 6 rounds: %d pages, %d page emails, want 1", tr, mailed("PAGE:"))
	}
	r.relays[0].start(r.relays[0].addr)
	r.relays[1].start(r.relays[1].addr)
	waitFor(t, 30*time.Second, "both relays carrying the canary again", func() bool {
		rd := p.round(ctx)
		return rd.ok("r1") && rd.ok("r2")
	})
	for range 3 {
		minute()
	}
	if tr, res := pagerCounts(); tr != 1 || res != 1 {
		t.Fatalf("after recovery: %d pages, %d resolves", tr, res)
	}

	cancel()
	select {
	case err := <-twinDone:
		if err != nil {
			t.Fatalf("the twin: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the twin didn't stop")
	}
}

// testLog shows the twin's log with the test's output, which go test
// prints only when the test fails or runs with -v.
func testLog(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// relayCA is a throwaway CA for the relays' control name, "localhost".
type relayCA struct {
	cert tls.Certificate
	pool *x509.CertPool
}

func newRelayCA(t *testing.T) *relayCA {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relays test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &relayCA{cert: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, pool: pool}
}

// hostedRelay is one mirrin-relay in hosted mode, as r1 and r2 run.
type hostedRelay struct {
	t     *testing.T
	id    string
	addr  string
	cfg   relayserver.Config
	opts  relayserver.Options
	mu    sync.Mutex
	srv   *relayserver.Server
	alive bool
}

func startHostedRelay(t *testing.T, id string, ca *relayCA, ent, dl map[string]ed25519.PublicKey, deny *httptest.Server) *hostedRelay {
	t.Helper()
	cfg := relayserver.DefaultConfig()
	cfg.ID, cfg.ControlHostname, cfg.StateDir = id, "localhost", t.TempDir()
	cfg.HTTPListen, cfg.MetricsListen = "", ""
	cfg.Zones = []string{cloud.TenantZone}
	for kid, k := range ent {
		cfg.IssuerKeys = append(cfg.IssuerKeys, relayserver.IssuerKey{Kid: kid, Key: entitle.EncodeKey(k)})
	}
	for kid, k := range dl {
		cfg.IssuerKeys = append(cfg.IssuerKeys, relayserver.IssuerKey{Kid: kid, Key: entitle.EncodeKey(k)})
	}
	cfg.DenylistURL = deny.URL + "/v1/denylist"
	cert := ca.cert
	r := &hostedRelay{t: t, id: id, cfg: cfg, opts: relayserver.Options{
		HTTPClient:   deny.Client(),
		Certificates: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil },
		Log:          slog.New(slog.DiscardHandler),
	}}
	r.start("127.0.0.1:0")
	t.Cleanup(r.stop)
	return r
}

func (r *hostedRelay) start(addr string) {
	r.t.Helper()
	s, err := relayserver.New(r.cfg, r.opts)
	if err != nil {
		r.t.Fatal(err)
	}
	var ln net.Listener
	for range 100 { // the port a stopped relay held may take a moment to free
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		r.t.Fatal(err)
	}
	go s.Serve(ln)
	r.mu.Lock()
	r.srv, r.addr, r.alive = s, ln.Addr().String(), true
	r.mu.Unlock()
}

func (r *hostedRelay) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.alive {
		r.srv.Close()
		r.alive = false
	}
}

func (r *hostedRelay) running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.alive
}

func (r *hostedRelay) dial(ctx context.Context, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", r.addr)
}
