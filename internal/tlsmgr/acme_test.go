package tlsmgr

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/MavrkAI/Mirrin/internal/tlsmgr/acmetest"
)

const testHost = "twin.mirrin.test"

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// acmeRig is a fake CA whose validator reaches a TLS listener serving the
// ACME source, as a browser would reach the daemon.
type acmeRig struct {
	ca    *acmetest.Server
	clock *fakeClock
	dir   string
	ln    net.Listener
	mu    sync.Mutex
	src   *ACME
}

func newRig(t *testing.T) *acmeRig {
	t.Helper()
	r := &acmeRig{ca: acmetest.New(), clock: &fakeClock{t: time.Now()}, dir: filepath.Join(t.TempDir(), "tls")}
	r.ca.Now = r.clock.Now
	t.Cleanup(r.ca.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.mu.Lock()
			src := r.src
			r.mu.Unlock()
			go func() {
				defer c.Close()
				tc := tls.Server(c, TLSConfig(src))
				tc.SetDeadline(time.Now().Add(5 * time.Second))
				_ = tc.Handshake()
			}()
		}
	}()
	r.ca.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	}
	return r
}

func (r *acmeRig) open(t *testing.T, mod ...func(*ACMEConfig)) *ACME {
	t.Helper()
	cfg := ACMEConfig{Directory: r.ca.Directory(), Hostnames: []string{testHost}, Dir: r.dir, HTTPClient: r.ca.Client(), Now: r.clock.Now}
	for _, m := range mod {
		m(&cfg)
	}
	a, err := NewACME(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.src = a
	r.mu.Unlock()
	return a
}

func TestACMEIssuesByTLSALPNAndReusesKey(t *testing.T) {
	r := newRig(t)
	a := r.open(t)
	pins := a.SPKIs()
	if len(pins) != 2 || pins[0] == pins[1] {
		t.Fatalf("pins %v", pins)
	}
	if _, err := a.GetCertificate(nil); err == nil {
		t.Fatal("served a certificate before issuance")
	}
	ctx := context.Background()
	if _, err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := a.GetCertificate(&tls.ClientHelloInfo{ServerName: testHost})
	if err != nil {
		t.Fatal(err)
	}
	if SPKIPin(c.Leaf) != pins[0] {
		t.Fatal("certificate isn't for the current key")
	}
	if _, err := c.Leaf.Verify(x509.VerifyOptions{DNSName: testHost, Roots: r.ca.Roots, CurrentTime: r.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if v := r.ca.Validations(); len(v) != 1 || v[0].Err != nil {
		t.Fatalf("validations %+v", v)
	}
	if a.AccountURI() == "" || !strings.HasPrefix(a.AccountURI(), r.ca.URL+"/acct/") {
		t.Fatalf("account %q", a.AccountURI())
	}
	for _, f := range []string{accountKeyFile, currentKeyFile, nextKeyFile, certFile, issuedFile} {
		fi, err := os.Stat(filepath.Join(r.dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v", f, fi.Mode())
		}
	}
	iss := a.Issued()
	if len(iss) != 1 || iss[0].SPKI != pins[0] || iss[0].Serial == "" || !iss[0].NotBefore.Equal(c.Leaf.NotBefore) {
		t.Fatalf("issued.json %+v", iss)
	}

	// A restart loads everything: same keys, same account, same certificate.
	b := r.open(t)
	if !slices.Equal(b.SPKIs(), pins) || b.AccountURI() != a.AccountURI() || b.Leaf() == nil || !b.Leaf().Equal(c.Leaf) {
		t.Fatal("state not reloaded")
	}
	st, err := ReadACMEState(r.dir)
	if err != nil || st.Current != pins[0] || st.Next != pins[1] || st.AccountURI != a.AccountURI() || st.Leaf == nil {
		t.Fatalf("ReadACMEState %+v %v", st, err)
	}

	// Renewal keeps the key.
	if err := b.Renew(ctx); err != nil {
		t.Fatal(err)
	}
	if SPKIPin(b.Leaf()) != pins[0] || b.Leaf().Equal(c.Leaf) {
		t.Fatal("renewal changed the key or didn't renew")
	}
}

func TestACMERotatePromotesAnnouncedNextKey(t *testing.T) {
	r := newRig(t)
	a := r.open(t)
	ctx := context.Background()
	if err := a.Renew(ctx); err != nil {
		t.Fatal(err)
	}
	before := a.SPKIs()
	if err := a.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	after := a.SPKIs()
	if SPKIPin(a.Leaf()) != before[1] || after[0] != before[1] || after[1] == before[0] || after[1] == before[1] {
		t.Fatalf("before %v after %v leaf %s", before, after, SPKIPin(a.Leaf()))
	}
	known, _ := a.Known()
	for _, p := range append(before, after...) {
		if !slices.Contains(known, p) {
			t.Fatalf("%s missing from history %v", p, known)
		}
	}
	// The files agree after a restart.
	b := r.open(t)
	if !slices.Equal(b.SPKIs(), after) {
		t.Fatal("rotation not saved")
	}
}

func TestACMEFinishesInterruptedRotation(t *testing.T) {
	r := newRig(t)
	a := r.open(t)
	if err := a.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	pins := a.SPKIs()
	// A certificate for the next key was written, then the process died
	// before the keys moved.
	next, err := readKey(filepath.Join(r.dir, nextKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	c := r.ca.Mint(next.Public(), testHost)
	os.WriteFile(filepath.Join(r.dir, certFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}), 0o600)
	b := r.open(t)
	got := b.SPKIs()
	if got[0] != pins[1] || got[1] == pins[0] || got[1] == pins[1] || SPKIPin(b.Leaf()) != pins[1] {
		t.Fatalf("pins %v → %v", pins, got)
	}
}

func TestACMESchedule(t *testing.T) {
	r := newRig(t)
	r.ca.Lifetime = 90 * 24 * time.Hour
	a := r.open(t, func(c *ACMEConfig) { c.KeyRotate = 200 * 24 * time.Hour })
	ctx := context.Background()
	if _, err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	first := a.Leaf()
	// Nothing is due for 60 days; Step says to wait, in slices no longer
	// than the ARI recheck.
	delay, err := a.Step(ctx)
	if err != nil || delay != ariRecheck || !a.Leaf().Equal(first) {
		t.Fatalf("delay %v err %v", delay, err)
	}
	r.clock.Add(58 * 24 * time.Hour)
	if _, err := a.Step(ctx); err != nil || !a.Leaf().Equal(first) {
		t.Fatal("renewed before a third of the lifetime was left")
	}
	r.clock.Add(2 * 24 * time.Hour)
	if _, err := a.Step(ctx); err != nil || a.Leaf().Equal(first) {
		t.Fatalf("didn't renew at a third left: %v", err)
	}
	if SPKIPin(a.Leaf()) != SPKIPin(first) {
		t.Fatal("renewal changed the key")
	}
	// KeyRotate passes: the next step rotates.
	next := a.SPKIs()[1]
	r.clock.Add(141 * 24 * time.Hour)
	if _, err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if SPKIPin(a.Leaf()) != next {
		t.Fatal("the announced next key didn't become current")
	}
}

func TestACMEUsesARIWindow(t *testing.T) {
	r := newRig(t)
	asked := 0
	r.ca.Window = func(leaf *x509.Certificate) (time.Time, time.Time) {
		asked++
		return leaf.NotBefore.Add(10 * 24 * time.Hour), leaf.NotBefore.Add(11 * 24 * time.Hour)
	}
	a := r.open(t)
	a.rand = func() float64 { return 0.5 }
	ctx := context.Background()
	if _, err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	first := a.Leaf()
	if _, err := a.Step(ctx); err != nil || !a.Leaf().Equal(first) || asked != 1 {
		t.Fatalf("asked %d err %v", asked, err)
	}
	r.clock.Add(10*24*time.Hour + 13*time.Hour) // past the window's middle
	if _, err := a.Step(ctx); err != nil || a.Leaf().Equal(first) {
		t.Fatalf("didn't renew inside the ARI window: %v", err)
	}
}

func TestACMEFailedValidationBacksOffAndWarns(t *testing.T) {
	r := newRig(t)
	r.ca.Dial = func(context.Context, string) (net.Conn, error) { return nil, errors.New("unreachable") }
	a := r.open(t)
	d1, err := a.Step(context.Background())
	if err == nil || d1 != retryMin || a.Warning() == nil {
		t.Fatalf("delay %v err %v warning %v", d1, err, a.Warning())
	}
	d2, _ := a.Step(context.Background())
	if d2 != 2*retryMin {
		t.Fatalf("second delay %v", d2)
	}
}

func TestACMEReadyGatesIssuance(t *testing.T) {
	r := newRig(t)
	a := r.open(t, func(c *ACMEConfig) {
		c.Ready = func(context.Context) error { return errors.New("tunnel down") }
	})
	if _, err := a.Step(context.Background()); err == nil || len(r.ca.Validations()) != 0 {
		t.Fatal("validated before the tunnel was ready")
	}
}

func TestACMEExpiryAlarm(t *testing.T) {
	r := newRig(t)
	var alarms []time.Duration
	a := r.open(t, func(c *ACMEConfig) { c.OnExpiring = func(d time.Duration) { alarms = append(alarms, d) } })
	if err := a.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.checkExpiry()
	if a.Warning() != nil || len(alarms) != 0 {
		t.Fatal("alarm with 90 days left")
	}
	r.clock.Add(81 * 24 * time.Hour)
	a.checkExpiry()
	a.checkExpiry()
	if a.Warning() == nil || len(alarms) != 1 {
		t.Fatalf("alarms %v warning %v", alarms, a.Warning())
	}
	r.clock.Add(25 * time.Hour)
	a.checkExpiry()
	if len(alarms) != 2 {
		t.Fatal("no daily repeat")
	}
}

func TestChallengeConfigOnlyForACMEHellos(t *testing.T) {
	r := newRig(t)
	a := r.open(t)
	if c, err := a.ChallengeConfig(&tls.ClientHelloInfo{ServerName: testHost, SupportedProtos: []string{"h2"}}); c != nil || err != nil {
		t.Fatal("ordinary hello diverted")
	}
	if _, err := a.ChallengeConfig(&tls.ClientHelloInfo{ServerName: testHost, SupportedProtos: []string{acme.ALPNProto}}); err == nil {
		t.Fatal("answered acme-tls/1 with nothing pending")
	}
	if TLSConfig(a).GetConfigForClient == nil || TLSConfig(Files("x", "y")).GetConfigForClient != nil {
		t.Fatal("TLSConfig hook")
	}
}

func TestNewACMERejectsBadNames(t *testing.T) {
	for _, h := range []string{"", "*.mirrin.test", "twin", "a b.test", "host:443"} {
		if _, err := NewACME(ACMEConfig{Dir: t.TempDir(), Hostnames: []string{h}}); err == nil {
			t.Fatalf("accepted %q", h)
		}
	}
}

func TestARICertID(t *testing.T) {
	// RFC 9773 appendix A: the serial's DER content keeps its leading zero.
	serial, _ := new(big.Int).SetString("87654321", 16)
	c := &x509.Certificate{SerialNumber: serial, AuthorityKeyId: []byte{0x69, 0x88, 0x5b, 0x6b, 0x87, 0x46, 0x40, 0x41, 0xe1, 0xb3, 0x7b, 0x84, 0x7b, 0xa0, 0xae, 0x2c, 0xde, 0x01, 0xc8, 0xd4}}
	id, err := ARICertID(c)
	if err != nil || id != "aYhba4dGQEHhs3uEe6CuLN4ByNQ.AIdlQyE" {
		t.Fatalf("%q %v", id, err)
	}
	if _, err := ARICertID(&x509.Certificate{SerialNumber: big.NewInt(1)}); err == nil {
		t.Fatal("no AKI")
	}
}

func TestSPKIPinMatchesKeyPin(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	c, _ := x509.ParseCertificate(der)
	if SPKIPin(c) != keyPin(k) {
		t.Fatal("pin forms differ")
	}
	if _, err := base64.RawURLEncoding.DecodeString(SPKIPin(c)); err != nil {
		t.Fatal(err)
	}
}

// Regression (review): opening data/tls always rewrote issued.json, so
// `mirrin reach use relay` and a running daemon overwrote each other's
// state. Opening an unchanged directory leaves the file alone.
func TestNewACMELeavesUnchangedStateAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := ACMEConfig{Dir: dir, Hostnames: []string{"twin.example.com"}, Directory: "https://ca.invalid/dir"}
	if _, err := NewACME(cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, issuedFile)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewACME(cfg); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("issued.json was rewritten though nothing changed")
	}
	// A new CA does change it: an account belongs to one CA.
	cfg.Directory = "https://other-ca.invalid/dir"
	if _, err := NewACME(cfg); err != nil {
		t.Fatal(err)
	}
	if st, _ := ReadACMEState(dir); st.Directory != cfg.Directory {
		t.Fatalf("directory %q", st.Directory)
	}
}
