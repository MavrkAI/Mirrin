package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// Tunnel timings.
const (
	handshakeTimeout = 10 * time.Second // challenge, hello, reply
	keepalive        = 25               // seconds between yamux pings, each way; announced in the welcome
	pingTimeout      = 10 * time.Second // a daemon that does not answer a ping in this time is gone
	controlTimeout   = 5 * time.Second  // writing one control line
	streamReady      = 5 * time.Second  // a client waits this long for a tunnel's session
	deniedRetry      = 3600             // seconds a denied daemon waits
)

// serveTunnel is GET /v1/tunnel: the mirrin.tunnel.v1 handshake, then a
// yamux session with the relay as client, until the tunnel ends. The
// WebSocket is hijacked from the HTTP server and outlives this handler.
func (s *Server) serveTunnel(w http.ResponseWriter, r *http.Request) {
	if !s.enter() {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}
	defer s.wg.Done()
	from := remoteAddr(r.RemoteAddr)
	limited := !s.hellos.allow(from.Addr(), s.now())
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{wire.Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	if ws.Subprotocol() != wire.Subprotocol {
		ws.Close(websocket.StatusPolicyViolation, "subprotocol "+wire.Subprotocol+" required")
		return
	}
	t, refusal := s.handshake(ws, r, limited)
	if t == nil {
		if refusal == nil {
			ws.CloseNow()
			return
		}
		s.m.hellos.inc(refusal.Code)
		s.log.Info("relay: tunnel refused", "code", refusal.Code, "daemon", netOf(from.Addr()))
		if b, err := refusal.Marshal(); err == nil {
			ctx, cancel := context.WithTimeout(s.ctx, controlTimeout)
			ws.Write(ctx, websocket.MessageText, b)
			cancel()
		}
		ws.Close(websocket.StatusNormalClosure, "")
		return
	}
	t.daemon = from.Addr()
	// The session runs on a fresh goroutine and this handler returns, so
	// an idle tunnel does not keep the HTTP request, or the stack that
	// grew during the TLS handshake, for the rest of its life.
	if !s.enter() {
		ws.CloseNow()
		return
	}
	go func() {
		defer s.wg.Done()
		s.runTunnel(ws, t)
	}()
}

// handshake runs challenge, hello and the checks. It returns the tunnel to
// attach, or the refusal to send; neither means the attempt just ends.
func (s *Server) handshake(ws *websocket.Conn, r *http.Request, limited bool) (*Tunnel, *wire.Error) {
	ctx, cancel := context.WithTimeout(s.ctx, handshakeTimeout)
	defer cancel()
	exporter, err := wire.Exporter(*r.TLS)
	if err != nil {
		return nil, nil
	}
	ch, err := wire.NewChallenge(s.cfg.ID)
	if err != nil {
		return nil, nil
	}
	b, err := ch.Marshal()
	if err != nil || ws.Write(ctx, websocket.MessageText, b) != nil {
		return nil, nil
	}
	ws.SetReadLimit(wire.MaxMessage)
	typ, b, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		return nil, nil
	}
	h, err := wire.ParseHello(b)
	switch {
	case errors.Is(err, wire.ErrVersion):
		return nil, refuse(wire.CodeUpgradeRequired, "This relay speaks mirrin.tunnel.v1; update Mirrin.", 0)
	case err != nil:
		return nil, refuse(wire.CodeBadSignature, "The relay could not read the hello.", 0)
	case limited:
		return nil, refuse(wire.CodeRateLimited, "Too many tunnel attempts from this address; trying again shortly.", 60)
	}
	if _, err := wire.VerifyHello(h, s.cfg.ID, ch.Nonce, exporter); err != nil {
		return nil, refuse(wire.CodeBadSignature, "The hello did not verify.", 0)
	}
	now := s.now()
	t, refusal := s.authenticate(h, now)
	if refusal != nil {
		return nil, refusal
	}
	if s.abuse.isSuspended(t.Handle, now) {
		return nil, refuse(wire.CodeDenied, "This address is suspended while it is under review.", deniedRetry)
	}
	return t, nil
}

func refuse(code, msg string, retryAfter int) *wire.Error {
	return &wire.Error{Code: code, Message: msg, RetryAfter: retryAfter}
}

// authenticate decides which names the hello's key may carry. Hosted mode
// checks, in order: the entitlement verifies under a pinned ent-* key and
// has not expired; it is bound to this key; its hosts are within this
// relay's zones; neither handle nor key is deny-listed. Self-host mode
// looks the key up in the allow list.
func (s *Server) authenticate(h wire.Hello, now time.Time) (*Tunnel, *wire.Error) {
	t := newTunnel()
	t.Key, t.StatusKeyHash = h.Key, h.StatusKeyHash
	t.limit = int64(s.cfg.Limits.StreamsPerTunnel)
	t.bucket = newBucket(s.cfg.Limits.BPS)
	if s.pol.allow != nil {
		names := s.pol.allow[entitle.EncodeKey(h.Key)]
		if len(names) == 0 {
			return nil, refuse(wire.CodeHostnameNotAllowed, "This relay's allow list has no hostname for this device key.", 0)
		}
		t.Hostnames, t.Handle, t.Iat = names, names[0], now
		return t, nil
	}
	if h.Ent == "" {
		return nil, refuse(wire.CodeHostnameNotAllowed, "This relay carries only names with an entitlement.", 0)
	}
	c, err := entitle.Verify(h.Ent, s.pol.entKeys, now)
	switch {
	case errors.Is(err, entitle.ErrExpired):
		return nil, refuse(wire.CodeEntitlementExpired, "The entitlement has expired.", 0)
	case errors.Is(err, entitle.ErrNotYetValid):
		return nil, refuse(wire.CodeEntitlementExpired, "The entitlement is not valid yet; check this computer's clock.", 0)
	case err != nil:
		return nil, refuse(wire.CodeBadSignature, "The entitlement did not verify.", 0)
	}
	if k, err := c.Key(); err != nil || !k.Equal(h.Key) {
		return nil, refuse(wire.CodeBadSignature, "The entitlement belongs to another device key.", 0)
	}
	hosts, ok := s.hostsFor(c)
	if !ok {
		return nil, refuse(wire.CodeHostnameNotAllowed, "The entitlement names a hostname this relay does not carry.", 0)
	}
	if why, denied := s.deny.denies(c.Handle, h.Key); denied {
		return nil, refuse(wire.CodeDenied, deniedMessage(why), deniedRetry)
	}
	t.Hostnames, t.Handle, t.Gen, t.Iat, t.exp = hosts, c.Handle, c.Gen, c.Iat, c.Exp
	return t, nil
}

// hostsFor returns an entitlement's hostnames if the relay may carry every
// one: each must be <handle>.<zone> for one of this relay's zones, and the
// entitlement must include Reach.
func (s *Server) hostsFor(c entitle.Claims) ([]string, bool) {
	if !c.Has("reach") || len(c.Hosts) == 0 || len(c.Hosts) > wire.MaxHostnames ||
		!wire.ValidHostname(c.Handle) || strings.Contains(c.Handle, ".") {
		return nil, false
	}
	hosts := slices.Clone(c.Hosts)
	for _, h := range hosts {
		if !slices.ContainsFunc(s.cfg.Zones, func(z string) bool { return h == c.Handle+"."+z }) {
			return nil, false
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts), true
}

// supersede tells displaced tunnel d that t holds its names now. A gen
// above d's makes its daemon stand by; the same gen makes it refresh and
// reconnect. Self-hosted tunnels are all gen 0, so there another device
// key counts as a higher gen (another machine holds the name) and the same
// key as a reconnect of the same machine.
func supersede(d, t *Tunnel) wire.Control {
	gen := t.Gen
	if gen == d.Gen && !d.Key.Equal(t.Key) {
		gen = d.Gen + 1
	}
	return wire.Control{T: wire.ControlSuperseded, Gen: gen, Message: "Another connection now holds this twin's address."}
}

// deniedMessage is what a denied daemon is told, with the deny list's
// reason when the wire can carry it verbatim. A reason the wire would
// refuse (a control or formatting character, a no-break space, too long)
// is dropped, so the daemon still hears "denied" and its retry_after
// rather than a bare close.
func deniedMessage(why string) string {
	const base = "This twin's address is suspended."
	m := base + " " + why
	if why == "" || !showable(m) {
		return base
	}
	return m
}

// showable reports whether m can be sent as both an error and a control
// message, which the daemon shows verbatim.
func showable(m string) bool {
	_, errE := refuse(wire.CodeDenied, m, deniedRetry).Marshal()
	_, errC := wire.Control{T: wire.ControlNotice, Message: m}.Marshal()
	return errE == nil && errC == nil
}

// runTunnel attaches t, welcomes the daemon and serves the session until
// it ends: the daemon goes away, the tunnel is kicked (superseded,
// denied, suspended) or the relay closes.
func (s *Server) runTunnel(ws *websocket.Conn, t *Tunnel) {
	defer close(t.done)
	now := s.now()
	s.mu.Lock()
	if s.draining || s.closed {
		s.mu.Unlock()
		ws.CloseNow()
		return
	}
	displaced, err := s.reg.Attach(t)
	if err != nil {
		s.mu.Unlock()
		code, msg := wire.CodeSupersededRetry, "A newer entitlement for this handle is connected; refreshing."
		if errors.Is(err, ErrSuperseded) {
			code, msg = wire.CodeSuperseded, "Another machine now holds this handle."
		}
		s.m.hellos.inc(code)
		s.log.Info("relay: tunnel refused", "code", code, "handle", t.Handle, "gen", t.Gen, "daemon", netOf(t.daemon))
		if b, err := refuse(code, msg, 0).Marshal(); err == nil {
			ctx, cancel := context.WithTimeout(s.ctx, controlTimeout)
			ws.Write(ctx, websocket.MessageText, b)
			cancel()
		}
		ws.Close(websocket.StatusNormalClosure, "")
		return
	}
	s.tunnels[t] = struct{}{}
	s.status.up(t, now)
	s.m.tunnels.Add(1)
	s.mu.Unlock()
	defer s.detach(t)
	for _, d := range displaced {
		d.kick(supersede(d, t))
	}
	log := s.log.With("handle", t.Handle, "gen", t.Gen, "daemon", netOf(t.daemon))

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	b, err := wire.Welcome{Hostnames: t.Hostnames, Gen: t.Gen, Keepalive: keepalive, MaxStreams: s.cfg.Limits.StreamsPerTunnel}.Marshal()
	if err != nil {
		ws.CloseNow()
		return
	}
	wctx, wcancel := context.WithTimeout(ctx, handshakeTimeout)
	err = ws.Write(wctx, websocket.MessageText, b)
	wcancel()
	if err != nil {
		ws.CloseNow()
		return
	}
	t.heard.Store(now.UnixNano())
	nc := heardConn{Conn: websocket.NetConn(ctx, ws, websocket.MessageBinary), t: t, now: s.now}
	sess, err := yamux.Client(nc, s.yamuxConfig())
	if err != nil {
		ws.CloseNow()
		return
	}
	defer sess.Close()
	ctrl, err := sess.OpenStream()
	if err != nil {
		return
	}
	t.sess = sess
	close(t.ready)
	s.m.hellos.inc("welcome")
	log.Info("relay: tunnel up", "hostnames", t.Hostnames)
	// A deny list that arrived during the handshake is enforced here.
	if why, denied := s.deny.denies(t.Handle, t.Key); denied {
		t.kick(wire.Control{T: wire.ControlNotice, Message: deniedMessage(why)})
	}
	ping := time.NewTicker(s.keepalive)
	defer ping.Stop()
	for {
		select {
		case <-sess.CloseChan():
			log.Info("relay: tunnel down")
			return
		case <-ctx.Done():
			return
		case <-ping.C:
			if _, err := sess.Ping(); err != nil {
				if !sess.IsClosed() {
					// Asleep, or off the network: nothing will answer a
					// close either.
					t.lost = true
					log.Info("relay: tunnel down", "reason", "no answer to a keepalive")
					ws.CloseNow()
				}
				return
			}
		case c := <-t.end:
			ctrl.SetWriteDeadline(time.Now().Add(controlTimeout))
			if err := wire.WriteControl(ctrl, c); err != nil {
				// Not err itself: a wrapped socket error names the
				// daemon's full address.
				var ne net.Error
				log.Debug("relay: control write failed", "timeout", errors.As(err, &ne) && ne.Timeout())
			}
			if c.T == wire.ControlDrain {
				select {
				case <-t.drained:
				default:
					close(t.drained)
				}
				continue // stay up for the streams in flight
			}
			log.Info("relay: tunnel ended", "reason", c.T, "gen_now", c.Gen)
			return
		}
	}
}

// detach removes an ended tunnel and records when its daemon was last
// seen: now, unless the daemon went quiet (a Mac asleep, a dropped
// network), when it is the last time the relay heard from it rather than
// the keepalive later that the relay noticed.
func (s *Server) detach(t *Tunnel) {
	seen := s.now()
	if h := t.heard.Load(); t.lost && h != 0 && h < seen.UnixNano() {
		seen = time.Unix(0, h)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reg.Detach(t)
	s.status.down(t, seen)
	delete(s.tunnels, t)
	s.m.tunnels.Add(-1)
}

// expire ends every tunnel whose entitlement has lapsed. Its daemon
// redials with the entitlement it holds now: a refreshed one is welcomed
// again, and a lapsed one gets entitlement_expired.
func (s *Server) expire(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t := range s.tunnels {
		if !t.exp.IsZero() && !now.Before(t.exp) {
			t.kick(wire.Control{T: wire.ControlNotice, Message: "The entitlement this tunnel was opened with has expired; reconnecting with the current one."})
		}
	}
}

// heardConn notes when the relay last read anything from a tunnel's
// daemon, keepalive answers included.
type heardConn struct {
	net.Conn
	t   *Tunnel
	now func() time.Time
}

func (c heardConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.t.heard.Store(c.now().UnixNano())
	}
	return n, err
}

// yamuxConfig is the relay's side of every session. The relay is the
// yamux client: it opens one stream per client connection, at most
// streams_per_tunnel at once, and AcceptBacklog lets that many SYNs be in
// flight, so parallel connections open in parallel. The relay never
// accepts a stream. A daemon that opens some parks at most that many in
// yamux's queue, each holding at most its 256 KiB window, which is what
// its own streams could hold anyway; yamux resets the rest.
//
// runTunnel sends the keepalives itself, so it knows when a daemon went
// quiet. yamux's own log is dropped: its errors carry socket addresses,
// and the relay logs addresses only as /24 or /48.
func (s *Server) yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.AcceptBacklog = s.cfg.Limits.StreamsPerTunnel
	c.EnableKeepAlive = false
	c.KeepAliveInterval = s.keepalive
	c.ConnectionWriteTimeout = min(pingTimeout, s.keepalive)
	c.StreamCloseTimeout = 10 * time.Second
	c.LogOutput = io.Discard
	return c
}
