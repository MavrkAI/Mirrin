package entitle

// PASETO v4.public, as specified at
// https://github.com/paseto-standard/paseto-spec (docs/01-Protocol-Versions/Version4.md
// and Common.md). Only the public purpose exists here: v4.local, v3 and every
// other header are refused before anything is decoded.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// MaxTokenSize bounds an entitlement, before any decoding.
const MaxTokenSize = 8 << 10

// v4Public is the header h, trailing period included.
const v4Public = "v4.public."

// b64 is unpadded base64url that also rejects non-zero trailing bits, as the
// spec requires.
var b64 = base64.RawURLEncoding.Strict()

var (
	// ErrTooLarge means the token is over its size limit.
	ErrTooLarge = errors.New("entitle: token too large")
	// ErrUnsupported means the token is not v4.public.
	ErrUnsupported = errors.New("entitle: not a v4.public token")
	// ErrMalformed means the token does not parse.
	ErrMalformed = errors.New("entitle: malformed token")
	// ErrSignature means the signature does not verify, or no key matches.
	ErrSignature = errors.New("entitle: bad signature")
)

// pae is the pre-authentication encoding: the piece count, then each piece
// prefixed by its length, as little-endian 64-bit integers with the top bit
// cleared.
func pae(pieces ...[]byte) []byte {
	n := 8
	for _, p := range pieces {
		n += 8 + len(p)
	}
	out := make([]byte, 0, n)
	out = le64(out, len(pieces))
	for _, p := range pieces {
		out = le64(out, len(p))
		out = append(out, p...)
	}
	return out
}

func le64(b []byte, n int) []byte {
	return binary.LittleEndian.AppendUint64(b, uint64(n)&^(1<<63))
}

// signV4 signs message m with footer f and implicit assertion i.
func signV4(priv ed25519.PrivateKey, m, f, i []byte, max int) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("entitle: bad private key")
	}
	sig := ed25519.Sign(priv, pae([]byte(v4Public), m, f, i))
	body := make([]byte, 0, len(m)+len(sig))
	body = append(append(body, m...), sig...)
	tok := v4Public + b64.EncodeToString(body)
	if len(f) > 0 {
		tok += "." + b64.EncodeToString(f)
	}
	if len(tok) > max {
		return "", ErrTooLarge
	}
	return tok, nil
}

// parseV4 splits a v4.public token into message, signature and footer. It
// checks the shape only; nothing it returns is authenticated yet.
func parseV4(tok string, max int) (m, sig, f []byte, err error) {
	if len(tok) > max {
		return nil, nil, nil, ErrTooLarge
	}
	if !strings.HasPrefix(tok, v4Public) {
		return nil, nil, nil, ErrUnsupported
	}
	body, foot, hasFoot := strings.Cut(tok[len(v4Public):], ".")
	if !isB64URL(body) {
		return nil, nil, nil, fmt.Errorf("%w: payload is not unpadded base64url", ErrMalformed)
	}
	if hasFoot && !isB64URL(foot) {
		return nil, nil, nil, fmt.Errorf("%w: footer is not unpadded base64url", ErrMalformed)
	}
	raw, err := b64.DecodeString(body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: payload is not canonical base64url", ErrMalformed)
	}
	if len(raw) < ed25519.SignatureSize {
		return nil, nil, nil, fmt.Errorf("%w: too short to hold a signature", ErrMalformed)
	}
	if hasFoot {
		if f, err = b64.DecodeString(foot); err != nil {
			return nil, nil, nil, fmt.Errorf("%w: footer is not canonical base64url", ErrMalformed)
		}
	}
	cut := len(raw) - ed25519.SignatureSize
	return raw[:cut], raw[cut:], f, nil
}

// verifyV4 checks sig over m, f and i with pub.
func verifyV4(pub ed25519.PublicKey, m, sig, f, i []byte) error {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return ErrSignature
	}
	if !ed25519.Verify(pub, pae([]byte(v4Public), m, f, i), sig) {
		return ErrSignature
	}
	return nil
}

// isB64URL reports whether s is non-empty and uses only the base64url
// alphabet. The stdlib decoder skips CR and LF, so they are refused here.
func isB64URL(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
