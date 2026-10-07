package reach

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/cloud/cloudtest"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/relay"
	"github.com/MavrkAI/Mirrin/internal/relay/server"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
	"github.com/MavrkAI/Mirrin/internal/skills/phone"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr/acmetest"
)

// The paid journey on one laptop: a fake control plane (cloudtest, which
// the real one passes the same contract as), two hosted mirrin-relays in
// this process, and a fake ACME CA that checks the handle's CAA pin. Every
// client dials 127.0.0.1, as DNS for the handle would send it to either
// relay.

const cloudHandle = "ember-otter-42"

var cloudHost = cloudHandle + "." + cloud.TenantZone

// relayCA is a throwaway CA for the relays' own control name, "localhost".
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

// hostedRelay is one mirrin-relay in hosted mode: it takes entitlements
// signed by the fake control plane's keys, for handles in the tenant zone,
// and polls the fake's deny list.
type hostedRelay struct {
	t     *testing.T
	id    string
	addr  string
	cfg   server.Config
	opts  server.Options
	mu    sync.Mutex
	srv   *server.Server
	ln    net.Listener
	alive bool
}

func startHostedRelay(t *testing.T, id string, ca *relayCA, ent, dl map[string]ed25519.PublicKey, deny *httptest.Server) *hostedRelay {
	t.Helper()
	cfg := server.DefaultConfig()
	cfg.ID, cfg.ControlHostname, cfg.StateDir = id, "localhost", t.TempDir()
	cfg.HTTPListen, cfg.MetricsListen = "", ""
	cfg.Zones = []string{cloud.TenantZone}
	for kid, k := range ent {
		cfg.IssuerKeys = append(cfg.IssuerKeys, server.IssuerKey{Kid: kid, Key: entitle.EncodeKey(k)})
	}
	for kid, k := range dl {
		cfg.IssuerKeys = append(cfg.IssuerKeys, server.IssuerKey{Kid: kid, Key: entitle.EncodeKey(k)})
	}
	cfg.DenylistURL = deny.URL + "/v1/denylist"
	cert := ca.cert
	r := &hostedRelay{t: t, id: id, cfg: cfg, opts: server.Options{
		HTTPClient:   deny.Client(),
		Certificates: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil },
	}}
	r.start("127.0.0.1:0")
	t.Cleanup(r.stop)
	return r
}

func (r *hostedRelay) start(addr string) {
	r.t.Helper()
	s, err := server.New(r.cfg, r.opts)
	if err != nil {
		r.t.Fatal(err)
	}
	var ln net.Listener
	for range 50 { // the port a stopped relay held may take a moment to free
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
	r.srv, r.ln, r.addr, r.alive = s, ln, ln.Addr().String(), true
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

func (r *hostedRelay) tunnelURL() string {
	_, port, _ := net.SplitHostPort(r.addr)
	return "wss://localhost:" + port + wire.Path
}

func (r *hostedRelay) dial(ctx context.Context, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", r.addr)
}

// streamer answers /message/stream in three ordered deltas.
type streamer struct{ rigBackend }

func (streamer) MessageEvents(_ context.Context, _ channels.Inbound, ev agent.Events) (string, error) {
	for _, d := range []string{"one ", "two ", "three"} {
		if ev.OnDelta != nil {
			ev.OnDelta(d)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "one two three", nil
}

// cloudRig is a linked daemon's cloud reach against the fake control plane,
// two relays and the fake CA.
type cloudRig struct {
	t      *testing.T
	dir    string
	fake   *cloudtest.Fake
	client *cloud.Client
	relays []*hostedRelay
	ca     *acmetest.Server
	rca    *relayCA
	api    *api.Server
	bus    *events.Bus
	store  *devices.Store
	phone  *phone.Client
	deps   Deps
	cancel context.CancelFunc

	mu    sync.Mutex
	e     *Endpoint
	notes []string
	now   atomic.Pointer[time.Time] // nil: the real clock
}

func (r *cloudRig) clock() time.Time {
	if p := r.now.Load(); p != nil {
		return *p
	}
	return time.Now()
}

func (r *cloudRig) endpoint() *Endpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.e
}

// startCloudRig links a machine with the fake, starts both relays and the
// CA, and starts cloud reach, waiting for the first certificate.
func startCloudRig(t *testing.T) *cloudRig {
	t.Helper()
	r := &cloudRig{t: t, dir: t.TempDir(), fake: cloudtest.NewFake(t), ca: acmetest.New(), rca: newRelayCA(t), bus: events.New(), store: devices.NewMemory()}
	t.Cleanup(r.ca.Close)
	deny := httptest.NewTLSServer(r.fake.Handler())
	t.Cleanup(deny.Close)
	ent, dl := r.fake.Keys(), r.fake.DenyListKeys()
	r.relays = []*hostedRelay{startHostedRelay(t, "r1", r.rca, ent, dl, deny), startHostedRelay(t, "r2", r.rca, ent, dl, deny)}
	var refs []entitle.Relay
	for _, h := range r.relays {
		refs = append(refs, entitle.Relay{ID: h.id, URL: h.tunnelURL(), IPs: []string{"127.0.0.1"}})
	}
	r.fake.SetRelays(refs)

	// The CA reaches the handle through either relay, and issues only to
	// the ACME account the handle's CAA record names, as Let's Encrypt
	// checks accounturi.
	var validations atomic.Int64
	r.ca.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
		return r.relays[validations.Add(1)%2].dial(ctx, addr)
	}
	r.ca.CAA = func(name, account string) error {
		pinned, _ := r.fake.ACMEAccount(strings.TrimSuffix(name, "."+cloud.TenantZone))
		if pinned != account {
			return fmt.Errorf("CAA names %q, not %q", pinned, account)
		}
		return nil
	}

	c, err := cloud.New(r.dir, r.fake.URL, r.fake.Keys())
	if err != nil {
		t.Fatal(err)
	}
	r.client = c
	ls, err := c.StartLink(t.Context(), cloudHandle, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.fake.Pay(ls.ID); err != nil {
		t.Fatal(err)
	}
	if st, _, err := c.PollLink(t.Context(), ls.ID); err != nil || st != cloud.LinkActive {
		t.Fatalf("link: %q %v", st, err)
	}

	r.phone = phone.New(func() config.Phone {
		return config.Phone{AccountSID: "AC123", AuthToken: "twilio-secret", From: "+15550100"}
	})
	r.phone.PublicURL = func() string { return r.endpoint().PublicURL() }
	r.api = api.New("127.0.0.1:0", "cloud-rig-master", streamer{}).WithDevices(r.store).WithScreen(fakeScreen{bus: r.bus}).WithPublic("/phone/", r.phone.Webhook)
	r.deps = Deps{
		Server: r.api, DataDir: r.dir, Health: health.New(),
		Devices: func() *devices.Store { return r.store },
		Notify: func(_ context.Context, s string) error {
			r.mu.Lock()
			r.notes = append(r.notes, s)
			r.mu.Unlock()
			return nil
		},
		CTSources:    []certwatch.CTSource{&fakeCT{}},
		CAA:          r.caa,
		ACMEClient:   r.ca.Client(),
		RelayRoots:   r.rca.pool,
		Now:          r.clock,
		Reach:        config.Reach{Mode: "cloud", ACMEDirectory: r.ca.Directory()},
		CloudCheck:   20 * time.Millisecond,
		PinSettle:    time.Nanosecond,
		ConfirmEvery: 50 * time.Millisecond,
	}
	r.start()
	waitFor(t, 30*time.Second, "the handle's first certificate", func() bool { return r.endpoint().ACME.Leaf() != nil })
	return r
}

// start starts (or starts again) cloud reach.
func (r *cloudRig) start() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e, err := StartCloud(ctx, r.client, nil, r.deps)
	if err != nil {
		cancel()
		r.t.Fatal(err)
	}
	r.mu.Lock()
	r.e, r.cancel = e, cancel
	r.mu.Unlock()
	r.t.Cleanup(func() {
		cancel()
		select {
		case <-e.Done():
		case <-time.After(15 * time.Second):
			r.t.Error("the cloud endpoint didn't stop")
		}
	})
}

// caa answers as the tenant zone's DNS would: what the control plane wrote.
func (r *cloudRig) caa(_ context.Context, name string) ([]certwatch.CAA, error) {
	acct, ok := r.fake.ACMEAccount(strings.TrimSuffix(name, "."+cloud.TenantZone))
	if !ok || acct == "" {
		return nil, nil
	}
	return []certwatch.CAA{
		{Name: name, Tag: "issue", Value: "acmetest.invalid; accounturi=" + acct + "; validationmethods=tls-alpn-01"},
		{Name: name, Tag: "issuewild", Value: ";"},
	}, nil
}

// phoneVia is an HTTPS client for the handle through relay i, trusting the
// fake CA, as a phone on cellular would be.
func (r *cloudRig) phoneVia(i int) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return r.relays[i].dial(ctx, "") },
		TLSClientConfig: &tls.Config{RootCAs: r.ca.Roots, ServerName: cloudHost},
	}}
}

func (r *cloudRig) get(i int, path, token string) (*http.Response, error) {
	req, _ := http.NewRequest("GET", "https://"+cloudHost+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := r.phoneVia(i).Do(req.WithContext(ctx))
	if err == nil {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		res.Body = io.NopCloser(bytes.NewReader(b))
	}
	return res, err
}

func (r *cloudRig) online() []string {
	var out []string
	for _, s := range r.endpoint().Listener.Status() {
		if s.Online {
			out = append(out, s.Relay)
		}
	}
	sort.Strings(out)
	return out
}

func (r *cloudRig) pairPhone() string {
	r.t.Helper()
	_, tok, err := r.store.Add("Akshay's iPhone", devices.KindPWA, []devices.Scope{devices.View, devices.Chat, devices.Approve}, "cloud", "")
	if err != nil {
		r.t.Fatal(err)
	}
	return tok
}

// The whole journey: link, entitlement, both tunnels, the ACME account
// pinned before the certificate, and a phone that loads the app, holds the
// event stream for a minute and gets a streamed reply in order, over the
// handle, from this machine's own key.
func TestCloudReachEndToEnd(t *testing.T) {
	t.Parallel()
	r := startCloudRig(t)
	e := r.endpoint()
	waitFor(t, 10*time.Second, "both tunnels", func() bool { return strings.Join(r.online(), ",") == "r1,r2" })

	// The certificate: TLS-ALPN-01 through the relays, for the handle, from
	// the account the control plane was told before the first order.
	for _, v := range r.ca.Validations() {
		if v.Err != nil || v.Name != cloudHost {
			t.Fatalf("validation %+v", v)
		}
	}
	acct := e.ACME.AccountURI()
	if pinned, _ := r.fake.ACMEAccount(cloudHandle); pinned == "" || pinned != acct {
		t.Fatalf("CAA pins %q, machine's account %q", pinned, acct)
	}
	if PinnedAccount(r.dir, cloudHandle) != acct {
		t.Fatal("the pin wasn't recorded, so it would be sent again")
	}
	if len(r.ca.Issued()) != 1 {
		t.Fatalf("%d certificates issued; the first order should have succeeded", len(r.ca.Issued()))
	}
	puts := 0
	for _, q := range r.fake.Requests() {
		if q.Method == "PUT" && q.Path == "/v1/acme-account" {
			puts++
		}
	}
	if puts != 1 {
		t.Fatalf("%d PUT /v1/acme-account, want 1", puts)
	}

	tok := r.pairPhone()
	for i := range r.relays {
		res, err := r.get(i, "/ui", tok)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 {
			t.Fatalf("/ui via %s: %d", r.relays[i].id, res.StatusCode)
		}
		if got := res.TLS.PeerCertificates[0]; !bytes.Equal(got.Raw, e.ACME.Leaf().Raw) {
			t.Fatal("the phone saw a certificate that isn't this machine's")
		}
		if tlsmgr.SPKIPin(res.TLS.PeerCertificates[0]) != e.Pins()[0] {
			t.Fatal("the served key isn't the current pin")
		}
	}

	// /events stays open for a minute through the relay, with its
	// keepalives, and still carries the next event.
	hold := 60 * time.Second
	if testing.Short() {
		hold = 3 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+cloudHost+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := r.phoneVia(0).Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("/events: %v %v", res, err)
	}
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	// Meanwhile a streamed reply arrives in order over the other relay.
	b, _ := json.Marshal(map[string]string{"channel": "screen", "chat_id": "local", "text": "count to three"})
	sreq, _ := http.NewRequest("POST", "https://"+cloudHost+"/message/stream", bytes.NewReader(b))
	sreq.Header.Set("Authorization", "Bearer "+tok)
	sreq.Header.Set("Content-Type", "application/json")
	sres, err := r.phoneVia(1).Do(sreq)
	if err != nil || sres.StatusCode != 200 {
		t.Fatalf("/message/stream: %v %v", sres, err)
	}
	var deltas []string
	sc := bufio.NewScanner(sres.Body)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok && strings.Contains(d, `"text"`) && !strings.Contains(d, "reply") {
			var m map[string]string
			json.Unmarshal([]byte(d), &m)
			deltas = append(deltas, m["text"])
		}
	}
	sres.Body.Close()
	if strings.Join(deltas, "") != "one two three" {
		t.Fatalf("deltas %q", deltas)
	}

	deadline := time.After(hold)
	keepalives := 0
wait:
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("/events closed before the minute was up")
			}
			if l == ": keepalive" {
				keepalives++
			}
		case <-deadline:
			break wait
		}
	}
	r.bus.Publish(events.Event{Kind: "notice", Text: "still here"})
	got := false
	timeout := time.After(10 * time.Second)
	for !got {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("/events closed")
			}
			got = strings.Contains(l, "still here")
		case <-timeout:
			t.Fatal("the event after the hold didn't arrive")
		}
	}
	if !testing.Short() && keepalives < 2 {
		t.Fatalf("%d keepalives in a minute", keepalives)
	}
	cancel()

	// The relays' status endpoints know the twin is online, under this
	// twin's status key only.
	urls := e.StatusURLs()
	if len(urls) != 2 {
		t.Fatalf("status urls %v", urls)
	}
	sc2 := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: r.rca.pool}}}
	for _, u := range urls {
		res, err := sc2.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Online bool `json:"online"`
		}
		json.NewDecoder(res.Body).Decode(&st)
		res.Body.Close()
		if res.StatusCode != 200 || !st.Online {
			t.Fatalf("%s: %d online=%v", u, res.StatusCode, st.Online)
		}
		pu, _ := url.Parse(u)
		q := pu.Query()
		q.Set("k", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
		pu.RawQuery = q.Encode()
		if res, err := sc2.Get(pu.String()); err != nil || res.StatusCode != 404 {
			t.Fatalf("a wrong status key: %v %v", res, err)
		}
	}

	// Health says so, and verify from outside passes.
	if st, detail, _ := CloudHealth(e); st != health.OK || !strings.Contains(detail, cloudHost) {
		t.Fatalf("health %v %q", st, detail)
	}
	e.Roots, e.Dial = r.ca.Roots, r.relays[1].dial
	rep, err := Verify(context.Background(), e)
	if err != nil || !rep.OK {
		t.Fatalf("verify %+v %v", rep, err)
	}
	if ready, detail := CloudStatus(e); !ready {
		t.Fatalf("not ready: %s", detail)
	}
}

// With phone.public_url empty, the phone skill uses the handle while reach
// is online: Twilio's webhook arrives over the relays at that address, and
// its signature checks against it.
func TestCloudPhoneWebhookUsesTheHandle(t *testing.T) {
	t.Parallel()
	r := startCloudRig(t)
	waitFor(t, 10*time.Second, "a tunnel", func() bool { return r.endpoint().PublicURL() != "" })
	if got := r.endpoint().PublicURL(); got != "https://"+cloudHost {
		t.Fatalf("public URL %q", got)
	}
	form := url.Values{"CallSid": {"CA1"}, "CallStatus": {"completed"}}
	path := "/phone/status?call=nope"
	post := func(sig string) int {
		req, _ := http.NewRequest("POST", "https://"+cloudHost+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Twilio-Signature", sig)
		res, err := r.phoneVia(0).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post(twilioSign("twilio-secret", "https://"+cloudHost+path, form)); code != 200 {
		t.Fatalf("signed for the handle: %d", code)
	}
	if code := post(twilioSign("twilio-secret", "https://elsewhere.example"+path, form)); code != http.StatusForbidden {
		t.Fatalf("signed for another address: %d", code)
	}
}

// twilioSign is Twilio's X-Twilio-Signature.
func twilioSign(token, fullURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(fullURL)
	for _, k := range keys {
		b.WriteString(k + form.Get(k))
	}
	mac := hmac.New(sha1.New, []byte(token))
	mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Dual-homed: with one relay down the phone still gets through the other,
// and the tunnel to the relay that comes back is up within 5 s of its
// restart, however long it was gone.
func TestCloudOneRelayDownTheOtherCarries(t *testing.T) {
	t.Parallel()
	r := startCloudRig(t)
	waitFor(t, 10*time.Second, "both tunnels", func() bool { return strings.Join(r.online(), ",") == "r1,r2" })
	tok := r.pairPhone()
	addr := r.relays[0].addr
	r.relays[0].stop()
	waitFor(t, 10*time.Second, "r1 down", func() bool { return strings.Join(r.online(), ",") == "r2" })
	for range 3 {
		res, err := r.get(1, "/status", tok)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("through r2 while r1 is down: %v %v", res, err)
		}
	}
	if st, _, _ := CloudHealth(r.endpoint()); st != health.OK {
		t.Fatal("one relay is enough")
	}
	time.Sleep(6 * time.Second) // long enough for the backoff to grow
	r.relays[0].start(addr)
	restarted := time.Now()
	waitFor(t, 10*time.Second, "r1 back", func() bool { return strings.Join(r.online(), ",") == "r1,r2" })
	if took := time.Since(restarted); took > 5*time.Second {
		t.Fatalf("reconnected %v after the restart, want within 5 s", took)
	}
	if res, err := r.get(0, "/status", tok); err != nil || res.StatusCode != 200 {
		t.Fatalf("through r1 again: %v %v", res, err)
	}
}

// intruder connects to every relay with key at gen, as another machine
// holding the handle would, and returns once it is online.
func (r *cloudRig) intruder(key ed25519.PrivateKey, gen int64, iat time.Time) *relay.Listener {
	r.t.Helper()
	var refs []relay.RelayRef
	var ents []entitle.Relay
	for _, h := range r.relays {
		refs = append(refs, relay.RelayRef{ID: h.id, URL: h.tunnelURL()})
		ents = append(ents, entitle.Relay{ID: h.id, URL: h.tunnelURL()})
	}
	tok, err := entitle.Sign(entitle.Claims{
		Iss: "127.0.0.1", Sub: "acct_x", Aud: entitle.Audience, Iat: iat, Nbf: iat.Add(-time.Minute), Exp: iat.Add(24 * time.Hour), PaidThrough: iat.Add(24 * time.Hour),
		Gen: gen, Plan: "cloud", Feat: []string{"reach"}, Handle: cloudHandle, Hosts: []string{cloudHost},
		Cnf: entitle.EncodeKey(key.Public().(ed25519.PublicKey)), Relays: ents,
	}, cloudtest.EntitlementKid, cloudtest.DevKey(cloudtest.EntitlementKid))
	if err != nil {
		r.t.Fatal(err)
	}
	l, err := relay.Listen(context.Background(), relay.ClientConfig{Relays: refs, Key: key, StatusKey: make([]byte, 32), Entitlement: func() string { return tok }, RootCAs: r.rca.pool})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { l.Close() })
	waitFor(r.t, 10*time.Second, "the other machine's tunnels", func() bool {
		n := 0
		for _, s := range l.Status() {
			if s.Online {
				n++
			}
		}
		return n == len(refs)
	})
	return l
}

// A recovery on another machine raises the handle's generation. The relays
// hand it the name and tell this machine superseded; the control plane
// confirms, and this machine stands by: the endpoint stops, says why, and
// the refresh loop and relays hear nothing more from it.
func TestCloudLowerGenStandsBy(t *testing.T) {
	t.Parallel()
	r := startCloudRig(t)
	waitFor(t, 10*time.Second, "both tunnels", func() bool { return strings.Join(r.online(), ",") == "r1,r2" })
	gen, err := r.fake.Supersede(cloudHandle)
	if err != nil || gen != 2 {
		t.Fatal(gen, err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	r.intruder(other, gen, time.Now())
	e := r.endpoint()
	select {
	case <-e.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("still running after another machine took over")
	}
	end, at := e.Ended()
	if end != CloudStandby || at.IsZero() {
		t.Fatalf("ended %v at %v", end, at)
	}
	// The state is saved as the endpoint stops, not before it.
	waitFor(t, 10*time.Second, "the state to say superseded", func() bool {
		_, kind := r.client.State().Current(time.Now())
		return kind == cloud.Superseded
	})
	// The endpoint that stood by sends nothing more, though the other
	// relay's refusal of the same takeover comes in meanwhile.
	quiet := len(r.fake.Requests())
	time.Sleep(3 * time.Second)
	if n := len(r.fake.Requests()) - quiet; n != 0 {
		t.Fatalf("%d requests after standing by", n)
	}
	// Starting again doesn't: the link says standby, and nothing is sent.
	before := len(r.fake.Requests())
	if _, err := StartCloud(context.Background(), r.client, nil, r.deps); !isStop(err, CloudStandby) {
		t.Fatalf("start again: %v", err)
	}
	if len(r.fake.Requests()) != before {
		t.Fatal("a machine standing by contacted the control plane")
	}
	if !strings.Contains(MovedLine("Mirrin", at), "Mirrin moved to another machine on ") {
		t.Fatal(MovedLine("Mirrin", at))
	}
}

// A takeover while the control plane can't be reached: the relays say
// superseded, and this machine keeps asking the control plane, with
// growing waits, until it answers, then stands by. It never runs on
// half-stopped as if nothing happened.
func TestCloudSupersedeConfirmedThroughAnOutage(t *testing.T) {
	t.Parallel()
	r := startCloudRig(t)
	waitFor(t, 10*time.Second, "both tunnels", func() bool { return strings.Join(r.online(), ",") == "r1,r2" })
	gen, err := r.fake.Supersede(cloudHandle)
	if err != nil {
		t.Fatal(err)
	}
	r.fake.SetDown(true)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	r.intruder(other, gen, time.Now())
	waitFor(t, 10*time.Second, "two unanswered confirmations", func() bool {
		n := 0
		for _, q := range r.fake.Requests() {
			if q.Path == "/v1/entitlement/refresh" && q.Status == 503 {
				n++
			}
		}
		return n >= 2
	})
	e := r.endpoint()
	if end, _ := e.Ended(); end != CloudRunning {
		t.Fatalf("ended %v without the control plane's word", end)
	}
	r.fake.SetDown(false)
	select {
	case <-e.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("still running once the control plane confirmed the takeover")
	}
	if end, _ := e.Ended(); end != CloudStandby {
		t.Fatalf("ended %v", end)
	}
}

// A relay says superseded but the control plane still names this machine:
// the endpoint starts again (the relay stopped that tunnel for good), and
// never stands by on a relay's word alone.
func TestCloudSupersededButStillOursRestarts(t *testing.T) {
	t.Parallel()
	r := startCloudRig(t)
	waitFor(t, 10*time.Second, "both tunnels", func() bool { return strings.Join(r.online(), ",") == "r1,r2" })
	held, _ := r.client.State().Current(time.Now())
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	r.intruder(other, held.Gen+1, time.Now()) // not what the control plane says
	e := r.endpoint()
	select {
	case <-e.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("a tunnel stopped for good and the endpoint ran on")
	}
	if end, _ := e.Ended(); end != CloudRestart {
		t.Fatalf("ended %v", end)
	}
}

// Refusals that only ask for a refresh back off: a relay that keeps
// refusing can't make this machine ask the control plane every 20 s.
func TestRefusalRefreshesBackOff(t *testing.T) {
	f, c := linkedFake(t)
	now := time.Now()
	ce := &cloudEndpoint{client: c, state: c.State(), log: slog.New(slog.DiscardHandler), d: Deps{Now: func() time.Time { return now }}}
	e := &Endpoint{done: make(chan struct{}), stop: func() {}, cloud: ce}
	refuse := func() { ce.refused(e, "r1", wire.Error{Code: wire.CodeEntitlementExpired}) }
	before := refreshes(f)
	for _, step := range []struct {
		after time.Duration
		want  int
	}{{0, 1}, {2 * refreshGap, 2}, {2 * refreshGap, 2}, {2 * refreshGap, 3}, {5 * refreshGap, 3}, {3 * refreshGap, 4}} {
		now = now.Add(step.after)
		refuse()
		if got := refreshes(f) - before; got != step.want {
			t.Fatalf("after +%v: %d refreshes, want %d", step.after, got, step.want)
		}
	}
	if ce.gap != 16*refreshGap {
		t.Fatalf("gap %v", ce.gap)
	}
}

// linkedFake is a machine linked with a fake control plane, without relays.
func linkedFake(t *testing.T) (*cloudtest.Fake, *cloud.Client) {
	t.Helper()
	f := cloudtest.NewFake(t)
	c, err := cloud.New(t.TempDir(), f.URL, f.Keys())
	if err != nil {
		t.Fatal(err)
	}
	ls, err := c.StartLink(t.Context(), cloudHandle, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Pay(ls.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PollLink(t.Context(), ls.ID); err != nil {
		t.Fatal(err)
	}
	return f, c
}

// The pin is recorded with the handle's generation: at the same one it
// isn't sent again, but after the handle went elsewhere and came back (a
// new generation), whose CAA may name the other machine, it is.
func TestPinIsSentAgainAtANewGeneration(t *testing.T) {
	f, c := linkedFake(t)
	dir := t.TempDir()
	const acct = "https://acme.example/acct/1"
	cl, _ := c.State().Current(time.Now())
	ce := &cloudEndpoint{client: c, state: c.State(), log: slog.New(slog.DiscardHandler), claims: cl, d: Deps{DataDir: dir, PinSettle: time.Nanosecond}}
	puts := func() int {
		n := 0
		for _, q := range f.Requests() {
			if q.Method == "PUT" && q.Path == "/v1/acme-account" {
				n++
			}
		}
		return n
	}
	for i, want := range []int{1, 1} {
		if err := ce.pin(t.Context(), acct); err != nil {
			t.Fatal(err)
		}
		if puts() != want {
			t.Fatalf("pin %d: %d PUTs", i, puts())
		}
	}
	ce.claims.Gen++
	if err := ce.pin(t.Context(), acct); err != nil {
		t.Fatal(err)
	}
	if puts() != 2 {
		t.Fatalf("a new generation: %d PUTs, want 2", puts())
	}
}

func isStop(err error, want CloudEnd) bool {
	var s *CloudStop
	return errors.As(err, &s) && s.End == want
}

// The same generation with a newer entitlement (another tunnel of this
// same link) only means a refresh is due: this machine refreshes and takes
// the name back, and never stands by.
func TestCloudSameGenRetryReconnects(t *testing.T) {
	t.Parallel()
	r := startCloudRig(t)
	waitFor(t, 10*time.Second, "both tunnels", func() bool { return strings.Join(r.online(), ",") == "r1,r2" })
	key, err := r.client.TunnelKey()
	if err != nil {
		t.Fatal(err)
	}
	held, _ := r.client.State().Current(time.Now())
	before := refreshes(r.fake)
	l := r.intruder(key, held.Gen, held.Iat) // a tie goes to the newcomer
	l.Close()
	<-l.Done()
	waitFor(t, 15*time.Second, "this machine back on both relays", func() bool {
		return refreshes(r.fake) > before && strings.Join(r.online(), ",") == "r1,r2"
	})
	if end, _ := r.endpoint().Ended(); end != CloudRunning {
		t.Fatalf("ended %v", end)
	}
	select {
	case <-r.endpoint().Done():
		t.Fatal("stopped")
	default:
	}
}

func refreshes(f *cloudtest.Fake) int {
	n := 0
	for _, q := range f.Requests() {
		if q.Path == "/v1/entitlement/refresh" {
			n++
		}
	}
	return n
}

// Lapse is calm: in grace the health line says until when, and nothing is
// pushed. At expiry the endpoint stops and says so, once.
func TestCloudGraceThenExpired(t *testing.T) {
	t.Parallel()
	r := startCloudRig(t)
	cl, _ := r.client.State().Current(time.Now())
	grace := cl.PaidThrough.Add(time.Hour)
	r.now.Store(&grace)
	waitFor(t, 10*time.Second, "grace", func() bool { _, k := r.endpoint().CloudState(); return k == cloud.Grace })
	if st, detail, _ := CloudHealth(r.endpoint()); st != health.Warn || !strings.Contains(detail, "payment lapsed") {
		t.Fatalf("grace health %v %q", st, detail)
	}
	expired := cl.Exp.Add(time.Hour)
	r.now.Store(&expired)
	select {
	case <-r.endpoint().Done():
	case <-time.After(10 * time.Second):
		t.Fatal("still running after the entitlement expired")
	}
	if end, _ := r.endpoint().Ended(); end != CloudExpired {
		t.Fatalf("ended %v", end)
	}
	if !ExpiryNotice(r.dir, cl.Exp) || ExpiryNotice(r.dir, cl.Exp) {
		t.Fatal("one expiry is one notice")
	}
	if _, err := StartCloud(context.Background(), r.client, nil, r.deps); !isStop(err, CloudExpired) {
		t.Fatalf("start again: %v", err)
	}
}

// A refreshed entitlement naming other relays starts the endpoint again.
func TestCloudNewRelaysRestart(t *testing.T) {
	t.Parallel()
	r := startCloudRig(t)
	r.fake.SetRelays([]entitle.Relay{{ID: r.relays[1].id, URL: r.relays[1].tunnelURL(), IPs: []string{"127.0.0.1"}}})
	if _, err := r.client.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.endpoint().Done():
	case <-time.After(10 * time.Second):
		t.Fatal("didn't restart for the new relay list")
	}
	if end, _ := r.endpoint().Ended(); end != CloudRestart {
		t.Fatalf("ended %v", end)
	}
	r.start()
	waitFor(t, 10*time.Second, "the new relay", func() bool { return strings.Join(r.online(), ",") == "r2" })
	if len(r.endpoint().Listener.Status()) != 1 {
		t.Fatal("still tunnelling to the old relay")
	}
}

// Every refusal code has its own sentence, word for word.
func TestRefusalSentences(t *testing.T) {
	for code, want := range map[string]string{
		wire.CodeEntitlementExpired: "The relays say this computer's pass for your address has run out. I'm fetching a new one and will reconnect.",
		wire.CodeBadSignature:       "The relays couldn't check this computer's signature. I'll keep trying; if it lasts, run `mirrin cloud status`.",
		wire.CodeHostnameNotAllowed: "The relays won't carry your address for this computer. I'll keep trying; if it lasts, run `mirrin cloud status`.",
		wire.CodeDenied:             "Your address is suspended at the relays. I'll try again when they allow it.",
		wire.CodeSuperseded:         "Another computer has taken over your address, so this one is standing by.",
		wire.CodeSupersededRetry:    "A newer connection for your address came in. I'm refreshing and reconnecting.",
		wire.CodeRateLimited:        "The relays asked me to slow down. I'll reconnect in a moment.",
		wire.CodeUpgradeRequired:    "The relays need a newer Mirrin. Run `mirrin update`, then restart Mirrin.",
		"something_new":             "The relays refused the connection. I'll keep trying.",
	} {
		if got := RefusalSentence(code); got != want {
			t.Errorf("%s: %q, want %q", code, got, want)
		}
	}
}

// StartCloud sends nothing and starts nothing for a machine that can't
// carry reach: never linked, or linked and superseded.
func TestStartCloudStopsWithoutContact(t *testing.T) {
	dir := t.TempDir()
	f := cloudtest.NewFake(t)
	c, err := cloud.New(dir, f.URL, f.Keys())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartCloud(context.Background(), c, nil, Deps{DataDir: dir}); !isStop(err, CloudUnlinked) {
		t.Fatalf("unlinked: %v", err)
	}
	if _, err := StartCloud(context.Background(), nil, nil, Deps{DataDir: dir}); !isStop(err, CloudUnlinked) {
		t.Fatalf("no client: %v", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests", n)
	}
	if _, err := os.Stat(cloudRecordPath(dir)); err == nil {
		t.Fatal("wrote a record")
	}
}

// The routes list the paid handle only when it is the mode, and last.
func TestCloudRouteOnlyWhenChosen(t *testing.T) {
	in := RouteInputs{Reach: config.Reach{Mode: "tailscale"}, CloudHost: cloudHost, CloudReady: true, Live: []api.Base{{URL: "https://" + cloudHost}}}
	for _, rt := range Routes(in) {
		if rt.Kind == api.RouteCloud {
			t.Fatal("offered without being chosen")
		}
	}
	in.Reach.Mode = "cloud"
	rs := Routes(in)
	if last := rs[len(rs)-1]; last.Kind != api.RouteCloud || !last.Ready || last.BaseURL != "https://"+cloudHost {
		t.Fatalf("routes %+v", rs)
	}
	in.CloudReady, in.CloudDetail = false, "connecting to the relays"
	rs = Routes(in)
	if last := rs[len(rs)-1]; last.Ready || last.Problem != "Your address isn't connected yet: connecting to the relays." {
		t.Fatalf("routes %+v", rs)
	}
	if best, _ := api.BestRoute(Routes(RouteInputs{Reach: config.Reach{Mode: "cloud"}, CloudHost: cloudHost, CloudReady: true, Live: []api.Base{{URL: "https://" + cloudHost}}})); best.Kind != api.RouteCloud {
		t.Fatal("the chosen handle isn't the QR code's route")
	}
}
