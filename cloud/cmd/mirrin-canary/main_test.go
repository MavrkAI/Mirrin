package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "Usage:") {
		t.Fatalf("no args: %d %q", code, errb.String())
	}
	if code := run(context.Background(), []string{"version"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "mirrin-canary") {
		t.Fatalf("version: %d", code)
	}
	errb.Reset()
	if code := run(context.Background(), []string{"page-everyone"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "unknown command") {
		t.Fatalf("unknown: %d %q", code, errb.String())
	}
}

// fakeCanary is an HTTPS server answering /healthz as the canary twin
// would, claiming to be carried by relay.
func fakeCanary(t *testing.T, relay string, status int) (*httptest.Server, *x509.CertPool) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(healthz{OK: true, Host: r.Host, Relay: relay, Time: time.Now()})
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

// A probe only counts an answer that names the relay it dialled, with a
// certificate for the canary's name: anything else answering on a relay's
// address is a failure.
func TestProbeCheck(t *testing.T) {
	good, roots := fakeCanary(t, "r1", 200)
	wrong, wrongRoots := fakeCanary(t, "r2", 200)
	broken, brokenRoots := fakeCanary(t, "r1", 503)
	for _, c := range []struct {
		name  string
		addr  string
		host  string
		roots *x509.CertPool
		want  string
	}{
		{"good", good.Listener.Addr().String(), "example.com", roots, ""},
		{"wrong relay", wrong.Listener.Addr().String(), "example.com", wrongRoots, `carried by relay "r2"`},
		{"503", broken.Listener.Addr().String(), "example.com", brokenRoots, "status 503"},
		{"other name", good.Listener.Addr().String(), "canary.example.net", roots, "tls:"},
		{"untrusted", good.Listener.Addr().String(), "example.com", nil, "tls:"},
		{"nothing there", "127.0.0.1:1", "example.com", roots, "connect:"},
	} {
		p := &prober{cfg: ProbeConfig{Host: c.host, Timeout: 5 * time.Second}, roots: c.roots, now: time.Now}
		_, err := p.check(context.Background(), "r1", c.addr)
		if (err == nil) != (c.want == "") || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// probe --once prints each relay and exits 1 when one failed.
func TestProbeOnce(t *testing.T) {
	good, _ := fakeCanary(t, "r1", 200)
	dir := t.TempDir()
	rootsFile := filepath.Join(dir, "roots.pem")
	os.WriteFile(rootsFile, certPEM(good), 0o600)
	cfgPath := filepath.Join(dir, "canary.yaml")
	os.WriteFile(cfgPath, []byte(`probe:
  region: here
  host: example.com
  roots: `+rootsFile+`
  relays:
    - {id: r1, addrs: ["`+good.Listener.Addr().String()+`"]}
    - {id: r2, addrs: ["127.0.0.1:1"]}
`), 0o600)
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"probe", "--config", cfgPath, "--once"}, &out, &errb)
	if code != 1 || !strings.Contains(out.String(), "via r1: ok") || !strings.Contains(out.String(), "via r2: FAILED") {
		t.Fatalf("%d\n%s\n%s", code, out.String(), errb.String())
	}
}

func certPEM(s *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
}

// The test page goes to the pager, then the mail, then resolves.
func TestSendTestPage(t *testing.T) {
	pager := newFakePager(t)
	mail := newFakeSMTP(t)
	n := newNotifier(WatchConfig{Page: PageConfig{Format: "webhook", URL: pager.URL},
		Email: EmailConfig{SMTP: mail.addr, From: "c@example.org", To: []string{"o@example.org"}}})
	var out bytes.Buffer
	if err := sendTestPage(context.Background(), n, &out); err != nil {
		t.Fatal(err)
	}
	if pager.triggers != 1 || pager.resolves != 1 || len(mail.messages()) != 1 {
		t.Fatalf("%d triggers, %d resolves, %d mails", pager.triggers, pager.resolves, len(mail.messages()))
	}
}
