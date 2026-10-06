// Package keys holds the control plane's signing keys. Each purpose has its
// own: ent-* keys sign entitlements and dl-* keys sign deny lists, so a key
// for one can never sign the other (internal/entitle checks the kid's
// prefix too). In production the private keys are files <kid>.pem, PKCS #8
// PEM, in a directory only the service can read, such as the one systemd
// fills from encrypted credentials ($CREDENTIALS_DIRECTORY). Every key in the
// directory is published at /v1/keys, so the next key can ship a release
// ahead of use; the config names the one of each purpose that signs.
package keys

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Purposes, as kid prefixes.
const (
	Entitlement = "ent"
	DenyList    = "dl"
)

// Set is the keys in use: the private key of each purpose that signs now,
// and every public key published.
type Set struct {
	EntKid string
	Ent    ed25519.PrivateKey
	DLKid  string
	DL     ed25519.PrivateKey
	// EntPublic and DLPublic are published at /v1/keys: the signing keys
	// and any others in the directory (the next ones).
	EntPublic map[string]ed25519.PublicKey
	DLPublic  map[string]ed25519.PublicKey
}

// Purpose returns the purpose a kid names, if it is a well-formed kid: the
// prefix, a hyphen, and 1 to 32 of a-z, 0-9 and '-'.
func Purpose(kid string) (string, bool) {
	for _, p := range []string{Entitlement, DenyList} {
		rest, ok := strings.CutPrefix(kid, p+"-")
		if ok && rest != "" && len(rest) <= 32 && strings.Trim(rest, "abcdefghijklmnopqrstuvwxyz0123456789-") == "" {
			return p, true
		}
	}
	return "", false
}

// Load reads every <kid>.pem in dir (other files are not keys of ours) and
// signs with entKid and dlKid.
func Load(dir, entKid, dlKid string) (*Set, error) {
	if p, ok := Purpose(entKid); !ok || p != Entitlement {
		return nil, fmt.Errorf("keys: %q is not an ent-* kid", entKid)
	}
	if p, ok := Purpose(dlKid); !ok || p != DenyList {
		return nil, fmt.Errorf("keys: %q is not a dl-* kid", dlKid)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.pem"))
	if err != nil {
		return nil, err
	}
	s := &Set{EntKid: entKid, DLKid: dlKid, EntPublic: map[string]ed25519.PublicKey{}, DLPublic: map[string]ed25519.PublicKey{}}
	seen := map[string]string{} // public key → kid
	for _, f := range files {
		kid := strings.TrimSuffix(filepath.Base(f), ".pem")
		purpose, ok := Purpose(kid)
		if !ok {
			continue // another credential, such as a TLS key
		}
		priv, err := readKey(f)
		if err != nil {
			return nil, err
		}
		pub := priv.Public().(ed25519.PublicKey)
		if other, dup := seen[string(pub)]; dup {
			return nil, fmt.Errorf("keys: %s and %s are the same key; each kid needs its own", kid, other)
		}
		seen[string(pub)] = kid
		switch purpose {
		case Entitlement:
			s.EntPublic[kid] = pub
		case DenyList:
			s.DLPublic[kid] = pub
		}
		switch kid {
		case entKid:
			s.Ent = priv
		case dlKid:
			s.DL = priv
		}
	}
	if s.Ent == nil || s.DL == nil {
		return nil, fmt.Errorf("keys: %s needs %s.pem and %s.pem", dir, entKid, dlKid)
	}
	return s, nil
}

// readKey reads one Ed25519 PKCS #8 PEM key that only its owner can read.
func readKey(path string) (ed25519.PrivateKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("keys: %s can be read by others (mode %v); chmod 600 it", path, fi.Mode().Perm())
	}
	if fi.Size() > 4096 {
		return nil, fmt.Errorf("keys: %s is too large to be a key", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, rest := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("keys: %s is not one PKCS #8 PEM private key", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("keys: %s: %w", path, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("keys: %s is not an Ed25519 key", path)
	}
	return priv, nil
}

// Generate makes a new key for kid in dir (created 0700), written 0600 and
// never over an existing file, and returns its public half.
func Generate(dir, kid string) (ed25519.PublicKey, error) {
	if _, ok := Purpose(kid); !ok {
		return nil, fmt.Errorf("keys: %q is not an ent-* or dl-* kid", kid)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, kid+".pem"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}), f.Close()); err != nil {
		os.Remove(f.Name())
		return nil, err
	}
	return pub, nil
}

// NextKid is the first unused kid for purpose and year in dir:
// ent-2027a, then ent-2027b, and so on.
func NextKid(dir, purpose string, year int) (string, error) {
	if purpose != Entitlement && purpose != DenyList {
		return "", fmt.Errorf("keys: purpose %q is not ent or dl", purpose)
	}
	for c := 'a'; c <= 'z'; c++ {
		kid := fmt.Sprintf("%s-%d%c", purpose, year, c)
		if _, err := os.Stat(filepath.Join(dir, kid+".pem")); errors.Is(err, fs.ErrNotExist) {
			return kid, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("keys: no free %s kid for %d", purpose, year)
}

// Public reads the public halves of every key in dir, by kid.
func Public(dir string) (map[string]ed25519.PublicKey, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.pem"))
	if err != nil {
		return nil, err
	}
	out := map[string]ed25519.PublicKey{}
	for _, f := range files {
		kid := strings.TrimSuffix(filepath.Base(f), ".pem")
		if _, ok := Purpose(kid); !ok {
			continue
		}
		priv, err := readKey(f)
		if err != nil {
			return nil, err
		}
		out[kid] = priv.Public().(ed25519.PublicKey)
	}
	return out, nil
}

// Kids returns m's kids, sorted.
func Kids(m map[string]ed25519.PublicKey) []string {
	return slices.Sorted(maps.Keys(m))
}

// DevKey is the public development key for kid: SHA-256("antbot dev key " +
// kid) as the seed. Anyone can derive it, so it proves nothing; builds tagged
// mirrin_devkeys trust it and nothing that ships does.
func DevKey(kid string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("antbot dev key " + kid))
	return ed25519.NewKeyFromSeed(seed[:])
}

// Dev is the key set of --dev mode: the development keys that
// internal/entitle trusts under -tags mirrin_devkeys, signing with the "a"
// keys and publishing the "b" keys as next.
func Dev() *Set {
	pub := func(kid string) ed25519.PublicKey { return DevKey(kid).Public().(ed25519.PublicKey) }
	return &Set{
		EntKid: "ent-dev-a", Ent: DevKey("ent-dev-a"),
		DLKid: "dl-dev-a", DL: DevKey("dl-dev-a"),
		EntPublic: map[string]ed25519.PublicKey{"ent-dev-a": pub("ent-dev-a"), "ent-dev-b": pub("ent-dev-b")},
		DLPublic:  map[string]ed25519.PublicKey{"dl-dev-a": pub("dl-dev-a"), "dl-dev-b": pub("dl-dev-b")},
	}
}
