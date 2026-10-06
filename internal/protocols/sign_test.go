package protocols

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"

	"github.com/MavrkAI/Mirrin/registry"
)

// testSigner is a minisign key pair made for a test.
type testSigner struct {
	id   [8]byte
	priv ed25519.PrivateKey
	key  *minisignKey
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &testSigner{priv: priv}
	_, _ = rand.Read(s.id[:])
	pubFile := "untrusted comment: minisign public key\n" +
		base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), s.id[:]...), pub...)) + "\n"
	if s.key, err = parseMinisignKey([]byte(pubFile)); err != nil {
		t.Fatal(err)
	}
	return s
}

// sign makes a .minisig the way `minisign -S` does (prehashed unless legacy).
func (s *testSigner) sign(data []byte, legacy bool) string {
	alg, msg := "ED", data
	if legacy {
		alg = "Ed"
	} else {
		h := blake2b.Sum512(data)
		msg = h[:]
	}
	sig := ed25519.Sign(s.priv, msg)
	trusted := "timestamp:1700000000\tfile:index.json\thashed"
	global := ed25519.Sign(s.priv, append(append([]byte{}, sig...), trusted...))
	return "untrusted comment: signature from minisign secret key\n" +
		base64.StdEncoding.EncodeToString(append(append([]byte(alg), s.id[:]...), sig...)) + "\n" +
		"trusted comment: " + trusted + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n"
}

// useRegistryKey makes s the built-in key for the rest of the test.
func useRegistryKey(t *testing.T, s *testSigner) {
	old := registryKey
	registryKey = s.key
	t.Cleanup(func() { registryKey = old })
}

func TestVerifyMinisign(t *testing.T) {
	s := newTestSigner(t)
	data := []byte(`{"packs":[]}`)
	for _, legacy := range []bool{false, true} {
		if err := verifyMinisign(s.key, data, []byte(s.sign(data, legacy))); err != nil {
			t.Fatalf("legacy=%v: a good signature was refused: %v", legacy, err)
		}
	}
	sig := s.sign(data, false)
	if err := verifyMinisign(s.key, []byte(`{"packs":[{}]}`), []byte(sig)); err == nil {
		t.Fatal("a changed file was accepted")
	}
	lines := strings.Split(sig, "\n")
	lines[2] = "trusted comment: something else"
	if err := verifyMinisign(s.key, data, []byte(strings.Join(lines, "\n"))); err == nil {
		t.Fatal("a changed trusted comment was accepted")
	}
	other := newTestSigner(t)
	if err := verifyMinisign(other.key, data, []byte(sig)); err == nil {
		t.Fatal("a signature from another key was accepted")
	}
	if err := verifyMinisign(nil, data, []byte(sig)); err == nil {
		t.Fatal("no key should accept nothing")
	}
	if k, err := parseMinisignKey([]byte("# only a note\n")); k != nil || err == nil {
		t.Fatal("a key file with no key should read as no key")
	}
}

// The published index was used as it came: anyone who could change it on the
// way, or in the repository, could point packs at other code. It is now used
// only when its signature checks out; otherwise the built-in index answers.
func TestFetchRegistryChecksTheSignature(t *testing.T) {
	s := newTestSigner(t)
	useRegistryKey(t, s)
	good := []byte(`{"packs":[{"name":"live","description":"d","repo":"https://github.com/x/live"}]}`)
	body, sig := good, s.sign(good, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".minisig") {
			if sig == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(sig))
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	old := registryURL
	registryURL = srv.URL + "/index.json"
	defer func() { registryURL = old }()
	ctx := context.Background()

	r, err := FetchRegistry(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Find("live"); !ok {
		t.Fatalf("a correctly signed index should be used: %+v", r)
	}

	body = []byte(`{"packs":[{"name":"live","description":"d","repo":"https://github.com/evil/live"}]}`)
	r, err = FetchRegistry(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Find("live"); ok {
		t.Fatal("a tampered index was used")
	}
	if _, ok := r.Find("starter"); !ok {
		t.Fatal("a tampered index should fall back to the built-in one")
	}

	body, sig = good, ""
	if r, _ = FetchRegistry(ctx, ""); r == nil || func() bool { _, ok := r.Find("live"); return ok }() {
		t.Fatal("an unsigned index was used")
	}

	// without a key built in, only the built-in index is trusted
	registryKey = nil
	body, sig = good, s.sign(good, false)
	if r, _ = FetchRegistry(ctx, ""); r == nil || func() bool { _, ok := r.Find("live"); return ok }() {
		t.Fatal("with no key, the published index was used")
	}
}

// A badly pasted key in registry/minisign.pub used to switch the published
// index off without a word. Anything there but comments must be a key.
func TestBuiltInRegistryKeyParses(t *testing.T) {
	if _, err := parseMinisignKey(registry.PublicKey); err != nil && !errors.Is(err, errNoRegistryKey) {
		t.Fatalf("registry/minisign.pub: %v", err)
	}
	if _, err := parseMinisignKey([]byte("# a comment\nRWQ not a key\n")); err == nil || errors.Is(err, errNoRegistryKey) {
		t.Fatalf("a damaged key wasn't reported: %v", err)
	}
}

// Releases must not ship without the key, or they ignore the published
// index for good. The release workflow sets MIRRIN_RELEASE_CHECK.
func TestReleaseHasRegistryKey(t *testing.T) {
	if os.Getenv("MIRRIN_RELEASE_CHECK") == "" {
		t.Skip("only checked for a release")
	}
	if registryKey == nil {
		t.Fatal("registry/minisign.pub has no key: add it (see registry/README.md) before tagging a release")
	}
}
