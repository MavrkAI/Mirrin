package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

func runCmd(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionAndUsage(t *testing.T) {
	if code, out, _ := runCmd("version"); code != 0 || !strings.Contains(out, "mirrin.tunnel.v1") {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, _, errs := runCmd(); code != 2 || !strings.Contains(errs, "check-config") {
		t.Fatalf("usage: %d", code)
	}
	if code, _, _ := runCmd("frobnicate"); code != 2 {
		t.Fatal("unknown command accepted")
	}
}

// keygen writes a PKCS#8 issuer key that only its owner can read, never
// overwrites one, and prints its issuer_keys entry. It makes no device
// keys: nothing in Mirrin could load one.
func TestKeygen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k.pem")
	code, out, errs := runCmd("keygen", "--out", p, "--kid", "ent-staging-a")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	fi, err := os.Stat(p)
	if err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	b, _ := os.ReadFile(p)
	blk, _ := pem.Decode(b)
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub := entitle.EncodeKey(k.(ed25519.PrivateKey).Public().(ed25519.PublicKey))
	if !strings.Contains(out, "{kid: ent-staging-a, key: "+pub+"}") {
		t.Fatalf("output %q", out)
	}
	if code, _, _ := runCmd("keygen", "--out", p); code == 0 {
		t.Fatal("overwrote a key")
	}
	for _, kid := range []string{"wk-a", "ent-", "ent-A", "dl-" + strings.Repeat("a", 33)} {
		if code, _, _ := runCmd("keygen", "--out", filepath.Join(dir, "x.pem"), "--kid", kid); code == 0 {
			t.Errorf("kid %q accepted", kid)
		}
	}
	code, _, errs = runCmd("keygen", "--out", filepath.Join(dir, "d.pem"))
	if code == 0 || !strings.Contains(errs, "mirrin reach use relay") {
		t.Fatalf("keygen without --kid: %d %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "d.pem")); !os.IsNotExist(err) {
		t.Fatal("keygen without --kid wrote a key")
	}
}

func TestCheckConfig(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "relay.yaml")
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notify := filepath.Join(dir, "relay-abuse-mail") // a full path on this system
	os.WriteFile(good, []byte("id: r1\ncontrol_hostname: relay.example.org\nstate_dir: "+dir+
		"\nallow:\n  - {hostname: twin.example.org, key: "+entitle.EncodeKey(pub)+"}\n"+
		"abuse: {notify_command: '"+notify+"'}\n"), 0o600)
	code, out, errs := runCmd("check-config", "--config", good)
	if code != 0 || !strings.Contains(out, "self-host") || !strings.Contains(out, "contacts no MavrkAI host") ||
		!strings.Contains(out, "suspensions run "+notify) {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	example, _ := filepath.Abs("../../packaging/relay/relay.example.yaml")
	if code, _, errs := runCmd("check-config", "--config", example); code != 1 || !strings.Contains(errs, "allow[0]: key") {
		t.Fatalf("the example's placeholder passed: %d %q", code, errs)
	}
	if code, _, _ := runCmd("check-config", "--config", filepath.Join(dir, "missing.yaml")); code != 1 {
		t.Fatal("missing file accepted")
	}
}

// --config names the settings; without it they are in /etc/mirrin-relay.
func TestConfigFlag(t *testing.T) {
	if got, err := configFlag("serve", nil); err != nil || got != defaultConfig {
		t.Fatalf("default: %q %v", got, err)
	}
	if got, err := configFlag("serve", []string{"--config", "/tmp/x.yaml"}); err != nil || got != "/tmp/x.yaml" {
		t.Fatalf("--config: %q %v", got, err)
	}
}
