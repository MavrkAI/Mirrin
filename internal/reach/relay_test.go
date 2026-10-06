package reach

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/relay/server"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr/acmetest"
)

// tenant is the name the test relay routes to the daemon. Nothing resolves
// it: every client here dials the relay's address directly, as DNS would
// send it.
const tenant = "ember-otter-42.mirrin.test"

// testRelay is an in-process mirrin-relay (WP-14) in self-host mode on
// 127.0.0.1, with a throwaway CA for its control name "localhost".
type testRelay struct {
	addr  string
	roots *x509.CertPool
	srv   *server.Server
}

func startTestRelay(t *testing.T, allow ...server.Allow) *testRelay {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	cert := tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
	cfg := server.DefaultConfig()
	cfg.ID, cfg.ControlHostname, cfg.StateDir = "r1", "localhost", t.TempDir()
	cfg.HTTPListen, cfg.MetricsListen = "", ""
	cfg.Allow = allow
	s, err := server.New(cfg, server.Options{Certificates: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &testRelay{addr: ln.Addr().String(), roots: pool, srv: s}
}

func (r *testRelay) tunnelURL() string {
	_, port, _ := net.SplitHostPort(r.addr)
	return "wss://localhost:" + port + wire.Path
}

// dial reaches any name at the relay, as DNS pointing the name there would.
func (r *testRelay) dial(ctx context.Context, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", r.addr)
}

func pubKey(k ed25519.PrivateKey) string { return entitle.EncodeKey(k.Public().(ed25519.PublicKey)) }

// fakeCT is a CT aggregator whose log the test writes.
type fakeCT struct {
	mu   sync.Mutex
	list []certwatch.Issuance
	down bool
}

func (f *fakeCT) Name() string { return "fake CT" }
func (f *fakeCT) Issuances(context.Context, string) ([]certwatch.Issuance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, io.ErrUnexpectedEOF
	}
	return append([]certwatch.Issuance(nil), f.list...), nil
}
func (f *fakeCT) add(is certwatch.Issuance) { f.mu.Lock(); f.list = append(f.list, is); f.mu.Unlock() }

type fakeScreen struct{ bus *events.Bus }

func (s fakeScreen) Screen(context.Context) any                                  { return map[string]string{} }
func (s fakeScreen) Events() *events.Bus                                         { return s.bus }
func (s fakeScreen) DecideApproval(context.Context, int64, bool) (string, error) { return "done", nil }
func (s fakeScreen) ScreenshotPath(string) (string, bool)                        { return "", false }

// The rig's approval is an ordinary write request, so another device may
// decide it without a passkey (dangerous ones need step-up, WP-06).
func (s fakeScreen) ApprovalDetail(_ context.Context, id int64) (api.ApprovalDetail, error) {
	return api.ApprovalDetail{ID: id, Tool: "send", Summary: "send a message", Risk: "write", Status: "pending"}, nil
}
func (s fakeScreen) DecideApprovalVia(ctx context.Context, id int64, approve bool, _ string) (string, error) {
	return s.DecideApproval(ctx, id, approve)
}

// rig is a daemon's relay reach against the test relay and the fake CA,
// with every outside service faked.
type rig struct {
	t      *testing.T
	dir    string
	relay  *testRelay
	ca     *acmetest.Server
	api    *api.Server
	store  *devices.Store
	ct     *fakeCT
	health *health.Monitor
	deps   Deps
	e      *Endpoint

	mu                     sync.Mutex
	notes, pushes, banners []string
}

type rigOpt func(*rig)

func startRig(t *testing.T, opts ...rigOpt) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), ca: acmetest.New(), ct: &fakeCT{}, health: health.New()}
	t.Cleanup(r.ca.Close)
	key, _, err := RelayKeys(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	r.relay = startTestRelay(t, server.Allow{Hostname: tenant, Key: pubKey(key)})
	r.ca.Dial = r.relay.dial
	if r.store == nil {
		r.store = devices.NewMemory()
	}
	for _, o := range opts {
		o(r)
	}
	r.api = api.New("127.0.0.1:0", "rig-master-token", rigBackend{}).WithDevices(r.store).WithScreen(fakeScreen{bus: events.New()})
	r.deps = Deps{
		Server: r.api, DataDir: r.dir, Health: r.health,
		Devices: func() *devices.Store { return r.store },
		Notify: func(_ context.Context, s string) error {
			r.mu.Lock()
			r.notes = append(r.notes, s)
			r.mu.Unlock()
			return nil
		},
		Push:       func(_, s string) { r.mu.Lock(); r.pushes = append(r.pushes, s); r.mu.Unlock() },
		Banner:     func(s string) { r.mu.Lock(); r.banners = append(r.banners, s); r.mu.Unlock() },
		CTSources:  []certwatch.CTSource{r.ct},
		CAA:        r.caa,
		ACMEClient: r.ca.Client(),
		RelayRoots: r.relay.roots,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.Reach{Mode: "relay", RelayURL: r.relay.tunnelURL(), Hostname: tenant, ACMEDirectory: r.ca.Directory()}
	e, err := StartRelay(ctx, cfg, r.deps)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	r.mu.Lock()
	r.e = e
	r.mu.Unlock()
	r.deps.Alarm = e.Alarm
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.Done():
		case <-time.After(10 * time.Second):
			t.Error("the relay endpoint didn't stop")
		}
	})
	waitFor(t, 20*time.Second, "the first certificate through the relay", func() bool { return e.ACME.Leaf() != nil })
	return r
}

// caa answers as the owner's DNS would once they published the records.
func (r *rig) caa(_ context.Context, name string) ([]certwatch.CAA, error) {
	r.mu.Lock()
	e := r.e
	r.mu.Unlock()
	if e == nil || e.ACME.AccountURI() == "" {
		return nil, nil
	}
	return []certwatch.CAA{
		{Name: name, Tag: "issue", Value: "acmetest.invalid; accounturi=" + e.ACME.AccountURI() + "; validationmethods=tls-alpn-01"},
		{Name: name, Tag: "issuewild", Value: ";"},
	}, nil
}

// phone is an HTTPS client that reaches tenant through the relay and
// trusts the fake CA, as a phone on cellular would.
func (r *rig) phone() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return r.relay.dial(ctx, "") },
		TLSClientConfig: &tls.Config{RootCAs: r.ca.Roots, ServerName: tenant},
	}}
}

func (r *rig) do(method, path, token, accept string) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest(method, "https://"+tenant+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	res, err := r.phone().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// message posts text to /message as a phone on the public name would, and
// returns the reply.
func (r *rig) message(token, text string) (int, string) {
	r.t.Helper()
	b, _ := json.Marshal(map[string]string{"channel": "screen", "chat_id": "local", "text": text})
	req, _ := http.NewRequest("POST", "https://"+tenant+"/message", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := r.phone().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	var m api.MessageResponse
	raw := body(res)
	_ = json.Unmarshal([]byte(raw), &m)
	if m.Reply == "" {
		return res.StatusCode, raw
	}
	return res.StatusCode, m.Reply
}

// rigBackend stands in for the daemon behind /message: it says whether the
// request came marked as held by the certificate alarm, which is what the
// daemon refuses a decision in words by (daemon/reachrelay.go).
type rigBackend struct{}

func (rigBackend) reply(ctx context.Context) string {
	if api.ApprovalsPaused(ctx) {
		return "held"
	}
	return "free"
}
func (rigBackend) Status(context.Context) api.Status { return api.Status{} }
func (b rigBackend) Message(ctx context.Context, _ channels.Inbound) (string, error) {
	return b.reply(ctx), nil
}
func (b rigBackend) MessageStreaming(ctx context.Context, _ channels.Inbound, _ func(string)) (string, error) {
	return b.reply(ctx), nil
}
func (b rigBackend) MessageEvents(ctx context.Context, _ channels.Inbound, _ agent.Events) (string, error) {
	return b.reply(ctx), nil
}
func (rigBackend) SetPaused(bool)                            {}
func (rigBackend) RunProtocol(context.Context, string) error { return nil }
func (rigBackend) RunJob(context.Context, string) error      { return nil }

func body(res *http.Response) string {
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return string(b)
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRelayReachEndToEnd(t *testing.T) {
	r := startRig(t)
	e := r.e
	// Issued by TLS-ALPN-01 through the relay, from every perspective.
	vs := r.ca.Validations()
	if len(vs) == 0 {
		t.Fatal("no validation")
	}
	for _, v := range vs {
		if v.Err != nil || v.Name != tenant {
			t.Fatalf("validation %+v", v)
		}
	}
	res := r.do("GET", "/healthz", "", "")
	if res.StatusCode != 200 {
		t.Fatalf("healthz %d %s", res.StatusCode, body(res))
	}
	if got := res.TLS.PeerCertificates[0]; !bytes.Equal(got.Raw, e.ACME.Leaf().Raw) {
		t.Fatal("the phone saw another certificate")
	}
	body(res)
	if st, detail, _ := relayHealth(e); st != health.OK || !strings.Contains(detail, tenant) {
		t.Fatalf("health %v %q", st, detail)
	}
	// Verify from outside passes every check.
	e.Roots, e.Dial = r.ca.Roots, r.relay.dial
	rep, err := Verify(context.Background(), e)
	if err != nil || !rep.OK {
		t.Fatalf("verify %+v %v", rep, err)
	}
	var names []string
	for _, c := range rep.Checks {
		names = append(names, c.Name)
		if c.Warn {
			t.Fatalf("warning %+v", c)
		}
	}
	if strings.Join(names, ",") != "relay,certificate,chain,expiry,twin,caa" {
		t.Fatalf("checks %v", names)
	}
	// The watcher sees this machine's own certificate and stays quiet.
	leaf := e.ACME.Leaf()
	r.ct.add(certwatch.Issuance{Source: "fake CT", ID: "1", DNSNames: leaf.DNSNames, SPKI: rep.Served, NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter})
	if fs := e.Watcher.Check(context.Background()); len(fs) != 0 {
		t.Fatalf("findings on our own certificate: %+v", fs)
	}
	if st, _, _ := e.Alarm.Health(context.Background()); st != health.OK {
		t.Fatal("alarm without a finding")
	}
}

func TestRelayRenewalKeepsKeyThroughRelay(t *testing.T) {
	r := startRig(t)
	first := r.e.ACME.Leaf()
	if err := r.e.ACME.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	res := r.do("GET", "/healthz", "", "")
	got := res.TLS.PeerCertificates[0]
	body(res)
	if bytes.Equal(got.Raw, first.Raw) || !bytes.Equal(got.RawSubjectPublicKeyInfo, first.RawSubjectPublicKeyInfo) {
		t.Fatal("renewal through the relay didn't keep the key, or didn't renew")
	}
}

func TestBYODRecords(t *testing.T) {
	acct := "https://acme-v02.api.letsencrypt.org/acme/acct/123456789"
	recs := BYODRecords("Twin.Example.com.", acct, []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("::ffff:198.51.100.2"), netip.MustParseAddr("2001:db8::7")})
	var lines []string
	for _, r := range recs {
		if r.Type == "CNAME" {
			t.Fatal("CNAME")
		}
		lines = append(lines, r.String())
	}
	want := []string{
		"twin.example.com. 300 IN A 203.0.113.7",
		"twin.example.com. 300 IN A 198.51.100.2",
		"twin.example.com. 300 IN AAAA 2001:db8::7",
		`_mirrin.twin.example.com. 300 IN TXT "v=mirrin1; acct=123456789"`,
		`twin.example.com. 300 IN CAA 0 issue "letsencrypt.org; accounturi=https://acme-v02.api.letsencrypt.org/acme/acct/123456789; validationmethods=tls-alpn-01"`,
		`twin.example.com. 300 IN CAA 0 issuewild ";"`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("records:\n%s", strings.Join(lines, "\n"))
	}
	// The printed CAA records are what the watcher accepts.
	var caa []certwatch.CAA
	for _, r := range recs {
		if r.Type == "CAA" {
			_, rest, _ := strings.Cut(r.Value, " ")
			tag, value, _ := strings.Cut(rest, " ")
			caa = append(caa, certwatch.CAA{Tag: tag, Value: strings.Trim(value, `"`)})
		}
	}
	if v := certwatch.CheckCAA(caa, acct); !v.OK {
		t.Fatalf("our own records don't check: %+v", v)
	}
	if caaIssuer("https://localhost:14000/my-account/1") != "localhost" || caaIssuer("https://acme.example.co/acct/1") != "example.co" {
		t.Fatal("issuer domain")
	}
}

func TestRelayKeysPersistAndAllowLine(t *testing.T) {
	dir := t.TempDir()
	k1, s1, err := RelayKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	k2, s2, err := RelayKeys(dir)
	if err != nil || !k1.Equal(k2) || !bytes.Equal(s1, s2) || len(s1) != wire.StatusKeySize {
		t.Fatal("keys not kept")
	}
	line := AllowLine(tenant, k1)
	if line != "  - {hostname: "+tenant+", key: "+pubKey(k1)+"}" {
		t.Fatal(line)
	}
	// The relay's own config parser takes the line as printed.
	if _, err := server.ParseConfig([]byte("id: r1\ncontrol_hostname: relay.example.com\nstate_dir: /var/lib/mirrin-relay\nallow:\n" + line + "\n")); err != nil {
		t.Fatalf("relay.yaml rejects the allow line: %v", err)
	}
}

func TestCheckRelayConfig(t *testing.T) {
	for _, c := range []config.Reach{
		{RelayURL: "https://relay.example.com/v1/tunnel", Hostname: tenant},
		{RelayURL: "wss://relay.example.com/v1/tunnel", Hostname: ""},
		{RelayURL: "wss://relay.example.com/v1/tunnel", Hostname: "*.example.com"},
		{RelayURL: "wss://", Hostname: tenant},
	} {
		if _, err := CheckRelayConfig(c); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}
