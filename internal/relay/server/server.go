// Package server is mirrin-relay: a stateless SNI passthrough that lets a
// phone on any network reach a twin whose TLS still ends on the user's own
// machine. It peeks at each client's ClientHello and either terminates TLS
// for its own control name, splices the still-encrypted bytes into the
// tunnel of the daemon that holds the name, or closes the connection
// without writing a byte. It holds no tenant key and has no code path that
// could load one. docs/relay-protocol.md is the tunnel protocol;
// docs/relay-selfhost.md is how to run one.
package server

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// Operational limits that are not configurable.
const (
	maxPeeking      = 4096             // connections still being peeked at
	maxPeekingAddr  = 64               // of those, from one address (IPv6: one /64)
	drainRetryAfter = 15               // seconds a draining relay asks daemons to wait
	housekeepEvery  = time.Minute      // ending lapsed entitlements' tunnels; pruning status and logs
	shutdownGrace   = 30 * time.Second // Run's wait for spliced connections to finish
)

// Options are the parts of a relay that do not come from relay.yaml.
type Options struct {
	Log     *slog.Logger // nil discards
	Version string       // shown by /v1/version

	// Certificates serves the control name, overriding cert_file and
	// ACME. Tests inject a test CA's certificate here, which keeps
	// autocert out of them.
	Certificates func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// HTTPClient fetches the deny list. Nil is a client with a 30 s
	// timeout that follows only https redirects.
	HTTPClient *http.Client
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
	// OnSuspend is called when the abuse heuristic suspends a handle, so
	// the operator can be told. It must not block.
	OnSuspend func(handle string, networks int)

	// Tests shorten these.
	denyEvery   time.Duration // the deny list poll
	denyTimeout time.Duration // one deny list refresh, primary and mirror
	sweepEvery  time.Duration // housekeeping
	keepalive   time.Duration // the relay's pings

	// Tests drive polls and wait until verification, tunnel eviction and caching finish.
	denyTicks     <-chan time.Time
	denyRefreshed func()

	tap func(client net.Conn, toDaemon bool, b []byte) // tests see every spliced byte
}

// Server is one relay.
type Server struct {
	cfg     Config
	pol     *policy
	log     *slog.Logger
	now     func() time.Time
	version string
	opts    Options
	certs   func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	client  *http.Client

	keepalive   time.Duration
	denyTimeout time.Duration
	certWatch   certWatch

	reg        Registry
	status     *statusStore
	abuse      *abuse
	hellos     *window
	statusRate *window
	deny       *denyState
	connLog    *connLog
	m          *metrics
	peeking    chan struct{}
	peekers    *perAddr
	ctl        *chanListener
	ctlSrv     *http.Server

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once

	mu        sync.Mutex
	closed    bool
	draining  bool
	tunnels   map[*Tunnel]struct{}
	listeners map[net.Listener]struct{}
	servers   []*http.Server
}

// New checks cfg and builds a relay. Nothing runs until Serve or Run.
func New(cfg Config, opts Options) (*Server, error) {
	pol, err := cfg.compile()
	if err != nil {
		return nil, err
	}
	certs, err := certSource(&cfg, opts.Certificates)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:        cfg,
		pol:        pol,
		log:        opts.Log,
		now:        opts.Now,
		version:    opts.Version,
		opts:       opts,
		certs:      certs,
		client:     opts.HTTPClient,
		reg:        NewRegistry(),
		status:     newStatusStore(),
		abuse:      newAbuse(cfg.Abuse),
		hellos:     newWindow(cfg.Limits.HelloPerIPPerMin),
		statusRate: newWindow(statusPerMin),
		deny:       &denyState{},
		connLog:    newConnLog(cfg.ConnLog),
		m:          newMetrics(),
		peeking:    make(chan struct{}, maxPeeking),
		peekers:    newPerAddr(maxPeekingAddr),
		ctl:        newChanListener(&net.TCPAddr{}),
		tunnels:    map[*Tunnel]struct{}{},
		listeners:  map[net.Listener]struct{}{},
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.version == "" {
		s.version = "dev"
	}
	if s.client == nil {
		s.client = defaultHTTPClient()
	}
	s.keepalive = cmp.Or(opts.keepalive, keepalive*time.Second)
	s.denyTimeout = cmp.Or(opts.denyTimeout, denyTimeout)
	s.loadSuspensions()
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.ctlSrv = s.controlServer()
	return s, nil
}

// start runs the control server and the background jobs, once.
func (s *Server) start() {
	s.once.Do(func() {
		if !s.enter() {
			return
		}
		go func() {
			defer s.wg.Done()
			s.ctlSrv.Serve(tls.NewListener(s.ctl, s.controlTLS()))
		}()
		if s.enter() {
			go s.housekeep()
		}
		if !s.cfg.SelfHosted() && s.enter() {
			go s.pollDenyList()
		}
	})
}

// enter counts one more goroutine the server owns, unless it has closed.
func (s *Server) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	return true
}

// Run listens on the configured addresses and serves until ctx ends, then
// drains: daemons are told to go elsewhere, and spliced connections get
// shutdownGrace to finish.
func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 3)
	listen := func(addr string, serve func(net.Listener) error) error {
		if addr == "" {
			return nil
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		go func() { errc <- serve(ln) }()
		return nil
	}
	err := errors.Join(
		listen(s.cfg.Listen, s.Serve),
		listen(s.cfg.HTTPListen, s.ServeRedirect),
		listen(s.cfg.MetricsListen, s.ServeMetrics),
	)
	if err != nil {
		s.Close()
		return err
	}
	s.log.Info("relay: serving", "id", s.cfg.ID, "mode", s.mode(), "control", s.cfg.ControlHostname, "listen", s.cfg.Listen)
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	s.Shutdown(sctx)
	return err
}

// Serve routes every connection on ln by the SNI in its ClientHello.
func (s *Server) Serve(ln net.Listener) error {
	s.start()
	if !s.track(ln) {
		ln.Close()
		return net.ErrClosed
	}
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return net.ErrClosed
			}
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			s.log.Warn("relay: accept", "err", err, "retry_in", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		from := addrPort(c.RemoteAddr()).Addr()
		select {
		case s.peeking <- struct{}{}:
		default:
			s.m.conns.inc(connBusy)
			c.Close()
			continue
		}
		if !s.peekers.acquire(from) {
			<-s.peeking
			s.m.conns.inc(connBusy)
			c.Close()
			continue
		}
		if !s.enter() {
			s.donePeeking(from)
			c.Close()
			return net.ErrClosed
		}
		go s.route(c, from)
	}
}

// donePeeking frees a connection's places in the peek limits.
func (s *Server) donePeeking(from netip.Addr) {
	s.peekers.release(from)
	<-s.peeking
}

// route peeks at c's ClientHello and sends it on: to the control server,
// into a tunnel, or nowhere, without writing a byte.
func (s *Server) route(c net.Conn, from netip.Addr) {
	defer s.wg.Done()
	stop := context.AfterFunc(s.ctx, func() { c.Close() })
	sni, _, replay, err := PeekClientHello(c, MaxHello, PeekTimeout)
	stopped := stop()
	s.donePeeking(from)
	if err != nil || !stopped {
		s.m.conns.inc(connBadHello)
		c.Close()
		return
	}
	name := lowerASCII(sni)
	if name == s.cfg.ControlHostname {
		s.m.conns.inc(connControl)
		s.ctl.deliver(&replayConn{Conn: c, replay: replay})
		return
	}
	t, ok := s.reg.Lookup(name)
	if !ok {
		s.m.conns.inc(connUnknown)
		c.Close()
		return
	}
	s.splice(c, t, name, replay)
}

// serveHTTP runs one of the relay's plain HTTP servers until Close.
func (s *Server) serveHTTP(srv *http.Server, ln net.Listener) error {
	s.start()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return net.ErrClosed
	}
	s.servers = append(s.servers, srv)
	s.mu.Unlock()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return net.ErrClosed
}

func (s *Server) track(ln net.Listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.draining {
		return false
	}
	s.listeners[ln] = struct{}{}
	return true
}

// Shutdown stops taking connections, tells every daemon the relay is
// going away, waits for spliced connections to finish or ctx to end, and
// then closes everything.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.draining = true
	for ln := range s.listeners {
		ln.Close()
	}
	var tunnels []*Tunnel
	for t := range s.tunnels {
		t.kick(wire.Control{T: wire.ControlDrain, RetryAfter: drainRetryAfter, Message: "This relay is restarting."})
		tunnels = append(tunnels, t)
	}
	s.mu.Unlock()
	for _, t := range tunnels {
		select {
		case <-t.drained:
		case <-t.done:
		case <-ctx.Done():
			return s.Close()
		}
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for s.m.splicing.Load() > 0 {
		select {
		case <-ctx.Done():
			return s.Close()
		case <-tick.C:
		}
	}
	return s.Close()
}

// Close ends every tunnel and connection at once and waits for the
// relay's goroutines to return.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.wg.Wait()
		return nil
	}
	s.closed = true
	for ln := range s.listeners {
		ln.Close()
	}
	servers := s.servers
	s.mu.Unlock()
	s.cancel()
	s.ctl.Close()
	s.ctlSrv.Close()
	for _, srv := range servers {
		srv.Close()
	}
	s.wg.Wait()
	return nil
}

// housekeep ends tunnels whose entitlement has lapsed and prunes what the
// relay remembers.
func (s *Server) housekeep() {
	defer s.wg.Done()
	t := time.NewTicker(cmp.Or(s.opts.sweepEvery, housekeepEvery))
	defer t.Stop()
	for {
		select {
		case <-t.C:
			now := s.now()
			s.expire(now)
			s.status.prune(now)
			s.connLog.prune(now)
		case <-s.ctx.Done():
			return
		}
	}
}
