package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/cloud/cloudtest"
	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// startDev runs `mirrin-cloud serve --dev` on a loopback port with its data
// in dir, and returns its origin. It stops when the test ends.
func startDev(t *testing.T, dir string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	origin := make(chan string, 1)
	done := make(chan int, 1)
	var stderr bytes.Buffer
	go func() {
		done <- run(ctx, []string{"serve", "--dev", "--listen", "127.0.0.1:0", "--data", dir}, os.Stdout, &stderr, func(o string) { origin <- o })
	}()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("serve --dev exited %d: %s", code, stderr.String())
		}
	})
	select {
	case o := <-origin:
		return o
	case code := <-done:
		t.Fatalf("serve --dev exited %d: %s", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("serve --dev did not start")
	}
	return ""
}

// The binary's --dev mode passes the daemon's contract suite.
func TestServeDevPassesTheContract(t *testing.T) {
	cloudtest.Contract(t, startDev(t, t.TempDir()))
}

// fetchKeys reads /v1/keys.
func fetchKeys(t *testing.T, origin string) map[string]ed25519.PublicKey {
	t.Helper()
	resp, err := http.Get(origin + "/v1/keys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		Entitlement map[string]string `json:"entitlement"`
	}
	if err := json.UnmarshalRead(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]ed25519.PublicKey{}
	for kid, s := range doc.Entitlement {
		k, err := entitle.ParseKey(s)
		if err != nil {
			t.Fatal(err)
		}
		out[kid] = k
	}
	return out
}

// A daemon links against `serve --dev` the way `mirrin cloud link` does;
// then the admin commands work on the same data.
func TestDevLinkAndAdmin(t *testing.T) {
	data := t.TempDir()
	origin := startDev(t, data)
	c, err := cloud.New(t.TempDir(), origin, fetchKeys(t, origin))
	if err != nil {
		t.Fatal(err)
	}
	ls, err := c.StartLink(t.Context(), "cli-test-01", "https://acme-staging-v02.api.letsencrypt.org/acme/acct/1")
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := http.Get(ls.CheckoutURL); err != nil || resp.StatusCode != 200 {
		t.Fatalf("checkout: %v %v", resp, err)
	}
	st, ent, err := c.PollLink(t.Context(), ls.ID)
	if err != nil || st != cloud.LinkActive || ent == "" {
		t.Fatalf("poll: %q %v", st, err)
	}
	pub, _ := c.PublicKey()

	cfg := filepath.Join(t.TempDir(), "cloud.yaml")
	os.WriteFile(cfg, []byte("data_dir: "+data+"\n"), 0o600)
	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"admin", "show", "cli-test-01", "--config", cfg}, &out, &errb, nil); code != 0 {
		t.Fatalf("admin show: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), entitle.EncodeKey(pub)) || strings.Contains(out.String(), "@") {
		t.Errorf("admin show: %s", out.String())
	}
	out.Reset()
	if code := run(t.Context(), []string{"admin", "deny", "key", entitle.EncodeKey(pub), "--why", "test", "--config", cfg}, &out, &errb, nil); code != 0 {
		t.Fatalf("admin deny key: %d %s", code, errb.String())
	}
	if _, err := c.Refresh(t.Context()); err == nil {
		t.Error("refresh with a denied key succeeded")
	}
	if code := run(t.Context(), []string{"admin", "deny", "handle", "cli-test-01", "--config", cfg}, &out, &errb, nil); code != 0 {
		t.Fatalf("admin deny handle: %d %s", code, errb.String())
	}
	if code := run(t.Context(), []string{"admin", "deny", "handle", "nope-nope", "--config", cfg}, &out, &errb, nil); code == 0 {
		t.Error("denying an unknown handle succeeded")
	}
}

func TestKeysCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"keys", "generate", "--dir", dir}, &out, &errb, nil); code != 0 {
		t.Fatalf("keys generate: %s", errb.String())
	}
	year := time.Now().UTC().Year()
	for _, want := range []string{`EntitlementKeys["ent-`, `DenyListKeys["dl-`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("keys generate printed %s", out.String())
		}
	}
	if code := run(t.Context(), []string{"keys", "generate", "--dir", dir}, &out, &errb, nil); code == 0 {
		t.Error("keys generate ran twice")
	}
	out.Reset()
	if code := run(t.Context(), []string{"keys", "rotate", "--dir", dir, "--purpose", "dl"}, &out, &errb, nil); code != 0 {
		t.Fatalf("keys rotate: %s", errb.String())
	}
	out.Reset()
	run(t.Context(), []string{"keys", "public", "--dir", dir}, &out, &errb, nil)
	if n := strings.Count(out.String(), "mustKey("); n != 3 || !strings.Contains(out.String(), "dl-"+itoa(year)+"b") {
		t.Errorf("keys public: %s", out.String())
	}
	if code := run(t.Context(), []string{"keys", "rotate", "--dir", dir, "--purpose", "wk"}, &out, &errb, nil); code == 0 {
		t.Error("rotated a purpose that does not exist")
	}
}

func TestServeRefusals(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"serve", "--config", filepath.Join(t.TempDir(), "none.yaml")}, &out, &errb, nil); code == 0 {
		t.Error("serve ran without a config")
	}
	cfg := filepath.Join(t.TempDir(), "cloud.yaml")
	os.WriteFile(cfg, []byte("public_url: https://cloud.example\nbilling:\n  provider: stripe\n"), 0o600)
	if code := run(t.Context(), []string{"serve", "--config", cfg}, &out, &errb, nil); code == 0 {
		t.Error("serve ran with an unsupported provider")
	}
	os.WriteFile(cfg, []byte("listen: 127.0.0.1:0\nunknown_field: 1\n"), 0o600)
	if code := run(t.Context(), []string{"serve", "--dev", "--config", cfg}, &out, &errb, nil); code == 0 {
		t.Error("serve took a config with an unknown field")
	}
	// A dev server takes no signature on its /dev/ routes: never on a
	// public interface.
	for _, l := range []string{"0.0.0.0:0", ":0", "[::]:0", "192.0.2.1:0", "localhost:0"} {
		errb.Reset()
		if code := run(t.Context(), []string{"serve", "--dev", "--listen", l, "--data", t.TempDir()}, &out, &errb, nil); code == 0 || !strings.Contains(errb.String(), "loopback") {
			t.Errorf("serve --dev --listen %s: %d %s", l, code, errb.String())
		}
	}
	os.WriteFile(cfg, []byte("listen: 0.0.0.0:0\n"), 0o600)
	if code := run(t.Context(), []string{"serve", "--dev", "--config", cfg}, &out, &errb, nil); code == 0 {
		t.Error("serve --dev took a config listening on every interface")
	}
	if code := run(t.Context(), []string{"nonsense"}, &out, &errb, nil); code != 2 {
		t.Errorf("unknown command: %d", code)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
