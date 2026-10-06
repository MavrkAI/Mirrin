package relay

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// Listener merges every relay's tunnel into one net.Listener. Serve it
// with a TLS server: each connection is a client's raw TLS stream.
type Listener struct {
	cfg           ClientConfig
	ctx           context.Context
	cancel        context.CancelFunc
	conns         chan *Conn
	tunnels       []*tunnel
	wg            sync.WaitGroup
	done          chan struct{}
	statusKeyHash []byte
	client        string
	log           *slog.Logger
}

// Listen starts a tunnel to every relay in cfg and returns at once; each
// tunnel connects, and reconnects, on its own. Cancelling ctx is the same
// as Close.
func Listen(ctx context.Context, cfg ClientConfig) (*Listener, error) {
	if err := checkConfig(&cfg); err != nil {
		return nil, err
	}
	cfg.Relays = slices.Clone(cfg.Relays)
	cfg.defaults()
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	client := cfg.Client
	if client == "" {
		client = defaultClient()
	}
	l := &Listener{
		cfg:           cfg,
		conns:         make(chan *Conn),
		done:          make(chan struct{}),
		statusKeyHash: statusKeyHash(cfg.StatusKey),
		client:        client,
		log:           log,
	}
	l.ctx, l.cancel = context.WithCancel(ctx)
	for _, ref := range cfg.Relays {
		t := &tunnel{l: l, ref: ref, log: log, st: TunnelStatus{Relay: ref.ID, URL: ref.URL}}
		l.tunnels = append(l.tunnels, t)
		l.wg.Add(1)
		go t.run()
	}
	go func() {
		l.wg.Wait()
		close(l.done)
	}()
	return l, nil
}

func checkConfig(cfg *ClientConfig) error {
	if len(cfg.Relays) == 0 {
		return errors.New("relay: no relays")
	}
	urls, ids := map[string]bool{}, map[string]bool{}
	for _, r := range cfg.Relays {
		u, err := url.Parse(r.URL)
		if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("relay: tunnel URL %q must be wss://host/…", r.URL)
		}
		if r.ID != "" && !wire.ValidRelayID(r.ID) {
			return fmt.Errorf("relay: bad relay id %q", r.ID)
		}
		if urls[r.URL] || ids[r.ID] {
			return fmt.Errorf("relay: %s listed twice", r.URL)
		}
		urls[r.URL] = true
		if r.ID != "" {
			ids[r.ID] = true
		}
	}
	if len(cfg.Key) != ed25519.PrivateKeySize {
		return errors.New("relay: bad device key")
	}
	if len(cfg.StatusKey) != wire.StatusKeySize {
		return fmt.Errorf("relay: the status key must be %d bytes", wire.StatusKeySize)
	}
	if cfg.Client != "" && (len(cfg.Client) > 128 || strings.ContainsFunc(cfg.Client, func(r rune) bool { return r < 0x20 || r > 0x7e })) {
		return fmt.Errorf("relay: bad client string %q", cfg.Client)
	}
	return nil
}

// Accept returns the next client connection from any tunnel. After Close,
// or once every relay has superseded this machine, it returns
// net.ErrClosed.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close ends every tunnel, and with them every connection accepted from
// them. It does not wait; Done says when the tunnels have stopped. It is
// safe to call from OnControl and OnRefused.
func (l *Listener) Close() error {
	l.cancel()
	return nil
}

// Done is closed once every tunnel goroutine has returned: after Close, or
// once every relay has superseded this machine.
func (l *Listener) Done() <-chan struct{} { return l.done }

// Addr names the listener; relayed connections have no local socket.
func (l *Listener) Addr() net.Addr {
	urls := make([]string, len(l.tunnels))
	for i, t := range l.tunnels {
		urls[i] = t.ref.URL
	}
	return addr(strings.Join(urls, ","))
}

// Status reports each relay's tunnel, in the order of cfg.Relays.
func (l *Listener) Status() []TunnelStatus {
	out := make([]TunnelStatus, len(l.tunnels))
	for i, t := range l.tunnels {
		out[i] = t.status()
	}
	return out
}

type addr string

func (addr) Network() string  { return "relay" }
func (a addr) String() string { return string(a) }

// serve accepts the relay's streams until the session ends. Stream 1 is
// control; every other stream is one client connection and counts against
// the session's stream limit until yamux lets go of it. It returns once
// every goroutine it started has.
func (t *tunnel) serve(ctx context.Context, sess *yamux.Session, relayID string, w wire.Welcome) (time.Duration, error) {
	var (
		wg  sync.WaitGroup
		end sessionEnd
		lim = newStreamLimit(w.MaxStreams)
	)
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	var err error
	for {
		if !lim.acquire(sess.CloseChan()) {
			err = yamux.ErrSessionShutdown
			break
		}
		var st *yamux.Stream
		st, err = sess.AcceptStream()
		if err != nil {
			break
		}
		if st.StreamID() == wire.ControlStreamID {
			lim.release() // control does not count
			wg.Add(1)
			go func() {
				defer wg.Done()
				t.control(st, sess, w, lim, &end)
			}()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.stream(st, sess.CloseChan(), relayID, w.Hostnames, lim.release)
		}()
	}
	sess.Close()
	wg.Wait()
	after, e := end.get()
	if e == nil {
		e = err
	}
	return after, e
}

// control reads the control stream. superseded ends the session: a higher
// generation means this machine lost its names, and the same generation (a
// newer token for them) means refresh and reconnect.
func (t *tunnel) control(st *yamux.Stream, sess *yamux.Session, w wire.Welcome, lim *streamLimit, end *sessionEnd) {
	defer st.Close()
	cr := wire.NewControlReader(st)
	for {
		c, err := cr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !sess.IsClosed() {
				end.set(fmt.Errorf("relay: control stream: %w", err))
				sess.Close()
			}
			return
		}
		if f := t.l.cfg.OnControl; f != nil {
			f(t.id(), c)
		}
		switch c.T {
		case wire.ControlSuperseded:
			code := wire.CodeSuperseded
			if c.Gen <= w.Gen {
				code = wire.CodeSupersededRetry
			}
			end.set(wire.Error{Code: code, Message: c.Message})
			sess.Close()
			return
		case wire.ControlDrain:
			end.waitAtLeast(time.Duration(c.RetryAfter) * time.Second)
			t.update(func(s *TunnelStatus) { s.Draining = true })
		case wire.ControlLimits:
			if c.MaxStreams > 0 {
				lim.setLimit(c.MaxStreams)
				t.update(func(s *TunnelStatus) { s.MaxStreams = c.MaxStreams })
			}
		}
	}
}

// stream reads a data stream's PROXY header and hands the connection to
// Accept. A stream whose header is late, malformed, from another relay, or
// for a name this tunnel does not carry is closed. release frees the
// stream's place in the session's limit once yamux has let go of it.
func (t *tunnel) stream(st *yamux.Stream, sessDone <-chan struct{}, relayID string, hostnames []string, release func()) {
	cfg := &t.l.cfg
	st.SetReadDeadline(time.Now().Add(cfg.headerTimeout))
	br := bufio.NewReader(st)
	h, err := wire.ReadProxyV2(br)
	switch {
	case err != nil:
	case h.Local:
		err = errors.New("LOCAL command")
	case h.RelayID != relayID:
		err = fmt.Errorf("relay id %q", h.RelayID)
	case !carries(hostnames, h.Authority):
		err = fmt.Errorf("authority %q", h.Authority)
	}
	if err == nil {
		st.SetReadDeadline(time.Time{})
		c := &Conn{st: st, br: br, hdr: h, release: release, closeTimeout: cfg.closeTimeout}
		select {
		case t.l.conns <- c:
			return
		case <-sessDone:
		case <-t.l.ctx.Done():
		}
	} else {
		t.log.Debug("relay: dropping stream", "relay", relayID, "err", err)
	}
	st.Close()
	linger(st, cfg.closeTimeout)
	release()
}

func carries(hostnames []string, authority string) bool {
	for _, h := range hostnames {
		if strings.EqualFold(h, authority) {
			return true
		}
	}
	return false
}

// Conn is one client connection carried by a tunnel. Its bytes are the
// client's raw TLS stream, from the ClientHello on.
type Conn struct {
	st  *yamux.Stream
	hdr wire.ProxyHeader

	rmu sync.Mutex
	br  *bufio.Reader // bytes read past the PROXY header; nil once drained

	closeOnce    sync.Once
	closed       atomic.Bool
	release      func()
	closeTimeout time.Duration
}

// Read reads the client's bytes.
func (c *Conn) Read(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if c.br != nil {
		if c.br.Buffered() > 0 {
			return c.br.Read(p)
		}
		c.br = nil
	}
	return c.st.Read(p)
}

// Write sends bytes to the client.
func (c *Conn) Write(p []byte) (int, error) { return c.st.Write(p) }

// Close closes the stream; the relay then closes the client's connection.
// The stream keeps its place in the tunnel's stream limit until the relay
// has closed its half as well, or yamux has reset it.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		err = c.st.Close()
		go func() {
			linger(c.st, c.closeTimeout)
			c.release()
		}()
	})
	return err
}

// RemoteAddr is the client's address, from the PROXY header.
func (c *Conn) RemoteAddr() net.Addr { return net.TCPAddrFromAddrPort(c.hdr.Source) }

// LocalAddr is the relay address the client connected to.
func (c *Conn) LocalAddr() net.Addr { return net.TCPAddrFromAddrPort(c.hdr.Dest) }

func (c *Conn) SetDeadline(t time.Time) error      { return c.st.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.st.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.st.SetWriteDeadline(t) }

// Relay is the id of the relay that carried the connection.
func (c *Conn) Relay() string { return c.hdr.RelayID }

// ServerName is the SNI the relay routed the connection by.
func (c *Conn) ServerName() string { return c.hdr.Authority }
