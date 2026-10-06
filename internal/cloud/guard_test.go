package cloud_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/cloud"
)

// guard watches everything this test binary sends, from before the first
// test until the process exits, so no test here can reach past loopback. It
// is installed once, before any goroutine exists, because the daemon leaves
// goroutines behind that would race with swapping the globals back.
var guard = &egressGuard{}

func TestMain(m *testing.M) {
	guard.install()
	os.Exit(m.Run())
}

// egressGuard watches at two layers:
//
//   - HTTP: it replaces http.DefaultTransport, which the cloud client and
//     most other clients use. Hosts a test registers with serve (the fake
//     model, a fake control plane) are answered in memory, with no socket, so
//     they work inside a synctest bubble; other loopback requests pass;
//     everything else is refused.
//   - DNS: it replaces net.DefaultResolver with the Go resolver speaking to
//     an in-process server that answers every name with NXDOMAIN. That sees
//     the names dialled by clients with their own transport, such as the
//     relay tunnel.
//
// A request or lookup for a Cloud or relay host is a violation, noted with
// the time (a bubble's fake time inside one). Nothing leaves the machine
// either way.
type egressGuard struct {
	next       http.RoundTripper
	dnsWatched bool // this platform's lookups go through the guard

	mu         sync.Mutex
	extra      []string                // host or host:port counted as Cloud, beyond the zones
	local      map[string]http.Handler // host:port answered in memory
	violations []violation
	refused    []string
}

type violation struct {
	what string
	at   time.Time
}

var errRefusedByGuard = errors.New("egress guard: refused")

func (g *egressGuard) install() {
	g.next = http.DefaultTransport
	http.DefaultTransport = g
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: g.dialDNS}
	// Check that lookups here really go through the guard.
	const probe = "egress-guard-probe." + cloud.OperatorZone
	net.DefaultResolver.LookupHost(context.Background(), probe)
	g.dnsWatched = g.saw("DNS " + probe)
}

// watchEgress starts a fresh record for t. cloudHosts are counted as Cloud
// too, such as a fake control plane's host:port.
func watchEgress(t *testing.T, cloudHosts ...string) *egressGuard {
	t.Helper()
	g := guard
	g.mu.Lock()
	g.extra, g.violations, g.refused = cloudHosts, nil, nil
	g.mu.Unlock()
	t.Cleanup(func() {
		g.mu.Lock()
		g.extra = nil
		g.mu.Unlock()
	})
	if !g.dnsWatched {
		t.Log("egress guard: DNS lookups bypass the Go resolver on this platform; watching HTTP only")
	}
	return g
}

// serve answers requests for host (as a URL names it, with any port) with
// h, in memory, until t ends.
func (g *egressGuard) serve(t *testing.T, host string, h http.Handler) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.local == nil {
		g.local = map[string]http.Handler{}
	}
	g.local[strings.ToLower(host)] = h
	t.Cleanup(func() {
		g.mu.Lock()
		delete(g.local, strings.ToLower(host))
		g.mu.Unlock()
	})
}

// isCloud reports whether host (a hostname, host:port, or a DNS name with a
// search domain appended) is Mirrin Cloud's or a relay's. g.mu is held.
func (g *egressGuard) isCloud(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if slices.Contains(g.extra, host) {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	api, _ := url.Parse(cloud.DefaultAPI)
	for _, zone := range []string{cloud.OperatorZone, cloud.TenantZone, api.Hostname()} {
		if strings.Contains("."+host+".", "."+zone+".") {
			return true
		}
	}
	return slices.Contains(g.extra, host)
}

// RoundTrip implements http.RoundTripper.
func (g *egressGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	what := req.Method + " " + req.URL.Redacted()
	g.mu.Lock()
	isCloud := g.isCloud(req.URL.Host)
	h := g.local[strings.ToLower(req.URL.Host)]
	pass := h == nil && !isCloud && isLoopbackHost(req.URL.Hostname())
	switch {
	case isCloud:
		g.violations = append(g.violations, violation{"HTTP " + what, time.Now()})
	case h == nil && !pass:
		g.refused = append(g.refused, what)
	}
	g.mu.Unlock()
	switch {
	case h != nil:
		return serveInMemory(h, req)
	case pass:
		return g.next.RoundTrip(req)
	}
	if req.Body != nil {
		req.Body.Close()
	}
	return nil, errRefusedByGuard
}

// serveInMemory answers req with h as a server would see it, without a
// connection.
func serveInMemory(h http.Handler, req *http.Request) (*http.Response, error) {
	in := req.Clone(req.Context())
	in.URL = &url.URL{Path: req.URL.Path, RawPath: req.URL.RawPath, RawQuery: req.URL.RawQuery}
	in.RequestURI = req.URL.RequestURI()
	in.Host = req.URL.Host
	in.RemoteAddr = "127.0.0.1:1"
	if in.Body == nil {
		in.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, in)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func isLoopbackHost(h string) bool {
	ip := net.ParseIP(h)
	return h == "localhost" || ip != nil && ip.IsLoopback()
}

// check fails t for every violation, and logs what else was refused.
func (g *egressGuard) check(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, v := range g.violations {
		if i == 10 {
			t.Errorf("… and %d more", len(g.violations)-i)
			break
		}
		t.Errorf("sent to Mirrin Cloud or a relay at %s: %s", v.at.UTC().Format(time.DateTime), v.what)
	}
	if len(g.refused) > 0 {
		t.Logf("egress guard refused (not Cloud): %s", strings.Join(g.refused, "; "))
	}
}

// saw reports whether a violation containing s has been recorded.
func (g *egressGuard) saw(s string) bool { return len(g.sawAt(s)) > 0 }

// sawAt returns when each violation containing s happened, oldest first.
func (g *egressGuard) sawAt(s string) []time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	var at []time.Time
	for _, v := range g.violations {
		if strings.Contains(v.what, s) {
			at = append(at, v.at)
		}
	}
	return at
}

// dialDNS hands the resolver one end of a pipe to a fake DNS server.
func (g *egressGuard) dialDNS(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	go g.answerDNS(server)
	return client, nil
}

// answerDNS reads length-prefixed queries (the stream form, since a pipe is
// not a PacketConn), records Cloud names, and answers NXDOMAIN to all.
func (g *egressGuard) answerDNS(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	for {
		var n [2]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(n[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		name, end, ok := question(q)
		if !ok {
			return
		}
		g.mu.Lock()
		if g.isCloud(name) {
			g.violations = append(g.violations, violation{"DNS " + name, time.Now()})
		}
		g.mu.Unlock()
		resp := slices.Clone(q[:end])
		resp[2] = 0x80 | q[2]&0x79 // QR, and the query's opcode and RD
		resp[3] = 0x80 | 3         // RA, NXDOMAIN
		clear(resp[6:12])          // no answer, authority or additional records
		binary.BigEndian.PutUint16(resp[4:], 1)
		binary.BigEndian.PutUint16(n[:], uint16(len(resp)))
		if _, err := c.Write(append(n[:], resp...)); err != nil {
			return
		}
	}
}

// question reads the one question of a DNS query: its name, and where the
// question section ends.
func question(q []byte) (string, int, bool) {
	if len(q) < 12 || binary.BigEndian.Uint16(q[4:6]) != 1 {
		return "", 0, false
	}
	var labels []string
	i := 12
	for {
		if i >= len(q) {
			return "", 0, false
		}
		n := int(q[i])
		i++
		if n == 0 {
			break
		}
		if n&0xc0 != 0 || i+n > len(q) {
			return "", 0, false
		}
		labels = append(labels, string(q[i:i+n]))
		i += n
	}
	if i+4 > len(q) {
		return "", 0, false
	}
	return strings.Join(labels, "."), i + 4, true
}

// waitFor polls cond for up to d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
