package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/relay"
)

// These tests run the whole path in one process on 127.0.0.1:0: a Go TLS
// client, this relay, the daemon's tunnel client (internal/relay, WP-12)
// and a daemon-side tls.Server whose certificate comes from a test CA.

const tenant = "h.test"

// tap records every byte the relay splices, per client connection and
// direction.
type tap struct {
	mu    sync.Mutex
	conns map[net.Conn]*[2]bytes.Buffer // [toDaemon, toClient]
}

func newTap() *tap { return &tap{conns: map[net.Conn]*[2]bytes.Buffer{}} }

func (tp *tap) record(c net.Conn, toDaemon bool, b []byte) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	bufs := tp.conns[c]
	if bufs == nil {
		bufs = new([2]bytes.Buffer)
		tp.conns[c] = bufs
	}
	i := 1
	if toDaemon {
		i = 0
	}
	bufs[i].Write(b)
}

// streams returns each connection's two directions.
func (tp *tap) streams() [][2][]byte {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	var out [][2][]byte
	for _, b := range tp.conns {
		out = append(out, [2][]byte{bytes.Clone(b[0].Bytes()), bytes.Clone(b[1].Bytes())})
	}
	return out
}

// daemon is the twin's side: the tunnel client and a TLS server holding
// the only key for h.test.
type daemon struct {
	l        *relay.Listener
	cert     tls.Certificate
	acmeCert tls.Certificate
	remotes  chan string
	protos   chan []string
}

func startDaemon(t testing.TB, r *testRelay, d device, ent string) *daemon {
	t.Helper()
	dm := &daemon{
		cert:     r.pki.leaf(t, tenant),
		acmeCert: r.pki.leaf(t, tenant), // stands in for a TLS-ALPN-01 validation certificate
		remotes:  make(chan string, 16),
		protos:   make(chan []string, 16),
	}
	l, err := relay.Listen(context.Background(), relay.ClientConfig{
		Relays:      []relay.RelayRef{{ID: "r1", URL: r.tunnelURL()}},
		Key:         d.key,
		StatusKey:   d.statusKey,
		Entitlement: func() string { return ent },
		RootCAs:     r.pki.pool,
		Client:      "mirrin/test e2e",
	})
	if err != nil {
		t.Fatal(err)
	}
	dm.l = l
	base := &tls.Config{Certificates: []tls.Certificate{dm.cert}, NextProtos: []string{"h2", "http/1.1"}, MinVersion: tls.VersionTLS13}
	base.GetConfigForClient = func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		select {
		case dm.protos <- slices.Clone(h.SupportedProtos):
		default:
		}
		if slices.Equal(h.SupportedProtos, []string{"acme-tls/1"}) {
			return &tls.Config{Certificates: []tls.Certificate{dm.acmeCert}, NextProtos: []string{"acme-tls/1"}, MinVersion: tls.VersionTLS13}, nil
		}
		return nil, nil
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			body, _ := io.ReadAll(req.Body)
			dm.remotes <- req.RemoteAddr
			fmt.Fprintf(w, "%s %s %s", respMarker, req.Proto, body)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go srv.Serve(tls.NewListener(l, base))
	t.Cleanup(func() {
		srv.Close()
		l.Close()
		<-l.Done()
	})
	waitFor(t, 10*time.Second, "the tunnel", func() bool { return l.Status()[0].Online })
	return dm
}

// Markers that travel only inside TLS.
var (
	reqMarker  = "PLAINTEXT-REQUEST-MARKER-" + rand.Text()
	respMarker = "PLAINTEXT-RESPONSE-MARKER-" + rand.Text()
)

// tenantClient is a browser: SNI h.test, the test CA, and the daemon's
// SPKI pinned.
func tenantClient(r *testRelay, pin [32]byte, h2 bool, locals chan<- string) *http.Client {
	var p http.Protocols
	p.SetHTTP1(!h2)
	p.SetHTTP2(h2)
	alpn := []string{"http/1.1"}
	if h2 {
		alpn = []string{"h2"}
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Protocols: &p,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", r.addr)
			if err == nil {
				locals <- c.LocalAddr().String()
			}
			return c, err
		},
		TLSClientConfig: &tls.Config{
			ServerName: tenant,
			RootCAs:    r.pki.pool,
			MinVersion: tls.VersionTLS13,
			NextProtos: alpn,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if spki(cs.PeerCertificates[0]) != pin {
					return errors.New("not the daemon's key")
				}
				return nil
			},
		},
	}}
}

// Acceptance: relay + WP-12 client + daemon TLS server. A Go TLS client with
// SNI h.test completes TLS 1.3 (h2 and http/1.1) against the daemon's SPKI,
// and a splice tap never sees a plaintext marker. Run in both modes.
func TestE2ESplice(t *testing.T) {
	for _, mode := range []string{"self-host", "hosted"} {
		t.Run(mode, func(t *testing.T) {
			tp := newTap()
			d := newDevice(t)
			var r *testRelay
			var ent string
			if mode == "self-host" {
				r = startRelay(t, selfHostConfig(t, Allow{Hostname: tenant, Key: d.enc()}), Options{tap: tp.record})
			} else {
				h := startHosted(t, func(c *Config, o *Options) {
					c.Zones = []string{"test"}
					o.tap = tp.record
				})
				r, ent = h.testRelay, h.is.entitlement(t, d, "h", 1, time.Now(), tenant)
			}
			dm := startDaemon(t, r, d, ent)
			pin := spki(dm.cert.Leaf)

			for _, h2 := range []bool{true, false} {
				locals := make(chan string, 4)
				client := tenantClient(r, pin, h2, locals)
				resp, err := client.Post("https://"+tenant+"/echo", "text/plain", strings.NewReader(reqMarker))
				if err != nil {
					t.Fatalf("h2=%v: %v", h2, err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				client.CloseIdleConnections()
				wantProto, wantALPN := "HTTP/1.1", "http/1.1"
				if h2 {
					wantProto, wantALPN = "HTTP/2.0", "h2"
				}
				switch {
				case resp.StatusCode != 200 || resp.Proto != wantProto:
					t.Fatalf("h2=%v: %d %s", h2, resp.StatusCode, resp.Proto)
				case resp.TLS.Version != tls.VersionTLS13 || resp.TLS.NegotiatedProtocol != wantALPN:
					t.Fatalf("h2=%v: TLS %x ALPN %q", h2, resp.TLS.Version, resp.TLS.NegotiatedProtocol)
				case spki(resp.TLS.PeerCertificates[0]) != pin:
					t.Fatal("served by a key other than the daemon's")
				case string(body) != respMarker+" "+wantProto+" "+reqMarker:
					t.Fatalf("body %q", body)
				}
				// The daemon sees the browser's own address, from the PROXY header.
				if local, remote := <-locals, <-dm.remotes; local != remote {
					t.Fatalf("daemon saw %s, browser is %s", remote, local)
				}
			}
			assertCiphertextOnly(t, tp, dm)
			for _, name := range r.asked() {
				if name != controlName {
					t.Fatalf("the relay's certificate source was asked for %q", name)
				}
			}
		})
	}
}

// assertCiphertextOnly checks what the relay handled: it saw neither
// marker, nor the daemon's certificate (TLS 1.3 encrypts it) or private
// key; and each direction is a TLS record stream that is encrypted after
// the hellos.
func assertCiphertextOnly(t *testing.T, tp *tap, dm *daemon) {
	t.Helper()
	streams := tp.streams()
	if len(streams) == 0 {
		t.Fatal("the tap saw nothing")
	}
	priv := dm.cert.PrivateKey.(*ecdsa.PrivateKey)
	d, _ := priv.Bytes()
	secrets := map[string][]byte{
		"request marker":          []byte(reqMarker),
		"response marker":         []byte(respMarker),
		"daemon certificate":      dm.cert.Leaf.Raw,
		"daemon public key":       dm.cert.Leaf.RawSubjectPublicKeyInfo,
		"daemon private key":      d,
		"marker (base64)":         []byte(base64.StdEncoding.EncodeToString([]byte(reqMarker))),
		"daemon certificate tail": dm.cert.Leaf.Raw[len(dm.cert.Leaf.Raw)-64:],
	}
	for _, s := range streams {
		for dir, b := range s {
			for what, secret := range secrets {
				if bytes.Contains(b, secret) {
					t.Fatalf("the relay saw the %s", what)
				}
			}
			types, err := recordTypes(b)
			if err != nil {
				t.Fatalf("direction %d: %v", dir, err)
			}
			if len(types) < 2 || types[0] != recordHandshake {
				t.Fatalf("direction %d starts %v", dir, types)
			}
			for _, typ := range types[1:] {
				if typ != 20 && typ != 21 && typ != 23 { // change_cipher_spec, alert, application_data
					t.Fatalf("direction %d has a plaintext record of type %d after its hello: %v", dir, typ, types)
				}
			}
		}
	}
}

// recordTypes splits a TLS byte stream into its records' content types.
func recordTypes(b []byte) ([]byte, error) {
	var types []byte
	for len(b) > 0 {
		if len(b) < 5 {
			return nil, errors.New("truncated record header")
		}
		n := int(b[3])<<8 | int(b[4])
		if b[1] != 3 || len(b) < 5+n {
			return nil, fmt.Errorf("not a TLS record stream at %d", len(types))
		}
		types = append(types, b[0])
		b = b[5+n:]
	}
	return types, nil
}

// Acceptance: an acme-tls/1 ClientHello for a tenant is forwarded, not
// answered: the daemon's TLS-ALPN-01 responder completes it.
func TestE2EACMETLSForwarded(t *testing.T) {
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: tenant, Key: d.enc()}), Options{})
	dm := startDaemon(t, r, d, "")
	c, err := tls.Dial("tcp", r.addr, &tls.Config{
		ServerName:         tenant,
		NextProtos:         []string{"acme-tls/1"},
		InsecureSkipVerify: true, // as a CA's validator: it checks the certificate, not a chain
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cs := c.ConnectionState()
	if cs.NegotiatedProtocol != "acme-tls/1" || spki(cs.PeerCertificates[0]) != spki(dm.acmeCert.Leaf) {
		t.Fatalf("ALPN %q; certificate from the daemon: %v", cs.NegotiatedProtocol, spki(cs.PeerCertificates[0]) == spki(dm.acmeCert.Leaf))
	}
	if p := <-dm.protos; !slices.Equal(p, []string{"acme-tls/1"}) {
		t.Fatalf("daemon saw %v", p)
	}
	for _, name := range r.asked() {
		if name != controlName {
			t.Fatalf("relay answered for %q", name)
		}
	}
}

// splitWrites sends its first Write (the ClientHello) in three TCP
// segments.
type splitWrites struct {
	net.Conn
	done atomic.Bool
}

func (c *splitWrites) Write(p []byte) (int, error) {
	if c.done.Swap(true) || len(p) < 3 {
		return c.Conn.Write(p)
	}
	third := len(p) / 3
	for _, part := range [][]byte{p[:third], p[third : 2*third], p[2*third:]} {
		if _, err := c.Conn.Write(part); err != nil {
			return 0, err
		}
		time.Sleep(30 * time.Millisecond)
	}
	return len(p), nil
}

// Acceptance: a ClientHello with an X25519MLKEM768 key share split across 3
// TCP writes is routed, and the daemon completes the hybrid handshake.
func TestE2EMLKEMSplitHello(t *testing.T) {
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: tenant, Key: d.enc()}), Options{})
	dm := startDaemon(t, r, d, "")
	raw, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	c := tls.Client(&splitWrites{Conn: raw}, &tls.Config{
		ServerName:       tenant,
		RootCAs:          r.pki.pool,
		NextProtos:       []string{"http/1.1"},
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768},
	})
	defer c.Close()
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	cs := c.ConnectionState()
	if cs.CurveID != tls.X25519MLKEM768 || spki(cs.PeerCertificates[0]) != spki(dm.cert.Leaf) {
		t.Fatalf("curve %v", cs.CurveID)
	}
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", tenant)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
}

// Acceptance: status through the real tunnel client: online while
// tunnelled, last_seen within 1 s of the daemon going away.
func TestE2EStatus(t *testing.T) {
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: tenant, Key: d.enc()}), Options{})
	dm := startDaemon(t, r, d, "")
	client := r.https()
	url := "https://" + controlName + "/v1/status/" + tenant + "?k=" + base64.RawURLEncoding.EncodeToString(d.statusKey)
	status := func() (int, string) {
		resp, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := status(); code != 200 || !strings.Contains(body, `"online":true`) {
		t.Fatalf("%d %s", code, body)
	}
	done := tunnelDone(t, r.Server, tenant)
	dm.l.Close()
	<-dm.l.Done()
	gone := time.Now()
	waitSignal(t, 5*time.Second, "the tunnel to detach", done)
	code, body := status()
	if code != 200 || !strings.Contains(body, `"online":false`) {
		t.Fatalf("offline: %d %s", code, body)
	}
	var last time.Time
	if i := strings.Index(body, `"last_seen":"`); i >= 0 {
		last, _ = time.Parse(time.RFC3339, body[i+13:i+13+20])
	}
	if diff := last.Sub(gone); diff < -time.Second || diff > time.Second {
		t.Fatalf("last_seen %s is %v from the disconnect", body, diff)
	}
}

// Acceptance: self-host mode works with no network access to any MavrkAI
// host. The relay's only outbound client fails the test if used, and the
// whole path still works.
func TestE2ESelfHostNeedsNoMavrkAI(t *testing.T) {
	var outbound atomic.Int64
	noNet := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		outbound.Add(1)
		return nil, fmt.Errorf("self-hosted relay tried to reach %s", req.URL.Host)
	})}
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: tenant, Key: d.enc()}), Options{HTTPClient: noNet, denyEvery: time.Millisecond})
	dm := startDaemon(t, r, d, "")
	locals := make(chan string, 2)
	resp, err := tenantClient(r, spki(dm.cert.Leaf), true, locals).Get("https://" + tenant + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	time.Sleep(50 * time.Millisecond) // many deny list periods
	if n := outbound.Load(); n != 0 {
		t.Fatalf("%d outbound requests", n)
	}
	if !r.cfg.SelfHosted() || r.cfg.DenylistURL != "" || r.pol.entKeys != nil {
		t.Fatal("not self-hosted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Closing the relay and the daemon leaves no goroutine behind.
func TestE2ENoGoroutineLeak(t *testing.T) {
	base := goroutineIDs()
	func() {
		d := newDevice(t)
		cfg := selfHostConfig(t, Allow{Hostname: tenant, Key: d.enc()})
		r := &testRelay{pki: newPKI(t)}
		cert := r.pki.leaf(t, controlName)
		s, err := New(cfg, Options{Certificates: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }})
		if err != nil {
			t.Fatal(err)
		}
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		r.Server, r.addr = s, ln.Addr().String()
		go s.Serve(ln)
		l, err := relay.Listen(context.Background(), relay.ClientConfig{
			Relays: []relay.RelayRef{{ID: "r1", URL: r.tunnelURL()}}, Key: d.key, StatusKey: d.statusKey, RootCAs: r.pki.pool,
		})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, 10*time.Second, "tunnel", func() bool { return l.Status()[0].Online })
		// One client connection held open through the relay.
		c, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Write(clientHello(t, &tls.Config{ServerName: tenant}))
		ac, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		c.Close()
		ac.Close()
		l.Close()
		<-l.Done()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var left []string
		for id, g := range goroutineIDs() {
			if _, ok := base[id]; !ok {
				left = append(left, g)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines left:\n\n%s", len(left), strings.Join(left, "\n\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func goroutineIDs() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	m := map[string]string{}
	for i, g := range strings.Split(string(buf), "\n\n") {
		if f := strings.Fields(g); i > 0 && len(f) > 1 && f[0] == "goroutine" {
			m[f[1]] = g
		}
	}
	return m
}

// The tap is a real witness: when plaintext does pass through the relay, as
// it would for a daemon that did not use TLS, the tap sees it, both ways.
func TestTapSeesWhatPasses(t *testing.T) {
	tp := newTap()
	d := newDevice(t)
	r := startRelay(t, selfHostConfig(t, Allow{Hostname: tenant, Key: d.enc()}), Options{tap: tp.record})
	tt := mustTunnel(t, r, signedHello(t, d, ""))
	c := dialName(t, r, tenant)
	io.WriteString(c, reqMarker)
	br, st, _ := tt.accept(t)
	hello := clientHello(t, &tls.Config{ServerName: tenant})
	got := make([]byte, len(hello)+len(reqMarker))
	if _, err := io.ReadFull(br, got); err != nil || !bytes.HasSuffix(got, []byte(reqMarker)) {
		t.Fatalf("daemon got %q (%v)", got, err)
	}
	io.WriteString(st, respMarker)
	if _, err := io.ReadFull(c, make([]byte, len(respMarker))); err != nil {
		t.Fatal(err)
	}
	s := tp.streams()
	if len(s) != 1 || !bytes.Contains(s[0][0], []byte(reqMarker)) || !bytes.Contains(s[0][1], []byte(respMarker)) {
		t.Fatal("the tap missed plaintext that passed through")
	}
}
