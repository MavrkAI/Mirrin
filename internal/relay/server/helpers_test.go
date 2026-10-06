package server

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// controlName is the test relay's control name. The tunnel client dials
// wss://localhost:<port>, which resolves without any DNS.
const controlName = "localhost"

// testPKI is a throwaway CA and the leaf certificates it signs.
type testPKI struct {
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newPKI(t testing.TB) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mirrin relay test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &testPKI{ca: ca, key: key, pool: pool}
}

var serial atomic.Int64

// leaf issues a certificate for names under a fresh P-256 key.
func (p *testPKI) leaf(t testing.TB, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(100 + serial.Add(1)),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// spki is the SHA-256 of a certificate's public key, as a client pins it.
func spki(c *x509.Certificate) [32]byte { return sha256.Sum256(c.RawSubjectPublicKeyInfo) }

// testRelay is a relay serving on 127.0.0.1:0 with a test CA certificate
// for its control name. It records every name GetCertificate's inner
// source is asked for.
type testRelay struct {
	*Server
	addr string
	pki  *testPKI

	mu        sync.Mutex
	certAsked []string
}

func selfHostConfig(t testing.TB, allow ...Allow) Config {
	c := DefaultConfig()
	c.ID = "r1"
	c.ControlHostname = controlName
	c.StateDir = t.TempDir()
	c.HTTPListen, c.MetricsListen = "", ""
	c.Allow = allow
	return c
}

func startRelay(t testing.TB, cfg Config, opts Options) *testRelay {
	t.Helper()
	return startRelayOn(t, cfg, opts, nil)
}

// startRelayOn is startRelay with the relay's listener wrapped by wrap.
func startRelayOn(t testing.TB, cfg Config, opts Options, wrap func(net.Listener) net.Listener) *testRelay {
	t.Helper()
	r := &testRelay{pki: newPKI(t)}
	cert := r.pki.leaf(t, controlName)
	opts.Certificates = func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		r.mu.Lock()
		r.certAsked = append(r.certAsked, h.ServerName)
		r.mu.Unlock()
		return &cert, nil
	}
	if opts.Log == nil {
		opts.Log = testLog(t)
	}
	s, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.Server, r.addr = s, ln.Addr().String()
	if wrap != nil {
		ln = wrap(ln)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	return r
}

// asked is every name the certificate source was asked for.
func (r *testRelay) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.certAsked...)
}

func (r *testRelay) port() string {
	_, p, _ := net.SplitHostPort(r.addr)
	return p
}

func (r *testRelay) tunnelURL() string { return "wss://" + controlName + ":" + r.port() + wire.Path }

// https is a client for the control name.
func (r *testRelay) https() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: r.pki.pool, ServerName: controlName},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", r.addr)
		},
	}, Timeout: 10 * time.Second}
}

// testLog sends the relay's log to t.Log.
func testLog(t testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(tlogWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type tlogWriter struct{ t testing.TB }

func (w tlogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// device is a daemon's device key and status key.
type device struct {
	key       ed25519.PrivateKey
	statusKey []byte
}

func newDevice(t testing.TB) device {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sk := make([]byte, 32)
	rand.Read(sk)
	return device{key: k, statusKey: sk}
}

func (d device) pub() ed25519.PublicKey { return d.key.Public().(ed25519.PublicKey) }
func (d device) enc() string            { return entitle.EncodeKey(d.pub()) }

// issuer signs entitlements and deny lists for a hosted test relay.
type issuer struct {
	ent, dl ed25519.PrivateKey
}

func newIssuer(t testing.TB) issuer {
	_, e, _ := ed25519.GenerateKey(rand.Reader)
	_, d, _ := ed25519.GenerateKey(rand.Reader)
	return issuer{ent: e, dl: d}
}

func (is issuer) keys() []IssuerKey {
	return []IssuerKey{
		{Kid: "ent-test-a", Key: entitle.EncodeKey(is.ent.Public().(ed25519.PublicKey))},
		{Kid: "dl-test-a", Key: entitle.EncodeKey(is.dl.Public().(ed25519.PublicKey))},
	}
}

// entitlement signs claims for handle h in zone "mirrin.test" unless hosts
// are given.
func (is issuer) entitlement(t testing.TB, d device, handle string, gen int64, iat time.Time, hosts ...string) string {
	t.Helper()
	if hosts == nil {
		hosts = []string{handle + ".mirrin.test"}
	}
	tok, err := entitle.Sign(entitle.Claims{
		Iss: "cloud.mirrin.test", Sub: "acct_test", Aud: entitle.Audience,
		Iat: iat, Nbf: iat, Exp: iat.Add(35 * 24 * time.Hour), PaidThrough: iat.Add(30 * 24 * time.Hour),
		Gen: gen, Plan: "cloud", Feat: []string{"reach", "backup"}, Handle: handle, Hosts: hosts,
		Cnf: d.enc(), Relays: []entitle.Relay{{ID: "r1", URL: "wss://localhost/v1/tunnel"}},
	}, "ent-test-a", is.ent)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (is issuer) denyList(t testing.TB, seq int64, handles []string, keys ...device) string {
	t.Helper()
	l := entitle.DenyList{Seq: seq, Iat: time.Now()}
	for _, h := range handles {
		l.Handles = append(l.Handles, entitle.Entry{Value: h, Why: "test"})
	}
	for _, k := range keys {
		l.Keys = append(l.Keys, entitle.Entry{Value: k.enc(), Why: "recovered"})
	}
	tok, err := entitle.SignDenyList(l, "dl-test-a", is.dl)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// denyServer serves whatever deny list token it is given.
type denyServer struct {
	*httptest.Server
	mu  sync.Mutex
	tok string
}

func newDenyServer(t testing.TB) *denyServer {
	d := &denyServer{}
	d.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		tok := d.tok
		d.mu.Unlock()
		if tok == "" {
			http.Error(w, "", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, tok+"\n")
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *denyServer) set(tok string) {
	d.mu.Lock()
	d.tok = tok
	d.mu.Unlock()
}

func hostedConfig(t testing.TB, is issuer, deny *denyServer) Config {
	c := DefaultConfig()
	c.ID = "r1"
	c.ControlHostname = controlName
	c.StateDir = t.TempDir()
	c.HTTPListen, c.MetricsListen = "", ""
	c.Zones = []string{"mirrin.test"}
	c.IssuerKeys = is.keys()
	c.DenylistURL = deny.URL + "/v1/denylist"
	return c
}

// testTunnel is a hand-driven daemon side of one tunnel.
type testTunnel struct {
	ws      *websocket.Conn
	sess    *yamux.Session
	welcome wire.Welcome
	ctrl    *wire.ControlReader
}

// helloFunc builds the hello frame for a challenge on a session with this
// exporter.
type helloFunc func(ch wire.Challenge, exporter []byte) []byte

func signedHello(t testing.TB, d device, ent string) helloFunc {
	return func(ch wire.Challenge, exporter []byte) []byte {
		sum := sha256.Sum256(d.statusKey)
		h, err := wire.SignHello(d.key, ch, exporter, ent, sum[:], "mirrin/test")
		if err != nil {
			t.Fatal(err)
		}
		b, err := h.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
}

// dialTunnel runs the handshake by hand. A refusal comes back as a
// wire.Error.
func dialTunnel(t testing.TB, r *testRelay, hello helloFunc) (*testTunnel, error) {
	t.Helper()
	return dialTunnelVia(t, r, r.addr, hello)
}

// dialTunnelVia is dialTunnel over a TCP connection to addr, which
// forwards to the relay.
func dialTunnelVia(t testing.TB, r *testRelay, addr string, hello helloFunc) (*testTunnel, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var tc *tls.Conn
	tr := &http.Transport{DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		d := tls.Dialer{Config: &tls.Config{ServerName: controlName, RootCAs: r.pki.pool, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}}}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			tc = c.(*tls.Conn)
		}
		return c, err
	}}
	defer tr.CloseIdleConnections()
	ws, _, err := websocket.Dial(ctx, r.tunnelURL(), &websocket.DialOptions{
		HTTPClient:   &http.Client{Transport: tr},
		Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*testTunnel, error) {
		ws.CloseNow()
		return nil, err
	}
	exporter, err := wire.Exporter(tc.ConnectionState())
	if err != nil {
		return fail(err)
	}
	_, b, err := ws.Read(ctx)
	if err != nil {
		return fail(err)
	}
	ch, err := wire.ParseChallenge(b)
	if err != nil {
		return fail(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, hello(ch, exporter)); err != nil {
		return fail(err)
	}
	_, b, err = ws.Read(ctx)
	if err != nil {
		return fail(err)
	}
	w, err := wire.ParseReply(b)
	if err != nil {
		return fail(err)
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = time.Duration(w.Keepalive) * time.Second
	sess, err := yamux.Server(websocket.NetConn(context.Background(), ws, websocket.MessageBinary), cfg)
	if err != nil {
		return fail(err)
	}
	st, err := sess.AcceptStreamWithContext(ctx)
	if err != nil || st.StreamID() != wire.ControlStreamID {
		sess.Close()
		return fail(err)
	}
	tt := &testTunnel{ws: ws, sess: sess, welcome: w, ctrl: wire.NewControlReader(st)}
	t.Cleanup(func() { sess.Close() })
	return tt, nil
}

func mustTunnel(t testing.TB, r *testRelay, hello helloFunc) *testTunnel {
	t.Helper()
	tt, err := dialTunnel(t, r, hello)
	if err != nil {
		t.Fatalf("tunnel refused: %v", err)
	}
	return tt
}

// refusal dials a tunnel that must be refused, and returns the refusal.
func refusal(t testing.TB, r *testRelay, hello helloFunc) wire.Error {
	t.Helper()
	tt, err := dialTunnel(t, r, hello)
	if err == nil {
		tt.sess.Close()
		t.Fatalf("tunnel welcomed with %+v", tt.welcome)
	}
	e, ok := err.(wire.Error)
	if !ok {
		t.Fatalf("not a refusal: %v", err)
	}
	return e
}

// nextControl reads the next control message, failing after d.
func (tt *testTunnel) nextControl(t testing.TB, d time.Duration) wire.Control {
	t.Helper()
	type res struct {
		c   wire.Control
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := tt.ctrl.Next()
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("control stream: %v", r.err)
		}
		return r.c
	case <-time.After(d):
		t.Fatal("no control message")
		return wire.Control{}
	}
}

// accept takes the next relayed stream and its PROXY header.
func (tt *testTunnel) accept(t testing.TB) (*bufio.Reader, *yamux.Stream, wire.ProxyHeader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := tt.sess.AcceptStreamWithContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(st)
	h, err := wire.ReadProxyV2(br)
	if err != nil {
		t.Fatal(err)
	}
	return br, st, h
}

// waitFor polls ok until it holds or d passes.
func waitFor(t testing.TB, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// clientHello returns the first flight a crypto/tls client sends for cfg:
// its ClientHello record(s).
func clientHello(t testing.TB, cfg *tls.Config) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go tls.Client(a, cfg).Handshake()
	b.SetReadDeadline(time.Now().Add(5 * time.Second))
	var out []byte
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(b, hdr); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(hdr[3])<<8|int(hdr[4]))
	if _, err := io.ReadFull(b, body); err != nil {
		t.Fatal(err)
	}
	out = append(append(out, hdr...), body...)
	return out
}

// stallProxy forwards TCP connections to a relay until stall; from then
// on it swallows every byte both ways and closes nothing, as a laptop
// that went to sleep does.
type stallProxy struct {
	ln      net.Listener
	stalled atomic.Bool
	mu      sync.Mutex
	conns   []net.Conn
}

func newStallProxy(t testing.TB, to string) *stallProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &stallProxy{ln: ln}
	t.Cleanup(p.close)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", to)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, u)
			p.mu.Unlock()
			go p.pipe(u, c)
			go p.pipe(c, u)
		}
	}()
	return p
}

func (p *stallProxy) addr() string { return p.ln.Addr().String() }
func (p *stallProxy) stall()       { p.stalled.Store(true) }

func (p *stallProxy) pipe(dst, src net.Conn) {
	b := make([]byte, 32<<10)
	for {
		n, err := src.Read(b)
		if n > 0 && !p.stalled.Load() {
			if _, err := dst.Write(b[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *stallProxy) close() {
	p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
}

// manyNetworks gives every connection the relay accepts a client address
// in a /24 of its own, as a crowd of clients would have.
func manyNetworks(ln net.Listener) net.Listener { return &netsListener{Listener: ln} }

type netsListener struct {
	net.Listener
	n atomic.Int64
}

func (l *netsListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	i := l.n.Add(1)
	ip := net.IPv4(198, 18, byte(i), 7).To4()
	return fromConn{c, &net.TCPAddr{IP: ip, Port: 40000 + int(i)}}, nil
}

type fromConn struct {
	net.Conn
	from net.Addr
}

func (c fromConn) RemoteAddr() net.Addr { return c.from }

// logBuffer holds a relay's log, as JSON lines, for a test to read.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lines counts the log lines whose msg is msg.
func (l *logBuffer) lines(msg string) int {
	return strings.Count(l.String(), `"msg":"`+msg+`"`)
}

// denyPolls drives the real polling loop one refresh at a time. The buffered
// completion never blocks shutdown; tests consume startup before advancing.
type denyPolls struct {
	ticks chan time.Time
	done  chan struct{}
}

func newDenyPolls(o *Options) *denyPolls {
	p := &denyPolls{ticks: make(chan time.Time, 1), done: make(chan struct{}, 1)}
	o.denyTicks = p.ticks
	o.denyRefreshed = func() { p.done <- struct{}{} }
	return p
}

func (p *denyPolls) next(t testing.TB) {
	t.Helper()
	p.ticks <- time.Now()
	waitSignal(t, 5*time.Second, "deny list refresh to finish", p.done)
}

func waitSignal(t testing.TB, d time.Duration, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// tunnelDone closes after the relay has removed the routes and recorded
// offline status. Capture it while connected, before ending the session.
func tunnelDone(t testing.TB, s *Server, hostname string) <-chan struct{} {
	t.Helper()
	tunnel, ok := s.reg.Lookup(hostname)
	if !ok {
		t.Fatalf("no tunnel for %s", hostname)
	}
	return tunnel.done
}
