package api

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRemotePolicy(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	for _, tc := range []struct {
		name, method, path, host, origin, site, token string
		want                                          int
	}{
		{name: "host", path: "/healthz", host: "evil.test", want: 421},
		{name: "health", path: "/healthz", want: 200},
		{name: "master", path: "/status", token: master, want: 401},
		{name: "query", path: "/ui?token=" + master, want: 401},
		{name: "foreign", method: "POST", path: "/approvals/1/approve", origin: "https://evil.test", site: "same-origin", token: "device", want: 403},
		{name: "fetch", method: "POST", path: "/approvals/1/approve", origin: "https://twin.test", token: "device", want: 403},
		{name: "approve", method: "POST", path: "/approvals/1/approve", origin: "https://twin.test", site: "same-origin", token: "device", want: 200},
		{name: "channels", path: "/channels", token: "device", want: 404}, {name: "accounts", path: "/accounts", token: "device", want: 404},
		{name: "unknown", path: "/unknown", want: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			_, tok, err := e.store.Add("phone", devices.KindPWA, []devices.Scope{devices.View, devices.Approve}, "", "")
			if err != nil {
				t.Fatal(err)
			}
			method := tc.method
			if method == "" {
				method = "GET"
			}
			r := httptest.NewRequest(method, "https://twin.test"+tc.path, nil)
			if tc.host != "" {
				r.Host = tc.host
			}
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			if tc.token == "device" {
				r.AddCookie(&http.Cookie{Name: cookieSecure, Value: tok})
			} else if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			e.s.remoteHandler(RemoteOptions{Hostnames: []string{"twin.test"}}).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
			for _, h := range []string{"Strict-Transport-Security", "Content-Security-Policy", "Cross-Origin-Resource-Policy", "X-Content-Type-Options", "Referrer-Policy"} {
				if w.Header().Get(h) == "" {
					t.Error("missing", h)
				}
			}
			if tc.want == 401 && len(w.Result().Cookies()) != 0 {
				t.Fatal("refusal set cookie")
			}
		})
	}
}
func TestRemoteAnonymousLimit(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	e := newEnv(t)
	h := e.s.remoteHandler(RemoteOptions{Hostnames: []string{"twin.test"}})
	for i := 1; i <= 11; i++ {
		r := httptest.NewRequest("GET", "https://twin.test/healthz", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 200
		if i == 11 {
			want = 429
			if w.Header().Get("Retry-After") == "" {
				t.Fatal("no retry")
			}
		}
		if w.Code != want {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
}

func TestRemoteAdminScope(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	for _, enabled := range []bool{false, true} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprint(enabled, admin), func(t *testing.T) {
				e := newEnv(t)
				scopes := []devices.Scope{devices.View}
				if admin {
					scopes = append(scopes, devices.Admin)
				}
				_, tok, err := e.store.Add("phone", devices.KindPWA, scopes, "", "")
				if err != nil {
					t.Fatal(err)
				}
				h := e.s.remoteHandler(RemoteOptions{Hostnames: []string{"twin.test"}, AllowAdmin: enabled})
				for _, path := range []string{"/channels", "/accounts"} {
					r := httptest.NewRequest("GET", "https://twin.test"+path, nil)
					r.Header.Set("Authorization", "Bearer "+tok)
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					want := 404
					if enabled && admin {
						want = 200
					}
					if w.Code != want {
						t.Fatalf("%s: %d %s", path, w.Code, w.Body)
					}
				}
			})
		}
	}
}

type pipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443} }

type testSource struct{ pair tls.Certificate }

func (s testSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return &s.pair, nil
}
func (s testSource) Hostnames() []string           { return []string{"twin.test"} }
func (s testSource) SPKIs() []string               { return []string{SPKIPin(s.pair.Leaf)} }
func (s testSource) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func remoteTestSource(t *testing.T) testSource     { return remoteNamedSource(t, "twin.test") }
func remoteNamedSource(t *testing.T, hostname string) testSource {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{hostname}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testSource{tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}
}

// A real TLS and HTTP/2 connection over net.Pipe works even in sandboxes
// which refuse local sockets. It exercises ServeRemote, not just its handler.
func TestRemoteHTTP2SSE(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	e := newEnv(t)
	src := remoteTestSource(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- e.s.ServeRemote(ctx, ln, src, RemoteOptions{Via: "files"}) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	roots := x509.NewCertPool()
	roots.AddCert(src.pair.Leaf)
	transport := &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: roots}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		select {
		case ln.connections <- server:
			return client, nil
		case <-ctx.Done():
			client.Close()
			server.Close()
			return nil, ctx.Err()
		}
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second}
	_, tok, err := e.store.Add("phone", devices.KindPWA, []devices.Scope{devices.View, devices.Approve}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, origin string
		want         int
	}{{"wrong.test", "https://twin.test", 421}, {"twin.test", "https://foreign.test", 403}, {"twin.test", "https://twin.test", 200}} {
		r, _ := http.NewRequest("POST", "https://twin.test/approvals/1/approve", nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.AddCookie(&http.Cookie{Name: cookieSecure, Value: tok})
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.want || res.ProtoMajor != 2 {
			t.Fatalf("%d %s", res.StatusCode, res.Proto)
		}
	}
	// The stream stays open and its keepalive arrives through ServeRemote,
	// unbuffered. The interval is made short so a busy machine can't push
	// it outside a timing window; that the twin waits for it is still seen.
	if eventsKeepalive != 20*time.Second {
		t.Fatalf("keepalive every %s", eventsKeepalive)
	}
	const every = 100 * time.Millisecond
	eventsKeepalive = every
	defer func() { eventsKeepalive = 20 * time.Second }()
	// Generous, so a busy machine can't fail it, but a stream that never
	// delivers fails here rather than at go test's timeout.
	reqCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	r, _ := http.NewRequestWithContext(reqCtx, "GET", "https://twin.test/events", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	start := time.Now()
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.ProtoMajor != 2 || res.StatusCode != 200 {
		t.Fatalf("%s %d", res.Proto, res.StatusCode)
	}
	scan := bufio.NewScanner(res.Body)
	for keepalives := 0; scan.Scan(); {
		if scan.Text() != ": keepalive" {
			continue
		}
		if keepalives++; time.Since(start) < time.Duration(keepalives)*every {
			t.Fatalf("keepalive %d came after %s", keepalives, time.Since(start))
		}
		if keepalives == 2 {
			return
		}
	}
	t.Fatal("no SSE keepalive within 10s:", scan.Err())
}

func TestRemoteLimiterBoundAndRefill(t *testing.T) {
	l := newRemoteLimiter(1, 2)
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("192.0.2.1:123"); !ok {
			t.Fatal("early limit")
		}
	}
	if ok, _ := l.allow("[::ffff:192.0.2.1]:321"); ok {
		t.Fatal("IP alias bypass")
	}
	now = now.Add(time.Second)
	if ok, _ := l.allow("192.0.2.1:123"); !ok {
		t.Fatal("did not refill")
	}
	for i := 0; i < 10000; i++ {
		l.allow(fmt.Sprint(i))
	}
	if len(l.b) != 10000 {
		t.Fatal(len(l.b))
	}
	if ok, _ := l.allow("another"); ok {
		t.Fatal("table grew past limit")
	}
	now = now.Add(3 * time.Second)
	if ok, _ := l.allow("another"); !ok {
		t.Fatal("did not reclaim idle buckets")
	}
}

// Exercise the shipping Client, including Claim's own client construction.
func TestRemoteNativeClient(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	e := newEnv(t)
	offer := e.offer("cli")
	src := remoteTestSource(t)
	tr := remotePipe(t, e.s, src, RemoteOptions{Via: "files"})
	old := http.DefaultTransport
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = old })
	claimed, err := Claim(context.Background(), "https://twin.test", src.SPKIs(), offer.Offer.ID, offer.Offer.Secret, "terminal", "cli")
	if err != nil {
		t.Fatal("Claim:", err)
	}
	c := newClient(Target{Address: "https://twin.test", Token: claimed.Token, Pins: src.SPKIs()})
	t.Cleanup(c.http.CloseIdleConnections)
	if got, err := c.Message(context.Background(), "cli", "terminal", "hello"); err != nil || got != "hi" {
		t.Fatal("Message:", got, err)
	}
	if got, err := c.MessageStream(context.Background(), "cli", "terminal", "hello", func(string) {}); err != nil || got != "hi" {
		t.Fatal("MessageStream:", got, err)
	}
}
func remotePipe(t *testing.T, s *Server, src tlsmgr.Source, o RemoteOptions) *http.Transport {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ln := &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.ServeRemote(ctx, ln, src, o) }()
	tr := &http.Transport{ForceAttemptHTTP2: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		select {
		case ln.connections <- server:
			return client, nil
		case <-ctx.Done():
			client.Close()
			server.Close()
			return nil, ctx.Err()
		case <-ln.closed:
			client.Close()
			server.Close()
			return nil, net.ErrClosed
		}
	}}
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return tr
}
func TestRemoteBrowserOriginAndSignedWebhook(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	e := newEnv(t)
	_, tok, _ := e.store.Add("phone", devices.KindPWA, []devices.Scope{devices.Approve}, "", "")
	e.s.public["/phone/"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Signature") != "valid" {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(204)
	}
	h := e.s.remoteHandler(RemoteOptions{Hostnames: []string{"twin.test"}})
	for _, tc := range []struct {
		path, origin, site, signature string
		cookie                        bool
		want                          int
	}{
		{path: "/phone/inbound", signature: "valid", want: 204}, {path: "/phone/inbound", want: 401},
		{path: "/approvals/1/approve", cookie: true, want: 403},
		{path: "/approvals/1/approve", origin: "https://foreign.test", site: "same-origin", want: 403},
		{path: "/approvals/1/approve", site: "cross-site", want: 403},
		{path: "/approvals/1/approve", origin: "https://twin.test", site: "same-origin", cookie: true, want: 200},
	} {
		r := httptest.NewRequest("POST", "https://twin.test"+tc.path, nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		r.Header.Set("X-Test-Signature", tc.signature)
		if tc.cookie {
			r.AddCookie(&http.Cookie{Name: cookieSecure, Value: tok})
		} else if !strings.HasPrefix(tc.path, "/phone/") {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body)
		}
	}
}
func TestRemoteAdminIsPerTLSListener(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	e := newEnv(t)
	e.s.AllowAdminRemote()
	_ = e.s.Handler() // built before ServeRemote/remoteHandler
	_, tok, _ := e.store.Add("admin", devices.KindPWA, []devices.Scope{devices.Admin}, "", "")
	if w := e.do(onLegacy, req{path: "/channels/list", header: bearer(tok)}); w.Code != 404 {
		t.Fatalf("cleartext admin: %d", w.Code)
	}
	for _, enabled := range []bool{true, false, true} {
		h := e.s.remoteHandler(RemoteOptions{Hostnames: []string{"twin.test"}, AllowAdmin: enabled})
		r := httptest.NewRequest("GET", "https://twin.test/channels/list", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 404
		if enabled {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("admin %v: %d", enabled, w.Code)
		}
	}
}

type rotatingSource struct {
	mu      sync.RWMutex
	current testSource
}

func (s *rotatingSource) GetCertificate(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current.GetCertificate(h)
}
func (s *rotatingSource) Hostnames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.current.pair.Leaf.DNSNames...)
}
func (s *rotatingSource) SPKIs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current.SPKIs()
}
func (s *rotatingSource) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func TestRemotePairBasesFollowRotation(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	e := newEnv(t)
	src := &rotatingSource{current: remoteTestSource(t)}
	tr := remotePipe(t, e.s, src, RemoteOptions{})
	pair, _ := src.GetCertificate(nil)
	roots := x509.NewCertPool()
	roots.AddCert(pair.Leaf)
	tr.TLSClientConfig = &tls.Config{RootCAs: roots}
	client := &http.Client{Transport: tr}
	res, err := client.Get("https://twin.test/healthz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	first := e.s.PairBases()
	if len(first) != 1 || first[0].Pins[0] != src.SPKIs()[0] {
		t.Fatal(first)
	}
	src.mu.Lock()
	src.current = remoteNamedSource(t, "next.test")
	src.mu.Unlock()
	next := e.s.PairBases()
	if next[0].URL != "https://next.test" {
		t.Fatal("stale hostname", next)
	}
	if next[0].Pins[0] != src.SPKIs()[0] || next[0].Pins[0] == first[0].Pins[0] {
		t.Fatalf("stale pins: %v -> %v", first, next)
	}
	second := tr.Clone()
	leaf, _ := src.GetCertificate(nil)
	secondRoots := x509.NewCertPool()
	secondRoots.AddCert(leaf.Leaf)
	second.TLSClientConfig = &tls.Config{RootCAs: secondRoots}
	defer second.CloseIdleConnections()
	response, err := (&http.Client{Transport: second}).Get("https://next.test/healthz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("new hostname refused", response.StatusCode)
	}
	next[0].Pins[0] = "caller mutation"
	if e.s.PairBases()[0].Pins[0] == "caller mutation" {
		t.Fatal("aliased pins")
	}
}
func TestTailscalePinFallbackRequiresCAAndHostname(t *testing.T) {
	// A private test CA stands in for the system trust store, never the network.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"twin.tailnet.ts.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	tr := transport([]string{"stale pin"}).(*http.Transport)
	tr.TLSClientConfig.RootCAs = roots
	cs := tls.ConnectionState{ServerName: "twin.tailnet.ts.net", PeerCertificates: []*x509.Certificate{cert}}
	if err = tr.TLSClientConfig.VerifyConnection(cs); err != nil {
		t.Fatal("trusted renewal", err)
	}
	cs.ServerName = "other.tailnet.ts.net"
	if tr.TLSClientConfig.VerifyConnection(cs) == nil {
		t.Fatal("wrong hostname accepted")
	}
	cs.ServerName = "twin.tailnet.ts.net"
	tr.TLSClientConfig.RootCAs = x509.NewCertPool()
	if tr.TLSClientConfig.VerifyConnection(cs) == nil {
		t.Fatal("untrusted CA accepted")
	}
	cs.ServerName = "other.example"
	tr.TLSClientConfig.RootCAs = roots
	if tr.TLSClientConfig.VerifyConnection(cs) == nil {
		t.Fatal("non-Tailscale origin accepted")
	}
}
func TestRemoteUsageAllowed(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	e := newEnv(t)
	_, tok, _ := e.store.Add("phone", devices.KindPWA, []devices.Scope{devices.View}, "", "")
	h := e.s.remoteHandler(RemoteOptions{Hostnames: []string{"twin.test"}})
	r := httptest.NewRequest("GET", "https://twin.test/usage", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestRemoteTailscaleClientSurvivesKeyRotation(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	e := newEnv(t)
	host := "twin.tailnet.ts.net"
	first := remoteNamedSource(t, host)
	second := remoteNamedSource(t, host)
	src := &rotatingSource{current: first}
	pipe := remotePipe(t, e.s, src, RemoteOptions{Via: "tailscale"})
	_, token, _ := e.store.Add("terminal", devices.KindCLI, []devices.Scope{devices.Chat}, "", "")
	c := newClient(Target{Address: "https://" + host, Token: token, Pins: first.SPKIs()})
	tr := c.http.Transport.(*http.Transport)
	tr.Proxy = nil
	tr.DialContext = pipe.DialContext
	roots := x509.NewCertPool()
	roots.AddCert(first.pair.Leaf)
	roots.AddCert(second.pair.Leaf)
	tr.TLSClientConfig.RootCAs = roots
	t.Cleanup(c.http.CloseIdleConnections)
	if _, err := c.Message(context.Background(), "cli", "terminal", "before renewal"); err != nil {
		t.Fatal(err)
	}
	tr.CloseIdleConnections()
	src.mu.Lock()
	src.current = second
	src.mu.Unlock()
	if _, err := c.Message(context.Background(), "cli", "terminal", "after renewal"); err != nil {
		t.Fatal("trusted renewed certificate refused", err)
	}
	tr.CloseIdleConnections()
	src.mu.Lock()
	src.current = remoteNamedSource(t, host)
	src.mu.Unlock()
	if _, err := c.Message(context.Background(), "cli", "terminal", "untrusted certificate"); err == nil {
		t.Fatal("untrusted replacement accepted")
	}
}
