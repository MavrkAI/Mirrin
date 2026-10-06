package relay

import (
	"crypto/tls"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// These tests play a relay that misbehaves, and check the limits that keep
// it from costing the daemon more than a bounded amount.

func welcomeMax(n int) wire.Welcome {
	w := welcome()
	w.MaxStreams = n
	return w
}

// acceptAll feeds every accepted connection into a channel, so a test can
// wait for one with a timeout without losing it.
func acceptAll(l *Listener) <-chan net.Conn {
	ch := make(chan net.Conn, 64)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			ch <- c
		}
	}()
	return ch
}

// firstByte reads the one byte a test stream starts with.
func firstByte(t *testing.T, c net.Conn) int {
	t.Helper()
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		t.Fatal(err)
	}
	return int(b[0])
}

func noConn(t *testing.T, got <-chan net.Conn, d time.Duration, why string) {
	t.Helper()
	select {
	case c := <-got:
		t.Fatalf("accepted a connection from %v %s", c.RemoteAddr(), why)
	case <-time.After(d):
	}
}

func oneConn(t *testing.T, got <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case c := <-got:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection accepted")
		return nil
	}
}

// closedByDaemon waits for the daemon's FIN on st, then closes the relay's
// half as a relay does.
func closedByDaemon(t *testing.T, st *yamux.Stream) {
	t.Helper()
	st.SetReadDeadline(time.Now().Add(3 * time.Second))
	if b, err := io.ReadAll(st); err != nil || len(b) != 0 {
		t.Fatalf("stream %d not closed cleanly by the daemon: %q %v", st.StreamID(), b, err)
	}
	st.Close()
}

// A session holds at most max_streams data streams. The next one waits in
// yamux's backlog, neither dropped nor accepted, until an earlier stream is
// closed by both sides; then it is accepted intact.
func TestStreamLimit(t *testing.T) {
	for _, c := range []struct {
		name   string
		limits bool // lower the limit with a limits message, not the welcome
	}{{"welcome", false}, {"limits", true}} {
		t.Run(c.name, func(t *testing.T) {
			w := welcomeMax(3)
			if c.limits {
				w = welcomeMax(64)
			}
			f := newFakeRelay(t, "r1", w)
			l := listen(t, config(f))
			got := acceptAll(l)
			s := f.session(t)
			if c.limits {
				s.control(t, wire.Control{T: wire.ControlLimits, MaxStreams: 3})
				waitFor(t, 5*time.Second, "the limits message", func() bool { return l.Status()[0].MaxStreams == 3 })
			}

			sts := make([]*yamux.Stream, 4)
			for i := range sts {
				sts[i] = s.open(t, header(i, "r1"))
				sts[i].Write([]byte{byte(i)})
			}
			conns := map[int]net.Conn{}
			for range 3 {
				c := oneConn(t, got)
				conns[firstByte(t, c)] = c
			}
			if _, ok := conns[3]; ok {
				t.Fatal("the fourth stream overtook one of the first three")
			}
			noConn(t, got, 300*time.Millisecond, "past max_streams 3")

			// The daemon closing a stream is not enough: yamux holds it
			// until the relay closes its half too.
			conns[0].Close()
			noConn(t, got, 300*time.Millisecond, "while a closed stream still held its place")
			closedByDaemon(t, sts[0])

			c4 := oneConn(t, got)
			defer c4.Close()
			if n := firstByte(t, c4); n != 3 {
				t.Fatalf("accepted stream %d, want 3", n)
			}
			if _, err := c4.Write([]byte("ok")); err != nil {
				t.Fatal(err)
			}
			sts[3].SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 2)
			if _, err := io.ReadFull(sts[3], buf); err != nil || string(buf) != "ok" {
				t.Fatalf("the waiting stream did not survive: %q %v", buf, err)
			}
			for _, cn := range conns {
				cn.Close()
			}
		})
	}
}

// A stream that stalls inside its PROXY header is closed at the deadline,
// so stalled headers cannot hold a session's places for good.
func TestHeaderDeadline(t *testing.T) {
	const deadline = 200 * time.Millisecond
	f := newFakeRelay(t, "r1", welcomeMax(2))
	cfg := config(f)
	cfg.headerTimeout = deadline
	l := listen(t, cfg)
	got := acceptAll(l)
	s := f.session(t)

	opened := time.Now()
	var partial []*yamux.Stream
	for range 2 { // every place in the session
		st, err := s.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		st.Write([]byte("\r\n\r\n\x00")) // the first 5 bytes of a signature
		partial = append(partial, st)
	}
	good := s.open(t, header(7, "r1"))
	defer good.Close()

	for _, st := range partial {
		closedByDaemon(t, st)
		if d := time.Since(opened); d < deadline {
			t.Fatalf("a stalled header was dropped after %v, before its deadline", d)
		}
	}
	c := oneConn(t, got)
	defer c.Close()
	if a := c.RemoteAddr().(*net.TCPAddr).AddrPort(); a != clientAddr(7) {
		t.Fatalf("accepted %v, want the good stream", a)
	}
}

// A relay floods the daemon with streams that each carry a bad header and
// then data, and never closes them: the probe that once held ~250 MiB.
// However many it opens, the daemon's yamux session holds at most the
// control stream, max_streams taken streams and its backlog; yamux resets
// the rest. Once the relay stops, every stream is let go and the tunnel
// still carries connections.
func TestStreamFloodBounded(t *testing.T) {
	const (
		limit = 16
		flood = 600
		bound = 1 + limit + acceptBacklog
	)
	f := newFakeRelay(t, "r1", welcomeMax(limit))
	f.backlog = 4096 // a hostile relay's yamux does not wait for its peer's backlog
	cfg := config(f)
	cfg.closeTimeout = 50 * time.Millisecond // how long each dropped stream lingers
	sessions := make(chan *yamux.Session, 1)
	cfg.session = func(s *yamux.Session) { sessions <- s }
	l := listen(t, cfg)
	got := acceptAll(l)
	s := f.session(t)
	daemon := <-sessions

	var peak int
	var mu sync.Mutex
	sample := func() int {
		n := daemon.NumStreams()
		mu.Lock()
		peak = max(peak, n)
		mu.Unlock()
		return n
	}
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				sample()
			}
		}
	}()

	data := make([]byte, 16<<10)
	bad := header(0, "r9")
	for range flood {
		st, err := s.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		// Streams past the backlog are reset under the writer; that is
		// the point, so errors here are expected.
		wire.WriteProxyV2(st, bad)
		st.Write(data)
	}
	// A ping is answered after every frame sent before it, so the daemon
	// has now seen every stream.
	if _, err := s.Ping(); err != nil {
		t.Fatal(err)
	}
	if n := sample(); n > bound {
		t.Fatalf("the daemon holds %d streams after the flood, want at most %d", n, bound)
	}
	waitFor(t, 20*time.Second, "every flooded stream let go", func() bool { return sample() == 1 })
	close(stop)
	<-sampled
	t.Logf("%d streams flooded; the daemon held at most %d at once (bound %d)", flood, peak, bound)
	if peak > bound {
		t.Fatalf("the daemon held %d streams at once, want at most %d", peak, bound)
	}
	if peak < bound/2 {
		t.Fatalf("peak %d: the flood never reached the daemon", peak)
	}
	if !l.Status()[0].Online {
		t.Fatal("the tunnel went down")
	}
	select {
	case c := <-got:
		t.Fatalf("a flooded stream from %v was accepted", c.RemoteAddr())
	default:
	}
	st := s.open(t, header(1, "r1"))
	defer st.Close()
	st.Write([]byte{1})
	c := oneConn(t, got)
	defer c.Close()
	if n := firstByte(t, c); n != 1 {
		t.Fatalf("accepted stream %d after the flood, want 1", n)
	}
}

// After a session that stayed up for backoff.stable the ceiling starts
// over; after a shorter one it keeps doubling. Each wait is at least the
// ceiling the backoff drew from.
func TestBackoffResetsAfterStableSession(t *testing.T) {
	const ms = time.Millisecond
	f := newFakeRelay(t, "r1", welcome())
	for range 4 {
		f.refuse(wire.Error{Code: wire.CodeRateLimited, Message: "Slow down."})
	}
	var (
		mu    sync.Mutex
		ceils []time.Duration
	)
	cfg := config(f)
	cfg.backoff = backoff{min: 40 * ms, max: 10 * time.Second, stable: 400 * ms, jitter: func(c time.Duration) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		ceils = append(ceils, c)
		return c // the top of [0, c), so every wait is known
	}}
	l := listen(t, cfg)
	f.session(t)
	h := f.seen()
	for i, want := range []time.Duration{40 * ms, 80 * ms, 160 * ms, 320 * ms} {
		if gap := h[i+1].at.Sub(h[i].at); gap < want {
			t.Fatalf("attempt %d came %v after the last, under its %v wait", i+2, gap, want)
		}
	}

	waitFor(t, 5*time.Second, "online", func() bool { return l.Status()[0].Online })
	time.Sleep(500 * ms) // past stable
	killed := time.Now()
	f.kill()
	f.session(t)
	if gap := f.seen()[5].at.Sub(killed); gap < 40*ms || gap > 400*ms {
		t.Fatalf("redialled %v after a stable session, want the first ceiling, 40ms", gap)
	}

	killed = time.Now()
	f.kill() // a session of moments: the ceiling doubles
	f.session(t)
	if gap := f.seen()[6].at.Sub(killed); gap < 80*ms {
		t.Fatalf("redialled %v after a short session, under the 80ms ceiling", gap)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []time.Duration{40 * ms, 80 * ms, 160 * ms, 320 * ms, 40 * ms, 80 * ms}; !slices.Equal(ceils, want) {
		t.Fatalf("ceilings %v, want %v", ceils, want)
	}
}

// The exporter is trusted only when the dial made exactly one TLS
// connection.
func TestOnlyConn(t *testing.T) {
	a, b := new(tls.Conn), new(tls.Conn)
	if c, err := onlyConn([]*tls.Conn{a}); err != nil || c != a {
		t.Fatalf("one connection: %p, %v", c, err)
	}
	for _, cs := range [][]*tls.Conn{nil, {a, b}} {
		if _, err := onlyConn(cs); err == nil {
			t.Errorf("%d connections accepted", len(cs))
		}
	}
}
