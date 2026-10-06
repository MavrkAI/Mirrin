package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/reach"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr/acmetest"
	"testing"
)

func TestReachUseRelayPrintsAllowLineAndRecords(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	ca := acmetest.New()
	defer ca.Close()
	acmeHTTPClient = ca.Client()
	defer func() { acmeHTTPClient = nil }()
	loaded, _ := config.Load()
	var out bytes.Buffer
	err := useRelay(loaded, []string{"wss://127.0.0.1:8443/v1/tunnel", "--hostname", "Twin.Example.com", "--acme", ca.Directory()}, &out)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := config.Load()
	if got.Reach.Mode != "relay" || got.Reach.Hostname != "twin.example.com" || got.Reach.RelayURL != "wss://127.0.0.1:8443/v1/tunnel" || got.Reach.ACMEDirectory != ca.Directory() {
		t.Fatalf("saved %+v", got.Reach)
	}
	key, _, _ := reach.RelayKeys(got.DataDir)
	s := out.String()
	for _, want := range []string{
		reach.AllowLine("twin.example.com", key),
		"twin.example.com. 300 IN A 127.0.0.1",
		`_mirrin.twin.example.com. 300 IN TXT "v=mirrin1; acct=`,
		"accounturi=" + ca.URL + "/acct/",
		"validationmethods=tls-alpn-01",
		`0 issuewild ";"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, " IN CNAME ") || strings.Contains(s, "Heads up") {
		t.Fatalf("output:\n%s", s)
	}
	// fingerprint shows the pins relay mode will present.
	out.Reset()
	if err := reachFingerprint(got, &out); err != nil {
		t.Fatal(err)
	}
	st, _ := tlsmgr.ReadACMEState(filepath.Join(got.DataDir, "tls"))
	if !strings.Contains(out.String(), st.Current) || !strings.Contains(out.String(), st.Next) || !strings.Contains(out.String(), st.AccountURI) {
		t.Fatalf("fingerprint:\n%s", out.String())
	}
	// A new name warns about the phones already paired.
	out.Reset()
	if err := useRelay(got, []string{"wss://127.0.0.1:8443/v1/tunnel", "--hostname=other.example.com", "--acme=" + ca.Directory()}, &out); err != nil || !strings.Contains(out.String(), "Heads up") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, bad := range [][]string{{"https://relay/v1/tunnel", "--hostname", "a.example.com"}, {"wss://relay/v1/tunnel", "--hostname"}, {"wss://a/v1/tunnel", "wss://b/v1/tunnel", "--hostname", "a.example.com"}, {"--bogus"}} {
		if err := useRelay(got, bad, io.Discard); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestReachAlarmOfflineAndVerifyNeedsRelay(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, _ := config.Load()
	var out bytes.Buffer
	if err := reachAlarm(loaded, nil, &out); err != nil || !strings.Contains(out.String(), "No certificate alarm") {
		t.Fatalf("%v %s", err, out.String())
	}
	a, _ := reach.OpenAlarm(loaded.DataDir)
	reach.Playbook(context.Background(), certwatch.Finding{Kind: certwatch.CAAMismatch, Severity: certwatch.Critical, Host: "twin.example.com", Detail: "CAA changed."}, reach.Deps{Alarm: a})
	out.Reset()
	if err := reachAlarm(loaded, nil, &out); err != nil || !strings.Contains(out.String(), "CAA changed.") {
		t.Fatalf("%v %s", err, out.String())
	}
	out.Reset()
	if err := reachAlarm(loaded, []string{"clear"}, &out); err != nil || !strings.Contains(out.String(), "Alarm cleared") {
		t.Fatalf("%v %s", err, out.String())
	}
	if b, _ := reach.OpenAlarm(loaded.DataDir); b.Active() {
		t.Fatal("not cleared")
	}
	if err := reachAlarm(loaded, []string{"bogus"}, &out); err == nil {
		t.Fatal("bad args")
	}
	if err := reachVerify(loaded, nil, &out); err == nil || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("verify off mode: %v", err)
	}
	if err := reachFingerprint(loaded, &out); err == nil {
		t.Fatal("fingerprint without keys")
	}
}

func TestReachUseAndStatus(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"tailscale", "off"} {
		if err := configureReach([]string{"use", mode}); err != nil {
			t.Fatal(err)
		}
		got, err := config.Load()
		if err != nil || got.Reach.Mode != mode || got.Reach.StepUp != "dangerous" {
			t.Fatalf("%+v %v", got, err)
		}
	}
	if err := configureReach([]string{"status"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"use", "cloud"}, {"use", "relay"}, {"use", "files"}, {"use", "files", "missing.pem", "missing.key"}, {"use", "off", "extra"}, nil} {
		if err := configureReach(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	got, _ := config.Load()
	if got.Reach.Mode != "off" {
		t.Fatal("invalid invocation changed config")
	}
}

func TestReachStayAwakeAndHelp(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"on", "off"} {
		if err := configureReach([]string{"stay-awake", arg}); err != nil {
			t.Fatal(err)
		}
		got, _ := config.Load()
		if got.Reach.StayAwake != (arg == "on") {
			t.Fatal(got.Reach)
		}
	}
	if err := configureReach([]string{"stay-awake", "maybe"}); err == nil {
		t.Fatal("invalid power setting")
	}
	if !strings.Contains(usage, "mirrin reach") || !strings.Contains(usage, "stay-awake on|off") {
		t.Fatal("reach missing from help")
	}
}
func TestReachFilesRequiresDNSName(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), IPAddresses: []net.IP{net.ParseIP("192.0.2.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := x509.MarshalPKCS8PrivateKey(key)
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}), 0600)
	if err = configureReach([]string{"use", "files", certPath, keyPath}); err == nil || !strings.Contains(err.Error(), "DNS name") {
		t.Fatal(err)
	}
	got, _ := config.Load()
	if got.Reach.Mode != "off" {
		t.Fatal("saved unusable cert")
	}
}
