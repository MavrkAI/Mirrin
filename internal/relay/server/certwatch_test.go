package server

import (
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

// A self-hoster whose ACME issuance fails (DNS not pointed yet, CAA, port
// 443 unreachable, a rate limit) sees it in the relay's log: one line at
// once, at most one a minute after that, and one when a certificate is
// served again. No client address is logged, and tenant names never reach
// the source or the log.
func TestControlCertificateTroubleLogged(t *testing.T) {
	clk := &clock{}
	var logs logBuffer
	pki := newPKI(t)
	cert := pki.leaf(t, controlName)
	var failing atomic.Bool
	failing.Store(true)
	source := func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if h.ServerName != controlName {
			t.Errorf("certificate source asked for %q", h.ServerName)
		}
		if failing.Load() {
			return nil, errors.New("acme: urn:ietf:params:acme:error:rateLimited: too many certificates")
		}
		return &cert, nil
	}
	d := newDevice(t)
	s, err := New(selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()}),
		Options{Now: clk.now, Log: logs.logger(), Certificates: source})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	handshake := func() error {
		c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{ServerName: controlName, RootCAs: pki.pool})
		if err == nil {
			c.Close()
		}
		return err
	}
	const failed = "relay: control certificate"

	for range 3 {
		if handshake() == nil {
			t.Fatal("handshake without a certificate")
		}
	}
	if n := logs.lines(failed); n != 1 {
		t.Fatalf("%d lines for 3 failures in a minute:\n%s", n, logs.String())
	}
	clk.advance(certLogEvery)
	handshake()
	if n := logs.lines(failed); n != 2 || !strings.Contains(logs.String(), `"failures":3`) {
		t.Fatalf("a minute later:\n%s", logs.String())
	}
	if s.getCertificate(&tls.ClientHelloInfo{ServerName: "h.test"}); logs.lines(failed) != 2 {
		t.Fatal("a tenant name was logged as the control certificate")
	}

	failing.Store(false)
	if err := handshake(); err != nil {
		t.Fatal(err)
	}
	handshake()
	if n := logs.lines("relay: control certificate served again"); n != 1 {
		t.Fatalf("%d recovery lines:\n%s", n, logs.String())
	}
	if s.m.certErrors.Load() != 4 || s.m.certExpiry.Load() != cert.Leaf.NotAfter.Unix() {
		t.Fatalf("metrics: %d errors, expiry %d", s.m.certErrors.Load(), s.m.certExpiry.Load())
	}
	if out := logs.String(); strings.Contains(out, "127.0.0.1") || !strings.Contains(out, "rateLimited") {
		t.Fatalf("log:\n%s", out)
	}
}
