package tlsmgr

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"github.com/MavrkAI/Mirrin/internal/tailscale"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func certificate(t *testing.T, dir string, serial int64) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "twin.ts.net"}, DNSNames: []string{"twin.ts.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(60 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	priv, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "issued-cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "issued-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestFilesReloadAndTLS(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	dir := t.TempDir()
	certificate(t, dir, 1)
	s := Files(filepath.Join(dir, "issued-cert.pem"), filepath.Join(dir, "issued-key.pem"))
	if len(s.Hostnames()) != 1 || len(s.SPKIs()) != 2 {
		t.Fatal("missing identity")
	}
	pin := s.SPKIs()[0]
	first, _ := s.GetCertificate(nil)
	sum := sha256.Sum256(first.Leaf.RawSubjectPublicKeyInfo)
	if pin != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("pin format differs from pairing")
	}
	certificate(t, dir, 2)
	if err := s.(*files).reload(); err != nil {
		t.Fatal(err)
	}
	if pin == s.SPKIs()[0] {
		t.Fatal("key not reloaded")
	}
	os.WriteFile(filepath.Join(dir, "issued-cert.pem"), []byte("broken"), 0600)
	if s.(*files).reload() == nil {
		t.Fatal("accepted broken file")
	}
	p, e := s.GetCertificate(nil)
	if e != nil || p.Leaf.SerialNumber.Int64() != 2 {
		t.Fatal("lost last good certificate")
	}
	c := TLSConfig(s, "acme-tls/1")
	if strings.Join(c.NextProtos, ",") != "h2,http/1.1,acme-tls/1" {
		t.Fatal(c.NextProtos)
	}
}
func TestTailscaleIssueRenew(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	certificate(t, dir, 1)
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"BackendState":"Running","Self":{"DNSName":"twin.ts.net."},"CertDomains":["twin.ts.net"]}`), 0600)
	path, _ := filepath.Abs("testdata/fake-tailscale.sh")
	src, e := Tailscale(context.Background(), &tailscale.CLI{Path: path, Dir: dir})
	if e != nil {
		t.Fatal(e)
	}
	s := src.(*tailscaleSource)
	defer os.RemoveAll(s.dir)
	certificate(t, dir, 2)
	if e = s.issue(context.Background()); e != nil {
		t.Fatal(e)
	}
	p, e := s.GetCertificate(nil)
	if e != nil || p.Leaf.SerialNumber.Int64() != 2 {
		t.Fatal("not renewed", e)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "issues"))
	if strings.Count(string(b), "issued") != 2 {
		t.Fatal(string(b))
	}
	info, _ := os.Stat(s.key)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e = s.Run(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(s.dir); !os.IsNotExist(e) {
		t.Fatal("temporary keys retained")
	}
}

func TestTailscaleHealthErrors(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	_, err := Tailscale(context.Background(), &tailscale.CLI{Path: filepath.Join(t.TempDir(), "missing")})
	if err == nil || !strings.Contains(err.Error(), tailscale.AdminURL) || !strings.Contains(err.Error(), "install its CLI") {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"BackendState":"Running","Self":{"DNSName":"twin.ts.net."}}`), 0600)
	path, _ := filepath.Abs("testdata/fake-tailscale.sh")
	_, err = Tailscale(context.Background(), &tailscale.CLI{Path: path, Dir: dir})
	if err == nil || !strings.Contains(err.Error(), tailscale.AdminURL) || !strings.Contains(err.Error(), "enable HTTPS") {
		t.Fatal(err)
	}
}

func TestFilesRunRetriesAndExpires(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprint(recover), func(t *testing.T) {
			dir := t.TempDir()
			certificate(t, dir, 1)
			f := Files(filepath.Join(dir, "issued-cert.pem"), filepath.Join(dir, "issued-key.pem")).(*files)
			pair, _ := f.GetCertificate(nil)
			now := pair.Leaf.NotAfter.Add(-10 * time.Second)
			f.now = func() time.Time { return now }
			original, _ := os.ReadFile(f.cert)
			os.WriteFile(f.cert, []byte("half-written"), 0600)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waits := 0
			f.wait = func(_ context.Context, d time.Duration) bool {
				waits++
				if waits <= 2 {
					if f.Warning() == nil {
						t.Fatal("missing health warning")
					}
					if _, err := f.GetCertificate(nil); err != nil {
						t.Fatal("discarded good certificate", err)
					}
					if d != time.Duration(waits)*time.Second {
						t.Fatalf("backoff %v", d)
					}
				}
				if recover && waits == 2 {
					os.WriteFile(f.cert, original, 0600)
				}
				if recover && waits == 3 {
					if f.Warning() != nil {
						t.Fatal("warning was not cleared")
					}
					cancel()
					return false
				}
				now = now.Add(d)
				return true
			}
			err := f.Run(ctx)
			if recover && err != nil {
				t.Fatal(err)
			}
			if !recover && (err == nil || !strings.Contains(err.Error(), "expired")) {
				t.Fatalf("expiry: %v", err)
			}
		})
	}
}

func TestTailscaleRunRenewsAndRetries(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprint(recover), func(t *testing.T) {
			dir := t.TempDir()
			certificate(t, dir, 1)
			os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"BackendState":"Running","Self":{"DNSName":"twin.ts.net."},"CertDomains":["twin.ts.net"]}`), 0600)
			path, _ := filepath.Abs("testdata/fake-tailscale.sh")
			source, err := Tailscale(context.Background(), &tailscale.CLI{Path: path, Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			s := source.(*tailscaleSource)
			pair, _ := s.GetCertificate(nil)
			now := time.Now()
			s.now = func() time.Time { return now }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waits := 0
			s.wait = func(_ context.Context, d time.Duration) bool {
				waits++
				switch waits {
				case 1:
					b, _ := os.ReadFile(filepath.Join(dir, "issues"))
					if strings.Count(string(b), "issued") != 1 || d != time.Hour {
						t.Fatal("fresh certificate renewed", string(b), d)
					}
					now = pair.Leaf.NotAfter.Add(-10 * time.Second)
					os.WriteFile(filepath.Join(dir, "fail-renew"), nil, 0600)
				case 2:
					if s.Warning() == nil || d != time.Second {
						t.Fatal("no renewal warning/backoff", d)
					}
					now = now.Add(d)
					if recover {
						os.Remove(filepath.Join(dir, "fail-renew"))
						certificate(t, dir, 2)
					}
				case 3:
					if recover {
						p, e := s.GetCertificate(nil)
						if e != nil || p.Leaf.SerialNumber.Int64() != 2 || s.Warning() != nil {
							t.Fatal("not recovered", e)
						}
						b, _ := os.ReadFile(filepath.Join(dir, "issues"))
						if strings.Count(string(b), "issued") != 2 {
							t.Fatal(string(b))
						}
						cancel()
						return false
					}
					now = now.Add(d)
				default:
					now = now.Add(d)
				}
				return true
			}
			err = s.Run(ctx)
			if recover && err != nil {
				t.Fatal(err)
			}
			if !recover && (err == nil || !strings.Contains(err.Error(), "expired")) {
				t.Fatal(err)
			}
			if _, err = os.Stat(s.dir); !os.IsNotExist(err) {
				t.Fatal("keys not cleaned up", err)
			}
		})
	}
}

func TestFilesPublishNextKeyAndRotate(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	dir := t.TempDir()
	certificate(t, dir, 1)
	f := Files(filepath.Join(dir, "issued-cert.pem"), filepath.Join(dir, "issued-key.pem")).(*files)
	pins := f.SPKIs()
	if len(pins) != 2 {
		t.Fatal(pins)
	}
	reopened := Files(f.cert, f.key)
	if reopened.SPKIs()[1] != pins[1] {
		t.Fatal("next key changed on restart")
	}
	b, err := os.ReadFile(f.key + ".next")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(f.key + ".next")
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	p, _ := f.GetCertificate(nil)
	template := *p.Leaf
	template.SerialNumber = big.NewInt(2)
	template.PublicKey = f.next.Public()
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, f.next.Public(), f.next)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(f.key, b, 0600)
	os.WriteFile(f.cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	if err = f.reload(); err != nil {
		t.Fatal(err)
	}
	next := f.SPKIs()
	if next[0] != pins[1] || next[1] == pins[1] {
		t.Fatalf("rotation %v => %v", pins, next)
	}
}

func TestFilesNextKeyFailureKeepsValidCertificate(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	dir := t.TempDir()
	certificate(t, dir, 1)
	key := filepath.Join(dir, "issued-key.pem")
	os.WriteFile(key+".next", []byte("damaged next key"), 0600)
	f := Files(filepath.Join(dir, "issued-cert.pem"), key).(*files)
	if _, err := f.GetCertificate(nil); err != nil {
		t.Fatal("next key prevented serving the current certificate", err)
	}
	f.wait = func(context.Context, time.Duration) bool {
		if f.Warning() == nil {
			t.Fatal("missing warning")
		}
		return false
	}
	if err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}
