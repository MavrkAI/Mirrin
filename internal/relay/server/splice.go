package server

import (
	"errors"
	"io"
	"math"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

const copySize = 32 << 10

var copyBufs = sync.Pool{New: func() any { b := make([]byte, copySize); return &b }}

var errTunnelGone = errors.New("relay: tunnel ended")

// splice carries one client connection through t's session: a PROXY v2
// header naming the client, the bytes the router peeked, then both
// directions as they come. The relay never looks inside them; it counts
// them, paces them and forwards them.
func (s *Server) splice(c net.Conn, t *Tunnel, name string, replay []byte) {
	defer c.Close()
	wait := time.NewTimer(streamReady)
	select {
	case <-t.ready:
		wait.Stop()
	case <-t.done:
		wait.Stop()
		s.m.conns.inc(connNoSession)
		return
	case <-wait.C:
		s.m.conns.inc(connNoSession)
		return
	}
	src, dst := addrPort(c.RemoteAddr()), addrPort(c.LocalAddr())
	now := s.now()
	if s.abuse.isSuspended(t.Handle, now) {
		s.m.conns.inc(connSuspended)
		return
	}
	if tripped, n := s.abuse.observe(t.Handle, src.Addr(), now); tripped {
		s.suspend(t.Handle, n, src.Addr())
		s.m.conns.inc(connSuspended)
		return
	}
	if !t.acquire() {
		s.m.conns.inc(connFull)
		return
	}
	defer t.release()
	st, err := t.sess.OpenStream()
	if err != nil {
		s.m.conns.inc(connNoSession)
		return
	}
	s.m.conns.inc(connSpliced)
	s.m.splicing.Add(1)
	defer s.m.splicing.Add(-1)
	start := time.Now()
	p := &pipe{s: s, t: t, c: c, st: st}
	p.run(wire.ProxyHeader{Source: src, Dest: dst, Authority: name, RelayID: s.cfg.ID}, replay)
	s.connLog.add(connRecord{
		At:       s.now().Unix(),
		Handle:   t.Handle,
		Client:   netOf(src.Addr()),
		ToDaemon: p.toDaemon,
		ToClient: p.toClient,
		Millis:   time.Since(start).Milliseconds(),
	})
}

// pipe is one spliced connection.
type pipe struct {
	s        *Server
	t        *Tunnel
	c        net.Conn
	st       *yamux.Stream
	last     atomic.Int64 // unix nanoseconds of the last byte either way
	timer    *time.Timer
	toDaemon int64 // written by run's goroutine
	toClient int64 // written by the other one
}

// run splices until both directions are done. When the daemon closes its
// half, the relay closes the client's socket and then its own half; when
// the client is done sending, the relay closes its half and waits for the
// daemon to finish. A connection idle both ways for limits.idle is cut.
func (p *pipe) run(hdr wire.ProxyHeader, replay []byte) {
	if err := wire.WriteProxyV2(p.st, hdr); err != nil {
		p.st.Close()
		return
	}
	p.touch()
	p.timer = time.AfterFunc(math.MaxInt64, p.checkIdle) // armed below, once p.timer is set
	p.timer.Reset(p.s.cfg.Limits.Idle)
	defer p.timer.Stop()
	down := make(chan struct{})
	go func() {
		defer close(down)
		p.copy(p.c, p.st, false)
		p.c.Close()
		p.st.Close()
	}()
	if p.send(p.st, replay, true) == nil {
		p.copy(p.st, p.c, true)
	}
	p.st.Close()
	<-down
}

func (p *pipe) copy(dst io.Writer, src io.Reader, toDaemon bool) {
	bp := copyBufs.Get().(*[]byte)
	defer copyBufs.Put(bp)
	for {
		n, err := src.Read(*bp)
		if n > 0 && p.send(dst, (*bp)[:n], toDaemon) != nil {
			return
		}
		if err != nil {
			return
		}
	}
}

// send paces b through the tunnel's bucket and writes it.
func (p *pipe) send(dst io.Writer, b []byte, toDaemon bool) error {
	if len(b) == 0 {
		return nil
	}
	p.touch()
	if tap := p.s.opts.tap; tap != nil {
		tap(p.c, toDaemon, b)
	}
	if !p.t.bucket.wait(len(b), p.t.done) {
		return errTunnelGone
	}
	if _, err := dst.Write(b); err != nil {
		return err
	}
	p.touch()
	n := int64(len(b))
	if toDaemon {
		p.toDaemon += n
		p.s.m.toDaemon.Add(n)
	} else {
		p.toClient += n
		p.s.m.toClient.Add(n)
	}
	return nil
}

func (p *pipe) touch() { p.last.Store(time.Now().UnixNano()) }

// checkIdle cuts the connection once nothing has moved for limits.idle,
// and otherwise looks again when it could next be idle.
func (p *pipe) checkIdle() {
	idle := p.s.cfg.Limits.Idle
	quiet := time.Since(time.Unix(0, p.last.Load()))
	if quiet < idle {
		p.timer.Reset(idle - quiet)
		return
	}
	p.c.Close()
	p.st.SetReadDeadline(time.Now())
	p.st.Close()
}

// suspend acts on the abuse heuristic: every tunnel for the handle ends,
// and new ones are refused until the suspension lapses.
func (s *Server) suspend(handle string, networks int, latest netip.Addr) {
	s.m.suspensions.Add(1)
	s.log.Warn("relay: abuse: handle suspended for review", "handle", handle, "networks", networks,
		"latest", netOf(latest), "for", s.cfg.Abuse.SuspendFor)
	s.mu.Lock()
	for t := range s.tunnels {
		if t.Handle == handle {
			t.kick(wire.Control{T: wire.ControlNotice, Message: "This twin's address is suspended while it is under review."})
		}
	}
	s.mu.Unlock()
	s.saveSuspensions()
	if f := s.opts.OnSuspend; f != nil {
		f(handle, networks)
	}
}

// addrPort is a TCP address as the PROXY header carries it.
func addrPort(a net.Addr) netip.AddrPort {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return netip.AddrPort{}
	}
	ap := ta.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port())
}
