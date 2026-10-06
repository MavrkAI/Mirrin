package tlsmgr_test

// Issuance end to end through an in-process mirrin-relay (WP-14): the CA's
// validator dials the relay, the relay routes the acme-tls/1 hello by SNI
// down the daemon's tunnel, and the daemon's listener answers with the
// challenge certificate from tlsmgr.ACME.
//
// By default the CA is acmetest, an in-process RFC 8555 CA that verifies
// JWS signatures and nonces and really dials the name for TLS-ALPN-01.
// Built with -tags pebble, the same test runs against Let's Encrypt's
// Pebble instead (CI starts it; see .github/workflows/ci.yml and
// acme_pebble_test.go).

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/relay"
	"github.com/MavrkAI/Mirrin/internal/relay/server"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr/acmetest"
)

// pebble, when set by acme_pebble_test.go, supplies the CA: its directory,
// a client that trusts its API, the address the relay must listen on (the
// port Pebble validates TLS-ALPN-01 on) and the hostname to request.
var pebble func(t *testing.T) (directory string, client *http.Client, relayAddr, host string)

type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.off) }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.off += d; c.mu.Unlock() }

func controlCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay control CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestIntegrationIssueThroughRelay(t *testing.T) {
	host := "twin.mirrin.test"
	relayAddr := "127.0.0.1:0"
	var directory string
	var client *http.Client
	if pebble != nil {
		directory, client, relayAddr, host = pebble(t)
	} else {
		ca := acmetest.New()
		defer ca.Close()
		directory, client = ca.Directory(), ca.Client()
		defer func() {
			if t.Failed() {
				t.Logf("validations: %+v", ca.Validations())
			}
		}()
		// DNS for the name points at the relay.
		ca.Dial = func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", relayAddr)
		}
	}

	// The relay: self-host mode, the daemon's device key allowed for host.
	pub, dev, _ := ed25519.GenerateKey(rand.Reader)
	cert, roots := controlCert(t)
	cfg := server.DefaultConfig()
	cfg.ID, cfg.ControlHostname, cfg.StateDir = "r1", "localhost", t.TempDir()
	cfg.HTTPListen, cfg.MetricsListen = "", ""
	cfg.Allow = []server.Allow{{Hostname: host, Key: base64.RawURLEncoding.EncodeToString(pub)}}
	rs, err := server.New(cfg, server.Options{Certificates: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	relayAddr = ln.Addr().String()
	go rs.Serve(ln)
	defer rs.Close()
	_, port, _ := net.SplitHostPort(relayAddr)

	// The daemon: its tunnel, and a TLS listener on it using tlsmgr.
	tun, err := relay.Listen(context.Background(), relay.ClientConfig{
		Relays: []relay.RelayRef{{URL: "wss://localhost:" + port + wire.Path}}, Key: dev,
		StatusKey: make([]byte, wire.StatusKeySize), RootCAs: roots,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { tun.Close(); <-tun.Done() }()
	clk := &clock{}
	online := func(ctx context.Context) error {
		for !tun.Status()[0].Online {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
		return nil
	}
	a, err := tlsmgr.NewACME(tlsmgr.ACMEConfig{Directory: directory, Hostnames: []string{host}, Dir: filepath.Join(t.TempDir(), "tls"), HTTPClient: client, KeyRotate: 30 * 24 * time.Hour, Now: clk.Now, Ready: online})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: 5 * time.Second, TLSConfig: tlsmgr.TLSConfig(a)}
	go srv.ServeTLS(tun, "", "")
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pins := a.SPKIs()

	// First issuance, through the relay.
	if _, err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	first := a.Leaf()
	if first == nil || tlsmgr.SPKIPin(first) != pins[0] {
		t.Fatal("first certificate isn't for the current key")
	}
	// A renewal keeps the SPKI.
	if err := a.Renew(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Leaf().Equal(first) || tlsmgr.SPKIPin(a.Leaf()) != pins[0] {
		t.Fatal("renewal didn't keep the key")
	}
	// After KeyRotate the pre-announced next key becomes current. Against
	// acmetest the loop decides that itself once the clock passes
	// KeyRotate. Pebble signs with its own, real clock, so skipping ahead
	// would leave the new certificate looking expired to the handshake
	// below: there the rotation is asked for directly.
	if pebble != nil {
		err = a.Rotate(ctx)
	} else {
		clk.Add(31 * 24 * time.Hour)
		_, err = a.Step(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	if tlsmgr.SPKIPin(a.Leaf()) != pins[1] || a.SPKIs()[0] != pins[1] || a.SPKIs()[1] == pins[1] {
		t.Fatalf("rotation: announced %v, now %v, serving %s", pins, a.SPKIs(), tlsmgr.SPKIPin(a.Leaf()))
	}
	// Browsers reaching the name through the relay get the new certificate.
	c, err := tls.Dial("tcp", relayAddr, &tls.Config{ServerName: host, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if tlsmgr.SPKIPin(c.ConnectionState().PeerCertificates[0]) != pins[1] {
		t.Fatal("the relay path serves another key")
	}
}
