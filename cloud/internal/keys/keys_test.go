package keys

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// --dev signs with the keys a daemon built with -tags mirrin_devkeys
// trusts, and publishes the same set.
func TestDevMatchesTheDaemonsDevKeys(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "entitle", "keys_dev.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, m := range regexp.MustCompile(`(?:EntitlementKeys|DenyListKeys)\["([a-z0-9-]+)"\] = mustKey\("([A-Za-z0-9_-]+)"\)`).FindAllStringSubmatch(string(src), -1) {
		want[m[1]] = m[2]
	}
	d := Dev()
	got := map[string]string{}
	for kid, k := range d.EntPublic {
		got[kid] = entitle.EncodeKey(k)
	}
	for kid, k := range d.DLPublic {
		got[kid] = entitle.EncodeKey(k)
	}
	if len(want) != 4 || len(got) != len(want) {
		t.Fatalf("daemon dev keys %v, --dev keys %v", want, got)
	}
	for kid, k := range want {
		if got[kid] != k {
			t.Errorf("%s: --dev publishes %s, the daemon trusts %s", kid, got[kid], k)
		}
	}
	if entitle.EncodeKey(d.Ent.Public().(ed25519.PublicKey)) != want[d.EntKid] || entitle.EncodeKey(d.DL.Public().(ed25519.PublicKey)) != want[d.DLKid] {
		t.Error("--dev signs with keys the daemon does not trust")
	}
}

func TestGenerateAndLoad(t *testing.T) {
	dir := t.TempDir()
	for _, kid := range []string{"ent-2026a", "dl-2026a", "ent-2027a"} {
		if _, err := Generate(dir, kid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Generate(dir, "ent-2026a"); err == nil {
		t.Error("Generate overwrote a key")
	}
	s, err := Load(dir, "ent-2026a", "dl-2026a")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.EntPublic) != 2 || len(s.DLPublic) != 1 || s.Ent == nil || s.DL == nil {
		t.Errorf("%d ent, %d dl", len(s.EntPublic), len(s.DLPublic))
	}
	os.WriteFile(filepath.Join(dir, "tls-key.pem"), []byte("not ours"), 0o644)
	if _, err := Load(dir, "ent-2026a", "dl-2026a"); err != nil {
		t.Errorf("another credential in the directory: %v", err)
	}
	if kid, _ := NextKid(dir, Entitlement, 2026); kid != "ent-2026b" {
		t.Errorf("NextKid = %s", kid)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, "dl-2026a.pem")); fi.Mode().Perm() != 0o600 {
			t.Errorf("key mode %v", fi.Mode().Perm())
		}
	}

	for name, tc := range map[string]struct {
		setup      func(dir string)
		ent, dl    string
		skipOnWins bool
	}{
		"kids swapped":      {nil, "dl-2026a", "ent-2026a", false},
		"missing signer":    {nil, "ent-2030a", "dl-2026a", false},
		"readable by group": {func(d string) { os.Chmod(filepath.Join(d, "ent-2026a.pem"), 0o640) }, "ent-2026a", "dl-2026a", true},
		"one key twice": {func(d string) {
			b, _ := os.ReadFile(filepath.Join(d, "ent-2026a.pem"))
			os.WriteFile(filepath.Join(d, "dl-2027a.pem"), b, 0o600)
		}, "ent-2026a", "dl-2026a", false},
		"not a key": {func(d string) { os.WriteFile(filepath.Join(d, "dl-2027b.pem"), []byte("hello"), 0o600) }, "ent-2026a", "dl-2026a", false},
	} {
		if tc.skipOnWins && runtime.GOOS == "windows" {
			continue
		}
		d := t.TempDir()
		for _, kid := range []string{"ent-2026a", "dl-2026a"} {
			Generate(d, kid)
		}
		if tc.setup != nil {
			tc.setup(d)
		}
		if _, err := Load(d, tc.ent, tc.dl); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestPurpose(t *testing.T) {
	for kid, want := range map[string]string{"ent-2026a": Entitlement, "dl-dev-b": DenyList, "ent-": "", "wk-2026a": "", "ent-A": "", "dl-x/y": ""} {
		if p, _ := Purpose(kid); p != want {
			t.Errorf("Purpose(%q) = %q, want %q", kid, p, want)
		}
	}
}
