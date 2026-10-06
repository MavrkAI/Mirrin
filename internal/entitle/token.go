package entitle

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Key purposes. Each has its own kid prefix and its own compiled-in key set,
// so a key for one purpose never verifies a token of another.
const (
	entitlementPrefix = "ent-"
	denyListPrefix    = "dl-"
)

// maxFooter bounds the footer, which is read before the signature is checked.
const maxFooter = 128

// leeway is how far ahead of our clock an iat or nbf may be.
const leeway = 5 * time.Minute

// footer is the whole footer: {"kid":"ent-2026a"}.
type footer struct {
	Kid string `json:"kid"`
}

// seal signs payload under kid, which must carry prefix.
func seal(payload []byte, prefix, kid string, priv ed25519.PrivateKey, max int) (string, error) {
	if err := checkKid(kid, prefix); err != nil {
		return "", err
	}
	f, err := json.Marshal(footer{Kid: kid})
	if err != nil {
		return "", err
	}
	return signV4(priv, payload, f, nil, max)
}

// open verifies tok for one purpose and returns the signed payload. The kid
// in the footer picks the key; it must carry prefix and be in keys. The
// implicit assertion is empty.
func open(tok, prefix string, keys map[string]ed25519.PublicKey, max int) ([]byte, error) {
	m, sig, f, err := parseV4(tok, max)
	if err != nil {
		return nil, err
	}
	kid, err := parseFooter(f)
	if err != nil {
		return nil, err
	}
	if err := checkKid(kid, prefix); err != nil {
		return nil, err
	}
	pub, ok := keys[kid]
	if !ok {
		return nil, fmt.Errorf("%w: unknown key %q", ErrSignature, kid)
	}
	if err := verifyV4(pub, m, sig, f, nil); err != nil {
		return nil, err
	}
	return m, nil
}

func parseFooter(f []byte) (string, error) {
	if len(f) == 0 {
		return "", fmt.Errorf("%w: no key id", ErrMalformed)
	}
	if len(f) > maxFooter {
		return "", fmt.Errorf("%w: footer too long", ErrMalformed)
	}
	var ft footer
	if err := json.Unmarshal(f, &ft, json.RejectUnknownMembers(true)); err != nil {
		return "", fmt.Errorf("%w: footer: %v", ErrMalformed, err)
	}
	return ft.Kid, nil
}

// ErrPurpose means the token names a key meant for something else, such as
// a deny list presented as an entitlement.
var ErrPurpose = errors.New("entitle: key is for another purpose")

// checkKid insists on prefix followed by 1–32 of [a-z0-9-].
func checkKid(kid, prefix string) error {
	rest, ok := strings.CutPrefix(kid, prefix)
	if !ok {
		return fmt.Errorf("%w: key id %q is not a %s* key", ErrPurpose, kid, prefix)
	}
	if rest == "" || len(rest) > 32 {
		return fmt.Errorf("%w: bad key id %q", ErrMalformed, kid)
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
			return fmt.Errorf("%w: bad key id %q", ErrMalformed, kid)
		}
	}
	return nil
}

// decodeStrict unmarshals a signed payload: no unknown or duplicate members,
// no trailing data, valid UTF-8.
func decodeStrict(b []byte, v any) error {
	if err := json.Unmarshal(b, v, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return nil
}

// EncodeKey is the text form of a public key used in cnf, deny lists and the
// compiled-in key sets: unpadded base64url of the 32 bytes.
func EncodeKey(pub ed25519.PublicKey) string { return b64.EncodeToString(pub) }

// ParseKey reverses EncodeKey.
func ParseKey(s string) (ed25519.PublicKey, error) {
	if !isB64URL(s) {
		return nil, errors.New("entitle: bad public key encoding")
	}
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("entitle: bad public key")
	}
	return ed25519.PublicKey(b), nil
}

// formatTime writes a PASETO DateTime: RFC 3339, UTC, whole seconds.
func formatTime(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// inRange reports whether t is set and RFC 3339 can carry it.
func inRange(t time.Time) bool { return t.Year() >= 1970 && t.Year() <= 9999 }

// parseTime reads a PASETO DateTime: RFC 3339 with an uppercase T and Z.
func parseTime(name, s string) (time.Time, error) {
	if len(s) < 20 || len(s) > 35 || s[10] != 'T' || strings.ContainsRune(s, 'z') {
		return time.Time{}, fmt.Errorf("%w: %s is not an RFC 3339 time", ErrMalformed, name)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s: %v", ErrMalformed, name, err)
	}
	return t.UTC(), nil
}
