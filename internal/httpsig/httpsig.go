// Package httpsig signs and verifies Mirrin Cloud API requests with HTTP
// Message Signatures (RFC 9421). The profile is fixed: Ed25519 over @method,
// @target-uri and content-digest (RFC 9530, sha-256), with created, expires
// (created+300 s), a 16-byte nonce, keyid and tag "mirrin-cloud-v1". The
// server checks the clock with a skew allowance and refuses a nonce it has
// seen. It uses the standard library only.
//
// A server calls VerifyFor with the origins it answers to, such as
// "https://cloud.mirrin.app". @target-uri is rebuilt from the origin that
// r.Host names, so a request signed for another server (staging, a
// self-hosted control plane) is refused, and the scheme is the server's own
// whichever listener or proxy the request came through. Verify is the same
// check without the list: it trusts r.Host, and takes the scheme from
// r.URL.Scheme if the server set it, else from r.TLS. Forwarding headers are
// never read.
package httpsig

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"
)

const (
	// Tag marks Mirrin Cloud signatures, so other signatures on the same
	// request are ignored.
	Tag = "mirrin-cloud-v1"
	// Label is the dictionary key Sign uses. Verify finds signatures by Tag.
	Label = "sig1"
	// Lifetime is expires minus created. Verify refuses anything longer.
	Lifetime = 300 * time.Second
)

// covered is the exact component list, in order.
var covered = []string{"@method", "@target-uri", "content-digest"}

var (
	// ErrUnsigned means there is no mirrin-cloud-v1 signature.
	ErrUnsigned = errors.New("httpsig: request is not signed")
	// ErrSignature means the signature is malformed, off-profile or wrong.
	ErrSignature = errors.New("httpsig: bad signature")
	// ErrStale means the signature is outside its time window.
	ErrStale = errors.New("httpsig: signature outside its time window")
)

var b64url = base64.RawURLEncoding.Strict()

// KeyID names a device key: "dev:" and the unpadded base64url of the first
// 16 bytes of SHA-256 of the key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "dev:" + b64url.EncodeToString(sum[:16])
}

// Sign adds Content-Digest, Signature-Input and Signature to r. keyID must be
// KeyID of priv's public key. The body is read and put back.
func Sign(r *http.Request, keyID string, priv ed25519.PrivateKey, now time.Time) error {
	if len(priv) != ed25519.PrivateKeySize {
		return errors.New("httpsig: bad private key")
	}
	if keyID != KeyID(priv.Public().(ed25519.PublicKey)) {
		return errors.New("httpsig: key id does not match the key")
	}
	if r.Header == nil {
		r.Header = http.Header{}
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	if r.Body != nil && r.Body != http.NoBody {
		r.ContentLength = int64(len(body))
	}
	o, err := clientOrigin(r)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	created := now.Unix()
	s := signature{label: Label, params: []param{
		{"created", created},
		{"expires", created + int64(Lifetime/time.Second)},
		{"nonce", b64url.EncodeToString(nonce)},
		{"keyid", keyID},
		{"tag", Tag},
	}}
	for _, c := range covered {
		s.components = append(s.components, item{v: c})
	}
	r.Header.Set("Content-Digest", contentDigest(body))
	base, err := signatureBase(r, o, s)
	if err != nil {
		return err
	}
	r.Header.Set("Signature-Input", serializeDictionary([]member{{key: Label, list: s.components, item: item{params: s.params}, isList: true}}))
	r.Header.Set("Signature", serializeDictionary([]member{{key: Label, item: item{v: ed25519.Sign(priv, []byte(base))}}}))
	return nil
}

// Verify checks r's mirrin-cloud-v1 signature and returns its key id. lookup
// maps a key id to a public key; it should fail for unknown or revoked keys.
// now is when the request arrived, and skew the clock allowance either side
// (300 s for the control plane). seen is required: the nonce is recorded
// only once everything else passes, until expires+skew. r's body is read,
// checked against Content-Digest, and put back for the handler.
//
// Verify takes the authority from r.Host, so on its own it proves only that
// the request was signed for whatever host the client named. A server must
// call VerifyFor instead, or check r.Host against its own name first.
func Verify(r *http.Request, lookup func(keyID string) (ed25519.PublicKey, error), now time.Time, skew time.Duration, seen NonceCache) (string, error) {
	return verify(r, func() (origin, error) { return listenerOrigin(r) }, lookup, now, skew, seen)
}

// VerifyFor is Verify for a server that answers to origins, each
// "scheme://host[:port]". r.Host must name one of them, or VerifyFor fails
// with ErrWrongHost; @target-uri is then built from that origin.
func VerifyFor(r *http.Request, origins []string, lookup func(keyID string) (ed25519.PublicKey, error), now time.Time, skew time.Duration, seen NonceCache) (string, error) {
	return verify(r, func() (origin, error) { return matchOrigin(r, origins) }, lookup, now, skew, seen)
}

// verify is Verify with the rule for where r is going passed in.
func verify(r *http.Request, target func() (origin, error), lookup func(keyID string) (ed25519.PublicKey, error), now time.Time, skew time.Duration, seen NonceCache) (string, error) {
	if seen == nil || lookup == nil || skew < 0 {
		return "", errors.New("httpsig: Verify needs a key lookup, a nonce cache and a non-negative skew")
	}
	o, err := target()
	if err != nil {
		return "", err
	}
	sigs, err := parseSignatures(r.Header)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSignature, err)
	}
	var s *signature
	for i := range sigs {
		if tag, _ := sigs[i].param("tag"); tag == Tag {
			if s != nil {
				return "", fmt.Errorf("%w: two %s signatures", ErrSignature, Tag)
			}
			s = &sigs[i]
		}
	}
	if s == nil {
		return "", ErrUnsigned
	}
	p, err := profile(*s)
	if err != nil {
		return "", err
	}
	// Compared at full precision, so that the nonce below is held through
	// every instant this check lets the signature in.
	last := time.Unix(p.expires, 0).Add(skew)
	if time.Unix(p.created, 0).After(now.Add(skew)) || now.After(last) {
		return "", ErrStale
	}
	pub, err := lookup(p.keyID)
	if err != nil {
		return "", fmt.Errorf("%w: key %s: %v", ErrSignature, p.keyID, err)
	}
	if len(pub) != ed25519.PublicKeySize || KeyID(pub) != p.keyID {
		return "", fmt.Errorf("%w: key %s does not match its id", ErrSignature, p.keyID)
	}
	base, err := signatureBase(r, o, *s)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSignature, err)
	}
	if !verifyBase(pub, base, s.sig) {
		return "", ErrSignature
	}
	body, err := readBody(r)
	if err != nil {
		return "", err
	}
	if err := checkDigest(r.Header, body); err != nil {
		return "", err
	}
	if err := seen.Use(p.keyID, p.nonce, now, last); err != nil {
		return "", err
	}
	return p.keyID, nil
}

type params struct {
	created, expires int64
	nonce, keyID     string
}

// profile checks that s is exactly the Mirrin profile and returns its
// parameters.
func profile(s signature) (params, error) {
	bad := func(what string) (params, error) { return params{}, fmt.Errorf("%w: %s", ErrSignature, what) }
	if s.sig == nil {
		return bad("no Signature for " + s.label)
	}
	if len(s.components) != len(covered) {
		return bad("covered components are not the profile's")
	}
	for i, c := range s.components {
		if c.v != covered[i] || len(c.params) > 0 {
			return bad("covered components are not the profile's")
		}
	}
	var p params
	var ok bool
	for _, q := range s.params {
		switch q.key {
		case "created":
			p.created, ok = q.v.(int64)
		case "expires":
			p.expires, ok = q.v.(int64)
		case "nonce":
			p.nonce, ok = q.v.(string)
		case "keyid":
			p.keyID, ok = q.v.(string)
		case "tag":
			ok = true // matched by the caller
		case "alg":
			ok = q.v == "ed25519"
		default:
			return bad("unexpected parameter " + q.key)
		}
		if !ok {
			return bad("bad parameter " + q.key)
		}
	}
	if !slices.ContainsFunc(s.params, func(q param) bool { return q.key == "created" }) ||
		!slices.ContainsFunc(s.params, func(q param) bool { return q.key == "expires" }) ||
		p.keyID == "" {
		return bad("created, expires and keyid are required")
	}
	if n, err := b64url.DecodeString(p.nonce); err != nil || len(n) != 16 {
		return bad("nonce is not 16 bytes of base64url")
	}
	if p.expires <= p.created || p.expires-p.created > int64(Lifetime/time.Second) {
		return bad("expires must be within 300 s after created")
	}
	return p, nil
}
