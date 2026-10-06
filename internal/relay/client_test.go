package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

var relayAddr = netip.MustParseAddrPort("198.51.100.7:443")

// clientAddr is the made-up browser address for stream i: IPv4 and IPv6
// alternately.
func clientAddr(i int) netip.AddrPort {
	if i%2 == 0 {
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte{203, 0, 113, byte(i%250 + 1)}), uint16(20000+i))
	}
	a := netip.MustParseAddr("2001:db8::").As16()
	binary.BigEndian.PutUint16(a[14:], uint16(i))
	return netip.AddrPortFrom(netip.AddrFrom16(a), uint16(20000+i))
}

func header(i int, relay string) wire.ProxyHeader {
	return wire.ProxyHeader{Source: clientAddr(i), Dest: relayAddr, Authority: "h.test", RelayID: relay}
}

// payload is stream i's bytes: an 8-byte index and length, then 1 to
// ~60 KiB of pseudo-random data.
func payload(i int) []byte {
	n := 1 + (i*7919)%(60<<10)
	b := make([]byte, 8+n)
	binary.BigEndian.PutUint32(b, uint32(i))
	binary.BigEndian.PutUint32(b[4:], uint32(n))
	x := uint32(i)*2654435761 + 1
	for j := 8; j < len(b); j++ {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[j] = byte(x)
	}
	return b
}

// Acceptance: a fake relay opens 200 concurrent streams with PROXY v2
// headers; Accept returns all of them with RemoteAddr equal to the injected
// client and payloads intact.
func TestAccept200ConcurrentStreams(t *testing.T) {
	const n = 200
	f := newFakeRelay(t, "r1", welcome())
	l := listen(t, config(f))
	s := f.session(t)

	type accepted struct {
		i             int
		remote, local net.Addr
		relay, name   string
		sent          []byte
		err           error
	}
	got := make(chan accepted, n)
	barrier := make(chan struct{}) // no echo until every stream is open at once
	var arrived atomic.Int32
	go func() {
		for range n {
			c, err := l.Accept()
			if err != nil {
				got <- accepted{err: err}
				return
			}
			go func() {
				defer c.Close()
				var hdr [8]byte
				a := accepted{remote: c.RemoteAddr(), local: c.LocalAddr(), relay: c.(*Conn).Relay(), name: c.(*Conn).ServerName()}
				if _, a.err = io.ReadFull(c, hdr[:]); a.err == nil {
					a.i = int(binary.BigEndian.Uint32(hdr[:]))
					a.sent = make([]byte, 8+binary.BigEndian.Uint32(hdr[4:]))
					copy(a.sent, hdr[:])
					_, a.err = io.ReadFull(c, a.sent[8:])
				}
				if arrived.Add(1) == n {
					close(barrier)
				}
				<-barrier
				if a.err == nil {
					_, a.err = c.Write(a.sent)
				}
				got <- a
			}()
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := s.OpenStream()
			if err != nil {
				errs <- err
				return
			}
			defer st.Close()
			st.SetDeadline(time.Now().Add(30 * time.Second))
			if err := wire.WriteProxyV2(st, header(i, "r1")); err != nil {
				errs <- err
				return
			}
			p := payload(i)
			if _, err := st.Write(p); err != nil {
				errs <- fmt.Errorf("stream %d write: %w", i, err)
				return
			}
			echo := make([]byte, len(p))
			if _, err := io.ReadFull(st, echo); err != nil {
				errs <- fmt.Errorf("stream %d echo: %w", i, err)
				return
			}
			if !bytes.Equal(echo, p) {
				errs <- fmt.Errorf("stream %d: echo differs", i)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	seen := make([]bool, n)
	for range n {
		var a accepted
		select {
		case a = <-got:
		case <-time.After(10 * time.Second):
			t.Fatal("not every stream was accepted")
		}
		if a.err != nil {
			t.Fatal(a.err)
		}
		if a.i < 0 || a.i >= n || seen[a.i] {
			t.Fatalf("stream index %d twice or out of range", a.i)
		}
		seen[a.i] = true
		if want := clientAddr(a.i); a.remote.(*net.TCPAddr).AddrPort() != want {
			t.Errorf("stream %d: RemoteAddr %v, want %v", a.i, a.remote, want)
		}
		if a.local.(*net.TCPAddr).AddrPort() != relayAddr || a.relay != "r1" || a.name != "h.test" {
			t.Errorf("stream %d: LocalAddr %v, relay %q, name %q", a.i, a.local, a.relay, a.name)
		}
		if !bytes.Equal(a.sent, payload(a.i)) {
			t.Errorf("stream %d: payload differs", a.i)
		}
	}
}

// roundTrip sends one connection through a relay and checks it arrives on
// the listener from that relay.
func roundTrip(t *testing.T, l *Listener, s *fakeSession, relay string, i int) {
	t.Helper()
	st := s.open(t, header(i, relay))
	defer st.Close()
	st.Write([]byte("ping"))
	c, err := accept(l, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("read %q, %v", buf, err)
	}
	if got := c.(*Conn).Relay(); got != relay {
		t.Fatalf("connection came from %q, want %q", got, relay)
	}
}

func accept(l *Listener, d time.Duration) (net.Conn, error) {
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := l.Accept()
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(d):
		return nil, errors.New("accept timed out")
	}
}

// Acceptance: two fake relays feed one Listener; killing one socket
// reconnects within the backoff window, and Status goes false then true.
func TestTwoRelaysReconnect(t *testing.T) {
	f1, f2 := newFakeRelay(t, "r1", welcome()), newFakeRelay(t, "r2", welcome())
	cfg := config(f1, f2)
	cfg.backoff = backoff{} // production timing: the first retry waits under 1 s
	changes := make(chan TunnelStatus, 16)
	cfg.observe = func(s TunnelStatus) {
		if s.Relay == "r2" {
			changes <- s
		}
	}
	l := listen(t, cfg)
	s1, s2 := f1.session(t), f2.session(t)
	waitFor(t, 5*time.Second, "both tunnels online", func() bool {
		st := l.Status()
		return st[0].Online && st[1].Online
	})
	if c := <-changes; !c.Online {
		t.Fatal("first r2 change was not online")
	}
	roundTrip(t, l, s1, "r1", 1)
	roundTrip(t, l, s2, "r2", 2)

	killed := time.Now()
	f2.kill()
	for _, want := range []bool{false, true} {
		select {
		case c := <-changes:
			if c.Online != want {
				t.Fatalf("r2 went Online=%v, want %v", c.Online, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("r2 never went Online=%v", want)
		}
	}
	// The session had just started, so the retry drew from [0, 1 s); the
	// rest is handshake time.
	if d := time.Since(killed); d > time.Second+2*time.Second {
		t.Fatalf("reconnect took %v", d)
	}
	st := l.Status()
	if !st[0].Online || !st[1].Online || st[1].LastError != "" {
		t.Fatalf("status after reconnect: %+v", st)
	}
	roundTrip(t, l, f2.session(t), "r2", 3)
	roundTrip(t, l, s1, "r1", 4) // r1 was never disturbed
}

func TestBackoffFullJitter(t *testing.T) {
	b := backoff{}
	b.defaults()
	for i, ceil := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60} {
		ceil *= time.Second
		for range 50 {
			bb := b
			if d := bb.next(); d < 0 || d >= ceil {
				t.Fatalf("attempt %d: %v outside [0, %v)", i, d, ceil)
			}
		}
		b.next()
	}
}

// A dual-homed daemon caps the wait, so a relay that comes back is used
// within seconds however long it was down.
func TestMaxBackoffCapsTheWait(t *testing.T) {
	c := ClientConfig{MaxBackoff: 4 * time.Second}
	c.defaults()
	for i := range 20 {
		if d := c.backoff.next(); d < 0 || d >= 4*time.Second {
			t.Fatalf("attempt %d: %v outside [0, 4s)", i, d)
		}
	}
	d := ClientConfig{}
	d.defaults()
	if d.backoff.max != time.Minute {
		t.Fatalf("default ceiling %v", d.backoff.max)
	}
}

func TestHelloContents(t *testing.T) {
	f := newFakeRelay(t, "r1", welcome())
	cfg := config(f)
	cfg.StatusKey = bytes.Repeat([]byte{7}, 32)
	l := listen(t, cfg)
	f.session(t)
	h := f.seen()[0]
	sum := sha256.Sum256(cfg.StatusKey)
	switch {
	case !h.verified:
		t.Error("the relay could not verify the hello against its own exporter")
	case !h.Key.Equal(testKey.Public()):
		t.Error("hello key is not the device key")
	case h.Ent != "":
		t.Errorf("ent = %q, want null", h.Ent)
	case !bytes.Equal(h.StatusKeyHash, sum[:]):
		t.Error("status_key_hash is not SHA-256 of the status key")
	case h.Client != "mirrin/dev "+runtime.GOOS+"/"+runtime.GOARCH:
		t.Errorf("client = %q", h.Client)
	}
	waitFor(t, 5*time.Second, "online", func() bool { return l.Status()[0].Online })
	st := l.Status()[0]
	if st.Gen != 3 || st.MaxStreams != 256 || st.Hostnames[0] != "h.test" {
		t.Fatalf("status: %+v", st)
	}
}

// superseded_retry calls OnRefused, which refreshes the entitlement, and
// the next hello carries the fresh token.
func TestSupersededRetryRefreshes(t *testing.T) {
	f := newFakeRelay(t, "r1", welcome())
	f.refuse(wire.Error{Code: wire.CodeSupersededRetry, Message: "A newer token holds this name."})
	var ent atomic.Value
	ent.Store("v4.public.old")
	refused := make(chan wire.Error, 4)
	cfg := config(f)
	cfg.Entitlement = func() string { return ent.Load().(string) }
	cfg.OnRefused = func(relay string, e wire.Error) {
		if relay != "r1" {
			t.Errorf("OnRefused relay %q", relay)
		}
		ent.Store("v4.public.new")
		refused <- e
	}
	l := listen(t, cfg)
	f.session(t)
	if e := <-refused; e.Code != wire.CodeSupersededRetry || e.Message != "A newer token holds this name." {
		t.Fatalf("refusal %+v", e)
	}
	h := f.seen()
	if len(h) != 2 || h[0].Ent != "v4.public.old" || h[1].Ent != "v4.public.new" {
		t.Fatalf("hellos carried %+v", h)
	}
	waitFor(t, 5*time.Second, "online", func() bool { return l.Status()[0].Online })
	if st := l.Status()[0]; st.Refused != nil || st.Stopped {
		t.Fatalf("status after retry: %+v", st)
	}
}

// superseded at hello time stops the tunnel: no reconnect, Stopped set,
// and with every relay stopped the listener is done.
func TestSupersededStops(t *testing.T) {
	f := newFakeRelay(t, "r1", welcome())
	f.refuse(wire.Error{Code: wire.CodeSuperseded, Message: "Mirrin moved to another machine."})
	refused := make(chan wire.Error, 4)
	cfg := config(f)
	cfg.OnRefused = func(_ string, e wire.Error) { refused <- e }
	l := listen(t, cfg)
	if e := <-refused; e.Code != wire.CodeSuperseded {
		t.Fatalf("refusal %+v", e)
	}
	select {
	case <-l.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("listener not done after its only relay superseded it")
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after supersede: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // longer than the test backoff ceiling
	if n := len(f.seen()); n != 1 {
		t.Fatalf("%d hellos after superseded", n)
	}
	st := l.Status()[0]
	if !st.Stopped || st.Online || st.Refused == nil || st.Refused.Code != wire.CodeSuperseded {
		t.Fatalf("status: %+v", st)
	}
}

// A control-stream superseded naming a higher generation stops the
// tunnel; the same generation means refresh and reconnect.
func TestControlSuperseded(t *testing.T) {
	for _, c := range []struct {
		gen  int64
		code string
	}{{4, wire.CodeSuperseded}, {3, wire.CodeSupersededRetry}} {
		t.Run(c.code, func(t *testing.T) {
			f := newFakeRelay(t, "r1", welcome())
			refused := make(chan wire.Error, 4)
			controls := make(chan wire.Control, 4)
			cfg := config(f)
			cfg.OnRefused = func(_ string, e wire.Error) { refused <- e }
			cfg.OnControl = func(_ string, c wire.Control) { controls <- c }
			l := listen(t, cfg)
			s := f.session(t)
			waitFor(t, 5*time.Second, "online", func() bool { return l.Status()[0].Online })
			s.control(t, wire.Control{T: wire.ControlSuperseded, Gen: c.gen, Message: "Moved."})
			if got := <-controls; got.T != wire.ControlSuperseded || got.Gen != c.gen {
				t.Fatalf("OnControl got %+v", got)
			}
			if e := <-refused; e.Code != c.code || e.Message != "Moved." {
				t.Fatalf("OnRefused got %+v", e)
			}
			if c.code == wire.CodeSuperseded {
				<-l.Done()
				if st := l.Status()[0]; !st.Stopped || st.Online {
					t.Fatalf("status: %+v", st)
				}
				return
			}
			f.session(t) // reconnected
			waitFor(t, 5*time.Second, "online again", func() bool { return l.Status()[0].Online })
		})
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	f := newFakeRelay(t, "r1", welcome())
	f.refuse(wire.Error{Code: wire.CodeRateLimited, Message: "Slow down.", RetryAfter: 1})
	listen(t, config(f))
	f.session(t)
	h := f.seen()
	if len(h) != 2 {
		t.Fatalf("%d hellos", len(h))
	}
	if gap := h[1].at.Sub(h[0].at); gap < time.Second {
		t.Fatalf("reconnected after %v despite retry_after 1", gap)
	}
}

func TestControlMessages(t *testing.T) {
	f := newFakeRelay(t, "r1", welcome())
	controls := make(chan wire.Control, 8)
	cfg := config(f)
	cfg.OnControl = func(relay string, c wire.Control) {
		if relay != "r1" {
			t.Errorf("OnControl relay %q", relay)
		}
		controls <- c
	}
	l := listen(t, cfg)
	s := f.session(t)
	msgs := []wire.Control{
		{T: wire.ControlNotice, Message: "Maintenance at 02:00 UTC."},
		{T: wire.ControlLimits, MaxStreams: 32, BPS: 20e6},
		{T: "something_new"},
		{T: wire.ControlDrain, RetryAfter: 1},
	}
	for _, m := range msgs {
		s.control(t, m)
	}
	for _, want := range msgs {
		if got := <-controls; got != want {
			t.Fatalf("OnControl got %+v, want %+v", got, want)
		}
	}
	// OnControl runs before the tunnel acts on a message, so wait for the
	// status. The drain's retry_after is recorded before Draining is set.
	waitFor(t, 5*time.Second, "limits and drain in Status", func() bool {
		st := l.Status()[0]
		return st.MaxStreams == 32 && st.Draining
	})
	// The relay goes away after draining; the next hello waits retry_after.
	gone := time.Now()
	s.Close()
	f.session(t)
	if h := f.seen(); h[len(h)-1].at.Sub(gone) < time.Second {
		t.Fatal("reconnected before the drain's retry_after")
	}
}

// A malformed control line ends the session, which then reconnects.
func TestMalformedControlReconnects(t *testing.T) {
	f := newFakeRelay(t, "r1", welcome())
	l := listen(t, config(f))
	s := f.session(t)
	s.ctrl.Write([]byte("{\"t\":\"notice\",\"message\":\"\\u0007\"}\n"))
	f.session(t)
	waitFor(t, 5*time.Second, "online", func() bool { return l.Status()[0].Online })
}

// A relay whose challenge names another relay gets no hello: a hello
// signed for r1 must not be obtainable by r9.
func TestChallengeFromOtherRelay(t *testing.T) {
	f := newFakeRelay(t, "r1", welcome())
	f.claim = "r9"
	l := listen(t, config(f))
	waitFor(t, 5*time.Second, "an error", func() bool { return l.Status()[0].LastError != "" })
	if n := len(f.seen()); n != 0 {
		t.Fatalf("%d hellos sent to a relay claiming another id", n)
	}
	if e := l.Status()[0].LastError; !strings.Contains(e, `"r9"`) {
		t.Fatalf("LastError %q", e)
	}
}

// With no id configured, the tunnel takes the id the relay gives, and
// checks PROXY headers against it.
func TestRelayIDFromChallenge(t *testing.T) {
	f := newFakeRelay(t, "selfhost-1", welcome())
	cfg := config(f)
	cfg.Relays[0].ID = ""
	l := listen(t, cfg)
	s := f.session(t)
	waitFor(t, 5*time.Second, "online", func() bool { return l.Status()[0].Online })
	if id := l.Status()[0].Relay; id != "selfhost-1" {
		t.Fatalf("relay id %q", id)
	}
	roundTrip(t, l, s, "selfhost-1", 1)
}

// Streams that are not a relayed connection for this tunnel are closed
// and never reach Accept.
func TestBadStreamsDropped(t *testing.T) {
	f := newFakeRelay(t, "r1", welcome("h.test", "other.test"))
	l := listen(t, config(f))
	s := f.session(t)
	good := header(1, "r1")
	bad := map[string]func(st *yamux.Stream){
		"other relay id": func(st *yamux.Stream) { h := good; h.RelayID = "r2"; wire.WriteProxyV2(st, h) },
		"no relay id":    func(st *yamux.Stream) { h := good; h.RelayID = ""; wire.WriteProxyV2(st, h) },
		"unknown name":   func(st *yamux.Stream) { h := good; h.Authority = "evil.test"; wire.WriteProxyV2(st, h) },
		"no authority":   func(st *yamux.Stream) { h := good; h.Authority = ""; wire.WriteProxyV2(st, h) },
		"LOCAL": func(st *yamux.Stream) {
			wire.WriteProxyV2(st, wire.ProxyHeader{Local: true, Authority: "h.test", RelayID: "r1"})
		},
		"no header": func(st *yamux.Stream) {
			st.Write([]byte("\x16\x03\x01\x02\x00\x01\x00\x01\xfc\x03\x03\x00\x01\x02\x03\x04"))
		},
	}
	for name, write := range bad {
		st, err := s.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		write(st)
		st.SetReadDeadline(time.Now().Add(5 * time.Second))
		if b, err := io.ReadAll(st); err != nil || len(b) != 0 {
			t.Errorf("%s: stream not closed cleanly: %q %v", name, b, err)
		}
		st.Close()
	}
	// Names match case-insensitively, as SNI does.
	for i, name := range []string{"H.TEST", "other.test"} {
		h := header(i, "r1")
		h.Authority = name
		st := s.open(t, h)
		c, err := accept(l, 5*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.(*Conn).ServerName() != name {
			t.Fatalf("accepted %q, want %q", c.(*Conn).ServerName(), name)
		}
		c.Close()
		st.Close()
	}
}

func TestListenConfigErrors(t *testing.T) {
	good := func() ClientConfig {
		return ClientConfig{Relays: []RelayRef{{ID: "r1", URL: "wss://r1.relay.test/v1/tunnel"}}, Key: testKey, StatusKey: make([]byte, 32)}
	}
	// checkConfig, not Listen: a good config would start dialling.
	c := good()
	if err := checkConfig(&c); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*ClientConfig){
		"no relays":     func(c *ClientConfig) { c.Relays = nil },
		"plain ws":      func(c *ClientConfig) { c.Relays[0].URL = "ws://r1.relay.test/v1/tunnel" },
		"https":         func(c *ClientConfig) { c.Relays[0].URL = "https://r1.relay.test/v1/tunnel" },
		"userinfo":      func(c *ClientConfig) { c.Relays[0].URL = "wss://u:p@r1.relay.test/v1/tunnel" },
		"bad id":        func(c *ClientConfig) { c.Relays[0].ID = "R 1" },
		"duplicate url": func(c *ClientConfig) { c.Relays = append(c.Relays, RelayRef{ID: "r2", URL: c.Relays[0].URL}) },
		"duplicate id": func(c *ClientConfig) {
			c.Relays = append(c.Relays, RelayRef{ID: "r1", URL: "wss://r2.relay.test/v1/tunnel"})
		},
		"no key":           func(c *ClientConfig) { c.Key = nil },
		"short status key": func(c *ClientConfig) { c.StatusKey = make([]byte, 16) },
		"bad client":       func(c *ClientConfig) { c.Client = "mirrin\n" },
	} {
		c = good()
		mod(&c)
		if l, err := Listen(context.Background(), c); err == nil {
			l.Close()
			t.Errorf("%s: accepted", name)
		}
	}
}

// End to end: an HTTPS server on the listener completes TLS with its own
// certificate through the relay, and sees the browser's address.
func TestServeTLSOverRelay(t *testing.T) {
	f := newFakeRelay(t, "r1", welcome())
	l := listen(t, config(f))
	s := f.session(t)

	cert, pool := daemonCert(t, "h.test")
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s %s", r.RemoteAddr, r.TLS.ServerName)
		}),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	served := make(chan error, 1)
	go func() { served <- srv.ServeTLS(l, "", "") }()
	defer func() {
		srv.Close()
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()

	st := s.open(t, header(0, "r1"))
	tc := tls.Client(st, &tls.Config{ServerName: "h.test", RootCAs: pool, MinVersion: tls.VersionTLS13})
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(tc, "GET / HTTP/1.1\r\nHost: h.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if want := clientAddr(0).String() + " h.test"; string(body) != want {
		t.Fatalf("got %q, want %q", body, want)
	}
}

// daemonCert is a self-signed certificate standing in for the daemon's own.
func daemonCert(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, pool
}
