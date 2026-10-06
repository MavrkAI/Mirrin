// Package relay is the daemon's side of antbot.tunnel.v1. It keeps an
// outbound tunnel open to each relay and merges the connections they carry
// into one net.Listener. Each accepted connection is a client's raw TLS
// stream, and its RemoteAddr is the client's address from the PROXY v2
// header. TLS ends here, under a key only this machine holds; a relay sees
// ciphertext. docs/relay-protocol.md describes the protocol.
package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// Timeouts for one connection attempt, and for the streams it carries.
const (
	handshakeTimeout = 30 * time.Second // dial, TLS, upgrade, challenge, hello, reply
	dialTimeout      = 15 * time.Second // TCP connect
	headerTimeout    = 5 * time.Second  // a data stream's PROXY header
	closeTimeout     = 10 * time.Second // a stream the daemon closed, until yamux resets it
)

// RelayRef names one relay to tunnel to.
type RelayRef struct {
	// ID is the relay's id, such as "r1". The hello is signed for this id,
	// and a relay whose challenge names another is refused. Empty accepts
	// the id the relay gives, for a self-hosted relay whose id is not
	// configured.
	ID string
	// URL is the relay's tunnel endpoint: wss://r1.relay.mirrin.app/v1/tunnel.
	URL string
}

// ClientConfig configures Listen.
type ClientConfig struct {
	// Relays are every relay in the entitlement, or the one self-hosted
	// relay.
	Relays []RelayRef
	// Key is the device key that signs each hello. With an entitlement it
	// must be the key the entitlement is bound to.
	Key ed25519.PrivateKey
	// Entitlement returns the current entitlement token, or "" for a
	// self-hosted relay, which checks its allow list instead. It is called
	// before every hello, from several goroutines, so a refreshed token is
	// used by the next reconnect. Nil means "".
	Entitlement func() string
	// StatusKey is the 32-byte secret behind the relay's status endpoint.
	// A relay only ever sees its SHA-256.
	StatusKey []byte
	// OnControl, if set, is called with every message on a relay's control
	// stream, on that tunnel's control goroutine.
	OnControl func(relay string, c wire.Control)
	// OnRefused, if set, is called when a relay refuses the tunnel or
	// supersedes it. It runs on the tunnel's goroutine before any
	// reconnect, so an entitlement refreshed inside it is in the next
	// hello. After superseded (a higher generation holds the names) that
	// tunnel stops for good, and the daemon stands by; every other code is
	// retried, no sooner than the relay's retry_after.
	OnRefused func(relay string, e wire.Error)
	// Client identifies the build in each hello, as
	// "mirrin/0.4.0 darwin/arm64". Empty means "mirrin/dev" and the
	// platform.
	Client string
	// RootCAs verifies relay certificates. Nil means the system roots.
	RootCAs *x509.CertPool
	// MaxBackoff caps the wait between attempts. Zero is a minute. A
	// dual-homed daemon sets a few seconds, so a relay that comes back is
	// used again at once while the other carries the traffic meanwhile.
	MaxBackoff time.Duration
	// Log receives tunnel events. Nil discards them.
	Log *slog.Logger

	// Tests shorten or watch these.
	backoff       backoff
	headerTimeout time.Duration
	closeTimeout  time.Duration
	observe       func(s TunnelStatus) // every Online change
	session       func(*yamux.Session) // every session, once welcomed
}

func (c *ClientConfig) defaults() {
	if c.backoff.max <= 0 && c.MaxBackoff > 0 {
		c.backoff.max = c.MaxBackoff
		c.backoff.min = min(time.Second, c.MaxBackoff)
	}
	c.backoff.defaults()
	if c.headerTimeout <= 0 {
		c.headerTimeout = headerTimeout
	}
	if c.closeTimeout <= 0 {
		c.closeTimeout = closeTimeout
	}
}

// TunnelStatus is one relay's tunnel as Status reports it.
type TunnelStatus struct {
	Relay      string    // the relay's id: RelayRef.ID, or the id it gave
	URL        string    // RelayRef.URL
	Online     bool      // welcomed and carrying streams
	Since      time.Time // when Online last changed; zero before the first change
	Hostnames  []string  // from the last welcome
	Gen        int64     // from the last welcome
	MaxStreams int       // from the last welcome or limits message
	Draining   bool      // the relay said it is going away
	Refused    *wire.Error
	Stopped    bool   // superseded: this tunnel will not reconnect
	LastError  string // why the last attempt or session ended
}

// backoff waits between attempts: a random time in [0, ceiling) ("full
// jitter"), where the ceiling doubles from min to max with each failure. A
// tunnel that stayed up for stable starts over.
type backoff struct {
	min, max, stable time.Duration
	n                int
	jitter           func(ceil time.Duration) time.Duration // tests fix it; nil is rand.N
}

func (b *backoff) defaults() {
	if b.min <= 0 {
		b.min = time.Second
	}
	if b.max <= 0 {
		b.max = time.Minute
	}
	if b.stable <= 0 {
		b.stable = time.Minute
	}
}

func (b *backoff) next() time.Duration {
	ceil := b.max
	if b.n < 30 && b.min<<b.n < ceil {
		ceil = b.min << b.n
	}
	b.n++
	if b.jitter != nil {
		return b.jitter(ceil)
	}
	return rand.N(ceil)
}

// tunnel keeps one relay connected for the life of the listener.
type tunnel struct {
	l   *Listener
	ref RelayRef
	log *slog.Logger

	mu sync.Mutex
	st TunnelStatus
}

// sessionEnd records why a session ended, when a goroutine other than the
// accept loop decided it.
type sessionEnd struct {
	mu    sync.Mutex
	err   error
	after time.Duration // the relay asked for at least this long before reconnecting
}

func (e *sessionEnd) set(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err == nil {
		e.err = err
	}
}

// waitAtLeast records a relay's request to wait before reconnecting.
func (e *sessionEnd) waitAtLeast(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.after = max(e.after, d)
}

func (e *sessionEnd) get() (time.Duration, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.after, e.err
}

// run connects, serves and reconnects until the listener closes or the
// relay supersedes this machine.
func (t *tunnel) run() {
	defer t.l.wg.Done()
	ctx := t.l.ctx
	b := t.l.cfg.backoff
	for {
		up, after, err := t.session(ctx)
		if ctx.Err() != nil {
			t.setOffline(nil)
			return
		}
		var refused wire.Error
		if errors.As(err, &refused) {
			t.refused(refused)
			if refused.Code == wire.CodeSuperseded {
				t.log.Warn("relay: superseded; tunnel stopped", "relay", t.id(), "message", refused.Message)
				return
			}
			after = max(after, time.Duration(refused.RetryAfter)*time.Second)
		}
		t.setOffline(err)
		if up >= b.stable {
			b.n = 0
		}
		wait := max(after, b.next())
		t.log.Info("relay: tunnel down", "relay", t.id(), "err", err, "retry_in", wait.Round(time.Millisecond))
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.setOffline(nil)
			return
		}
	}
}

// session makes one attempt and, once welcomed, serves until the session
// ends. It returns how long the tunnel was up, any wait the relay asked
// for, and why it ended.
func (t *tunnel) session(ctx context.Context) (up, after time.Duration, err error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The attempt must finish its handshake in time; after that the
	// session lives as long as ctx. Cancelling sctx closes the WebSocket.
	timer := time.AfterFunc(handshakeTimeout, cancel)
	ws, exporter, err := t.dial(sctx)
	if err != nil {
		timer.Stop()
		return 0, 0, err
	}
	w, relayID, err := t.handshake(sctx, ws, exporter)
	if !timer.Stop() {
		err = errors.Join(errors.New("relay: handshake timed out"), err)
	}
	if err != nil {
		ws.CloseNow()
		return 0, 0, err
	}
	nc := websocket.NetConn(sctx, ws, websocket.MessageBinary)
	sess, err := yamux.Server(nc, t.yamuxConfig(w))
	if err != nil {
		nc.Close()
		return 0, 0, err
	}
	if f := t.l.cfg.session; f != nil {
		f(sess)
	}
	start := time.Now()
	t.setOnline(relayID, w)
	t.log.Info("relay: tunnel up", "relay", relayID, "hostnames", w.Hostnames, "gen", w.Gen)
	after, err = t.serve(sctx, sess, relayID, w)
	return time.Since(start), after, err
}

// dial opens the WebSocket and returns it with the TLS exporter of the
// connection underneath. A fresh transport per attempt dials exactly one
// TLS connection, which is how the exporter is known to belong to it.
func (t *tunnel) dial(ctx context.Context) (*websocket.Conn, []byte, error) {
	var (
		mu    sync.Mutex
		conns []*tls.Conn
	)
	tlsConf := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    t.l.cfg.RootCAs,
		NextProtos: []string{"http/1.1"},
	}
	tr := &http.Transport{
		Proxy: nil, // tunnels go direct; the exporter must be of our own TLS session
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("relay: tunnels need TLS")
		},
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := tls.Dialer{NetDialer: &net.Dialer{Timeout: dialTimeout}, Config: tlsConf}
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			conns = append(conns, c.(*tls.Conn))
			return c, nil
		},
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 16 << 10,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ws, _, err := websocket.Dial(ctx, t.ref.URL, &websocket.DialOptions{
		HTTPClient:      client,
		Subprotocols:    []string{wire.Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, nil, err
	}
	mu.Lock()
	tc, err := onlyConn(conns)
	mu.Unlock()
	fail := func(err error) (*websocket.Conn, []byte, error) {
		ws.CloseNow()
		return nil, nil, err
	}
	if err != nil {
		return fail(err)
	}
	if ws.Subprotocol() != wire.Subprotocol {
		return fail(fmt.Errorf("relay: subprotocol %q", ws.Subprotocol()))
	}
	exporter, err := wire.Exporter(tc.ConnectionState())
	if err != nil {
		return fail(err)
	}
	return ws, exporter, nil
}

// onlyConn is the one TLS connection a dial made. Any other count means the
// exporter cannot be tied to the WebSocket, and the attempt fails.
func onlyConn(conns []*tls.Conn) (*tls.Conn, error) {
	if len(conns) != 1 {
		return nil, fmt.Errorf("relay: %d TLS connections for one dial", len(conns))
	}
	return conns[0], nil
}

// handshake answers the relay's challenge and reads its reply. A refusal
// comes back as a wire.Error.
func (t *tunnel) handshake(ctx context.Context, ws *websocket.Conn, exporter []byte) (wire.Welcome, string, error) {
	ws.SetReadLimit(wire.MaxMessage)
	b, err := readText(ctx, ws)
	if err != nil {
		return wire.Welcome{}, "", err
	}
	ch, err := wire.ParseChallenge(b)
	if err != nil {
		return wire.Welcome{}, "", err
	}
	if t.ref.ID == "" {
		t.update(func(s *TunnelStatus) { s.Relay = ch.Relay })
	} else if ch.Relay != t.ref.ID {
		return wire.Welcome{}, "", fmt.Errorf("relay: %s says it is %q", t.ref.ID, ch.Relay)
	}
	cfg := &t.l.cfg
	ent := ""
	if cfg.Entitlement != nil {
		ent = cfg.Entitlement()
	}
	h, err := wire.SignHello(cfg.Key, ch, exporter, ent, t.l.statusKeyHash, t.l.client)
	if err != nil {
		return wire.Welcome{}, "", err
	}
	if b, err = h.Marshal(); err != nil {
		return wire.Welcome{}, "", err
	}
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		return wire.Welcome{}, "", err
	}
	if b, err = readText(ctx, ws); err != nil {
		return wire.Welcome{}, "", err
	}
	w, err := wire.ParseReply(b)
	return w, ch.Relay, err
}

func readText(ctx context.Context, ws *websocket.Conn) ([]byte, error) {
	typ, b, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, fmt.Errorf("%w: binary frame in the handshake", wire.ErrMalformed)
	}
	return b, nil
}

func (t *tunnel) yamuxConfig(w wire.Welcome) *yamux.Config {
	c := yamux.DefaultConfig()
	c.AcceptBacklog = acceptBacklog
	c.StreamCloseTimeout = t.l.cfg.closeTimeout
	c.EnableKeepAlive = true
	c.KeepAliveInterval = time.Duration(w.Keepalive) * time.Second
	c.LogOutput = nil
	c.Logger = slog.NewLogLogger(t.log.Handler(), slog.LevelDebug)
	return c
}

func (t *tunnel) id() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st.Relay
}

func (t *tunnel) status() TunnelStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.st
	s.Hostnames = append([]string(nil), s.Hostnames...)
	if s.Refused != nil {
		r := *s.Refused
		s.Refused = &r
	}
	return s
}

func (t *tunnel) setOnline(relayID string, w wire.Welcome) {
	t.mu.Lock()
	t.st.Relay = relayID
	t.st.Online, t.st.Since = true, time.Now()
	t.st.Hostnames, t.st.Gen, t.st.MaxStreams = w.Hostnames, w.Gen, w.MaxStreams
	t.st.Draining, t.st.Refused, t.st.LastError = false, nil, ""
	t.mu.Unlock()
	t.observe()
}

func (t *tunnel) setOffline(err error) {
	t.mu.Lock()
	changed := t.st.Online
	if changed {
		t.st.Online, t.st.Since = false, time.Now()
	}
	if err != nil {
		t.st.LastError = err.Error()
	}
	t.mu.Unlock()
	if changed {
		t.observe()
	}
}

func (t *tunnel) observe() {
	if f := t.l.cfg.observe; f != nil {
		f(t.status())
	}
}

func (t *tunnel) update(f func(*TunnelStatus)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(&t.st)
}

func (t *tunnel) refused(e wire.Error) {
	t.setOffline(nil)
	t.mu.Lock()
	t.st.Refused, t.st.LastError = &e, e.Error()
	t.st.Stopped = e.Code == wire.CodeSuperseded
	relay := t.st.Relay
	t.mu.Unlock()
	t.log.Warn("relay: refused", "relay", relay, "code", e.Code, "message", e.Message)
	if f := t.l.cfg.OnRefused; f != nil {
		f(relay, e)
	}
}

// statusKeyHash is what a hello carries for the status endpoint.
func statusKeyHash(k []byte) []byte {
	h := sha256.Sum256(k)
	return h[:]
}

func defaultClient() string {
	return "mirrin/dev " + runtime.GOOS + "/" + runtime.GOARCH
}
