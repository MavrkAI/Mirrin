package server

import (
	"bytes"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The example config in the repository loads and passes the production
// checks.
func TestExampleConfig(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", "cloud.example.yaml"), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(false); err != nil {
		t.Fatal(err)
	}
	if c.Keys.Entitlement != "ent-2026a" || c.Billing.Paddle.Prices["cloud"] == "" || c.DNS.Route53.HostedZoneID == "" || len(c.Relays) != 2 {
		t.Errorf("%+v", c)
	}
}

func TestConfigRefusals(t *testing.T) {
	for name, yaml := range map[string]string{
		"unknown field":     "public_url: https://cloud.example\nemail_everyone: true\n",
		"two documents":     "public_url: https://cloud.example\n---\nlisten: x\n",
		"http origin":       "public_url: http://cloud.example\n",
		"origin with path":  "public_url: https://cloud.example/api\n",
		"bad relay url":     "public_url: https://cloud.example\nrelays: [{id: r1, url: https://r1, ips: [192.0.2.1]}]\n",
		"relay without ips": "public_url: https://cloud.example\nrelays: [{id: r1, url: wss://r1/v1/tunnel}]\n",
		"bad relay ip":      "public_url: https://cloud.example\nrelays: [{id: r1, url: wss://r1/v1/tunnel, ips: [fe80::1%en0]}]\n",
		"bad zone":          "public_url: https://cloud.example\ntenant_zone: Mirrin.link\n",
		"cert without key":  "public_url: https://cloud.example\ncert_file: /x\n",
	} {
		p := filepath.Join(t.TempDir(), "cloud.yaml")
		os.WriteFile(p, []byte(yaml), 0o600)
		c, err := LoadConfig(p, DevConfig())
		if err == nil {
			err = c.Validate(false)
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// net/http's own error lines reach the log without the client's address.
func TestAddrScrubber(t *testing.T) {
	var buf bytes.Buffer
	a := addrScrubber{slog.New(slog.NewTextHandler(&buf, nil))}
	a.Write([]byte("http: TLS handshake error from 203.0.113.9:51234: EOF"))
	a.Write([]byte("http: TLS handshake error from [2001:db8::7]:443: EOF"))
	a.Write([]byte("http: panic serving 198.51.100.4:1: boom"))
	for _, ip := range []string{"203.0.113.9", "2001:db8::7", "198.51.100.4"} {
		if strings.Contains(buf.String(), ip) {
			t.Errorf("%s logged: %s", ip, buf.String())
		}
	}
	if !strings.Contains(buf.String(), "TLS handshake error from <addr>") {
		t.Errorf("the message was lost: %s", buf.String())
	}
}

// A limiter is a bucket per key that refills, and forgets full buckets.
func TestLimiter(t *testing.T) {
	l := newLimiter[string](2)
	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	if !l.allow("a", now) || !l.allow("a", now) || l.allow("a", now) {
		t.Error("a bucket of two gave three")
	}
	if !l.allow("b", now) {
		t.Error("another key shares the bucket")
	}
	if !l.allow("a", now.Add(31*time.Second)) {
		t.Error("the bucket did not refill")
	}
	l.allow("c", now.Add(10*time.Minute))
	l.mu.Lock()
	n := len(l.b)
	l.mu.Unlock()
	if n != 1 {
		t.Errorf("%d buckets kept after ten quiet minutes", n)
	}
	if !newLimiter[string](0).allow("x", now) {
		t.Error("a zero limit limited")
	}
	if got := newLimiter[string](30).retryAfter(); got != 2 {
		t.Errorf("retryAfter for 30 a minute: %d", got)
	}
}

// Clients are IPv4 addresses and IPv6 /48s, and a trusted proxy's clients
// are read from X-Forwarded-For, right to left, and nobody else's are.
func TestClientAddr(t *testing.T) {
	for a, want := range map[string]string{
		"192.0.2.7":          "192.0.2.7/32",
		"::ffff:192.0.2.7":   "192.0.2.7/32",
		"2001:db8:1:2:3::4":  "2001:db8:1::/48",
		"2001:db8:1:ffff::1": "2001:db8:1::/48",
		"fe80::1%en0":        "fe80::/48",
		"2001:db8:2::1":      "2001:db8:2::/48",
	} {
		if got := clientPrefix(netip.MustParseAddr(a)); got.String() != want {
			t.Errorf("clientPrefix(%s) = %s, want %s", a, got, want)
		}
	}
	if clientPrefix(netip.Addr{}) != (netip.Prefix{}) {
		t.Error("an invalid address is not one prefix")
	}

	cfg := DevConfig()
	cfg.TrustedProxies = []string{"127.0.0.1", "10.0.0.0/8", "::1"}
	trusted, err := cfg.trustedProxies()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{trusted: trusted}
	for _, c := range []struct{ peer, xff, want string }{
		{"203.0.113.9:4000", "", "203.0.113.9"},
		{"203.0.113.9:4000", "198.51.100.1", "203.0.113.9"}, // not a proxy: its header is ignored
		{"127.0.0.1:4000", "", "127.0.0.1"},
		{"127.0.0.1:4000", "198.51.100.1", "198.51.100.1"},
		{"127.0.0.1:4000", "6.6.6.6, 198.51.100.1", "198.51.100.1"},  // a client's own entry is further left
		{"127.0.0.1:4000", "198.51.100.1, 10.1.2.3", "198.51.100.1"}, // proxies in a chain are skipped
		{"[::1]:4000", "2001:db8::5", "2001:db8::5"},
		{"127.0.0.1:4000", "[2001:db8::5]:443", "2001:db8::5"},
		{"127.0.0.1:4000", "::ffff:198.51.100.1", "198.51.100.1"},
		{"127.0.0.1:4000", "nonsense", "127.0.0.1"},
		{"not an address", "198.51.100.1", "invalid IP"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := s.clientAddr(r).String(); got != c.want {
			t.Errorf("peer %s, X-Forwarded-For %q: %s, want %s", c.peer, c.xff, got, c.want)
		}
	}
}

// Production serving plain HTTP must name its proxy, and --dev listens on
// loopback only.
func TestConfigProxyAndDevListen(t *testing.T) {
	c := DefaultConfig()
	c.PublicURL = "https://cloud.example"
	c.Relays = DevConfig().Relays
	if err := c.Validate(false); err == nil || !strings.Contains(err.Error(), "trusted_proxies") {
		t.Errorf("plain HTTP with no trusted proxy: %v", err)
	}
	c.TrustedProxies = []string{"127.0.0.1", "::1/128"}
	if err := c.Validate(false); err != nil {
		t.Errorf("with a trusted proxy: %v", err)
	}
	c.TrustedProxies = []string{"proxy.internal"}
	if err := c.Validate(false); err == nil {
		t.Error("a trusted proxy that is not an address")
	}
	c.TrustedProxies, c.CertFile, c.KeyFile = nil, "/c.pem", "/k.pem"
	if err := c.Validate(false); err != nil {
		t.Errorf("ending TLS here needs no proxy: %v", err)
	}
	for _, l := range []string{"0.0.0.0:8787", ":8787", "[::]:8787", "192.0.2.1:8787", "localhost:8787"} {
		d := DevConfig()
		d.Listen = l
		if err := d.Validate(true); err == nil {
			t.Errorf("--dev on %s", l)
		}
	}
	for _, l := range []string{"127.0.0.1:0", "[::1]:8787", "127.0.0.2:1"} {
		if err := CheckDevListen(l); err != nil {
			t.Errorf("--dev on %s: %v", l, err)
		}
	}
}

// A server whose listener fails returns instead of hanging on its sweeper.
func TestServeReturnsOnFailure(t *testing.T) {
	e := newEnv(t, false, func(c *Config) { c.CertFile, c.KeyFile = "/nonexistent/cert.pem", "/nonexistent/key.pem" })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.s.Serve(t.Context(), ln) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("serving with missing certificate files succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve hung after its listener failed")
	}
}
