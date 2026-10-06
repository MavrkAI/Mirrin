package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// fakeRelay is an in-process relay: an httptest TLS server that speaks the
// tunnel handshake, then drives a yamux session as the client, the way
// mirrin-relay does. It checks hellos exactly as a relay must, against its
// own challenge and its side of the TLS exporter.
type fakeRelay struct {
	t       testing.TB
	id      string // the id the relay signs for
	claim   string // the id its challenge names; normally id
	welcome wire.Welcome
	srv     *httptest.Server
	url     string
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu       sync.Mutex
	backlog  int          // the yamux client's AcceptBacklog; 0 is yamux's default
	refusals []wire.Error // each hello takes the next; none left means welcome
	hellos   []helloSeen
	raw      []net.Conn // TCP connections, so a test can kill them
	sessions chan *fakeSession
}

type helloSeen struct {
	wire.Hello
	at       time.Time
	verified bool
}

type fakeSession struct {
	*yamux.Session
	ctrl  *yamux.Stream // stream 1
	hello wire.Hello
}

func newFakeRelay(t testing.TB, id string, w wire.Welcome) *fakeRelay {
	f := &fakeRelay{t: t, id: id, claim: id, welcome: w, sessions: make(chan *fakeSession, 64)}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.Listener = &trackingListener{Listener: f.srv.Listener, f: f}
	f.srv.EnableHTTP2 = false
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.srv.StartTLS()
	f.url = strings.Replace(f.srv.URL, "https://", "wss://", 1) + wire.Path
	t.Cleanup(f.Close)
	return f
}

func welcome(hosts ...string) wire.Welcome {
	if len(hosts) == 0 {
		hosts = []string{"h.test"}
	}
	return wire.Welcome{Hostnames: hosts, Gen: 3, Keepalive: 25, MaxStreams: 256}
}

func (f *fakeRelay) ref() RelayRef { return RelayRef{ID: f.id, URL: f.url} }

func (f *fakeRelay) roots() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(f.srv.Certificate())
	return p
}

// refuse queues a refusal for the next hello.
func (f *fakeRelay) refuse(e wire.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusals = append(f.refusals, e)
}

func (f *fakeRelay) seen() []helloSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]helloSeen(nil), f.hellos...)
}

// kill drops every TCP connection to the relay without a goodbye.
func (f *fakeRelay) kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.raw {
		c.Close()
	}
	f.raw = nil
}

// session waits for the next welcomed tunnel.
func (f *fakeRelay) session(t testing.TB) *fakeSession {
	t.Helper()
	select {
	case s := <-f.sessions:
		return s
	case <-time.After(10 * time.Second):
		t.Fatalf("relay %s: no tunnel", f.id)
		return nil
	}
}

func (f *fakeRelay) Close() {
	f.srv.Close() // no new handlers after this
	f.cancel()
	f.kill()
	f.wg.Wait()
}

func (f *fakeRelay) serve(w http.ResponseWriter, r *http.Request) {
	f.wg.Add(1)
	defer f.wg.Done()
	if r.URL.Path != wire.Path {
		http.NotFound(w, r)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wire.Subprotocol}})
	if err != nil {
		return
	}
	defer c.CloseNow()
	if c.Subprotocol() != wire.Subprotocol {
		return
	}
	ctx := f.ctx
	exporter, err := wire.Exporter(*r.TLS)
	if err != nil {
		return
	}
	ch, err := wire.NewChallenge(f.claim)
	if err != nil {
		return
	}
	b, _ := ch.Marshal()
	if c.Write(ctx, websocket.MessageText, b) != nil {
		return
	}
	c.SetReadLimit(wire.MaxMessage)
	_, b, err = c.Read(ctx)
	if err != nil {
		return
	}
	h, err := wire.ParseHello(b)
	if err != nil {
		return
	}
	_, verr := wire.VerifyHello(h, f.id, ch.Nonce, exporter)
	f.mu.Lock()
	f.hellos = append(f.hellos, helloSeen{Hello: h, at: time.Now(), verified: verr == nil})
	backlog := f.backlog
	var refusal *wire.Error
	if verr != nil {
		refusal = &wire.Error{Code: wire.CodeBadSignature, Message: "The hello did not verify."}
	} else if len(f.refusals) > 0 {
		refusal = &f.refusals[0]
		f.refusals = f.refusals[1:]
	}
	f.mu.Unlock()
	if refusal != nil {
		b, _ = refusal.Marshal()
		c.Write(ctx, websocket.MessageText, b)
		c.Close(websocket.StatusNormalClosure, "")
		return
	}
	b, _ = f.welcome.Marshal()
	if c.Write(ctx, websocket.MessageText, b) != nil {
		return
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = time.Duration(f.welcome.Keepalive) * time.Second
	if backlog > 0 {
		cfg.AcceptBacklog = backlog
	}
	sess, err := yamux.Client(websocket.NetConn(ctx, c, websocket.MessageBinary), cfg)
	if err != nil {
		return
	}
	defer sess.Close()
	ctrl, err := sess.OpenStream()
	if err != nil {
		return
	}
	select {
	case f.sessions <- &fakeSession{Session: sess, ctrl: ctrl, hello: h}:
	case <-ctx.Done():
		return
	}
	select {
	case <-sess.CloseChan():
	case <-ctx.Done():
	}
}

// open starts a relayed connection: a stream and its PROXY header.
func (s *fakeSession) open(t testing.TB, h wire.ProxyHeader) *yamux.Stream {
	t.Helper()
	st, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteProxyV2(st, h); err != nil {
		t.Fatal(err)
	}
	return st
}

func (s *fakeSession) control(t testing.TB, c wire.Control) {
	t.Helper()
	if err := wire.WriteControl(s.ctrl, c); err != nil {
		t.Fatal(err)
	}
}

type trackingListener struct {
	net.Listener
	f *fakeRelay
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.f.mu.Lock()
		l.f.raw = append(l.f.raw, c)
		l.f.mu.Unlock()
	}
	return c, err
}

// testKey is the device key every test daemon signs with.
var testKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))

// config returns a client config for these relays with a short backoff.
func config(relays ...*fakeRelay) ClientConfig {
	cfg := ClientConfig{
		Key:       testKey,
		StatusKey: make([]byte, 32),
		RootCAs:   relays[0].roots(), // httptest servers share one certificate
		backoff:   backoff{min: 20 * time.Millisecond, max: 200 * time.Millisecond},
	}
	for _, r := range relays {
		cfg.Relays = append(cfg.Relays, r.ref())
	}
	return cfg
}

func listen(t testing.TB, cfg ClientConfig) *Listener {
	t.Helper()
	l, err := Listen(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		l.Close()
		<-l.Done()
	})
	return l
}

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
