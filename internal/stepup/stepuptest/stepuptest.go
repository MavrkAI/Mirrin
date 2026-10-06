// Package stepuptest is a software passkey for tests: it answers
// navigator.credentials.create and .get the way a phone's platform
// authenticator would (ES256, "none" attestation), with switches for the
// ways a real one can let a check down.
package stepuptest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Authenticator is one software passkey holder.
type Authenticator struct {
	Origin string // the page's origin, as the browser would report it
	// NoUV makes it skip user verification (a presence tap only).
	NoUV bool
	// StuckCounter keeps the signature counter from moving on, as a copied
	// key's would.
	StuckCounter bool

	mu      sync.Mutex
	key     *ecdsa.PrivateKey
	credID  []byte
	rpID    string
	user    []byte
	counter uint32
	last    []byte // the last assertion made, for replays
}

// New is an authenticator for pages at origin.
func New(origin string) *Authenticator { return &Authenticator{Origin: origin} }

var b64 = base64.RawURLEncoding

func decode(s string) ([]byte, error) { return b64.DecodeString(strings.TrimRight(s, "=")) }

// Create answers PublicKeyCredentialCreationOptions (as JSON, bare or under
// "publicKey") with a new credential, as the JSON the page would post.
func (a *Authenticator) Create(options []byte) ([]byte, error) {
	var o struct {
		PublicKey *json.RawMessage `json:"publicKey"`
	}
	if json.Unmarshal(options, &o) == nil && o.PublicKey != nil {
		options = *o.PublicKey
	}
	var opts struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(options, &opts); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	id := make([]byte, 32)
	_, _ = rand.Read(id)
	user, _ := decode(opts.User.ID)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.key, a.credID, a.rpID, a.user, a.counter = key, id, opts.RP.ID, user, 0
	clientData, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": opts.Challenge, "origin": a.Origin, "crossOrigin": false})
	x, y := padded(key.X.Bytes()), padded(key.Y.Bytes())
	cose := cborMap([][2][]byte{
		{cborInt(1), cborInt(2)},    // kty: EC2
		{cborInt(3), cborInt(-7)},   // alg: ES256
		{cborInt(-1), cborInt(1)},   // crv: P-256
		{cborInt(-2), cborBytes(x)}, // x
		{cborInt(-3), cborBytes(y)}, // y
	})
	var att []byte
	att = append(att, make([]byte, 16)...) // AAGUID: none
	att = binary.BigEndian.AppendUint16(att, uint16(len(id)))
	att = append(att, id...)
	att = append(att, cose...)
	authData := a.authData(0x40, 0) // AT: attested credential data follows
	authData = append(authData, att...)
	attObj := cborMap([][2][]byte{
		{cborText("fmt"), cborText("none")},
		{cborText("attStmt"), cborMap(nil)},
		{cborText("authData"), cborBytes(authData)},
	})
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(id), "rawId": b64.EncodeToString(id), "type": "public-key",
		"authenticatorAttachment": "platform",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData),
			"attestationObject": b64.EncodeToString(attObj),
			"transports":        []string{"internal"},
		},
		"clientExtensionResults": map[string]any{},
	})
}

// authData is the authenticator data up to the counter. Callers hold mu.
func (a *Authenticator) authData(extra byte, counter uint32) []byte {
	rp := sha256.Sum256([]byte(a.rpID))
	flags := byte(0x01) | extra // UP
	if !a.NoUV {
		flags |= 0x04 // UV
	}
	out := append([]byte{}, rp[:]...)
	out = append(out, flags)
	return binary.BigEndian.AppendUint32(out, counter)
}

// Get answers PublicKeyCredentialRequestOptions (as JSON, bare or under
// "publicKey", or the API's {"stepup": …}) with an assertion.
func (a *Authenticator) Get(options []byte) ([]byte, error) {
	var o struct {
		PublicKey *json.RawMessage `json:"publicKey"`
		StepUp    *json.RawMessage `json:"stepup"`
	}
	if json.Unmarshal(options, &o) == nil {
		switch {
		case o.StepUp != nil:
			options = *o.StepUp
		case o.PublicKey != nil:
			options = *o.PublicKey
		}
	}
	var opts struct {
		Challenge string `json:"challenge"`
		RPID      string `json:"rpId"`
	}
	if err := json.Unmarshal(options, &opts); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.key == nil {
		return nil, errors.New("no passkey on this authenticator")
	}
	if opts.RPID != "" && opts.RPID != a.rpID {
		return nil, fmt.Errorf("no passkey for %s", opts.RPID)
	}
	if !a.StuckCounter {
		a.counter++
	}
	clientData, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": opts.Challenge, "origin": a.Origin, "crossOrigin": false})
	authData := a.authData(0, a.counter)
	sum := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), sum[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(map[string]any{
		"id": b64.EncodeToString(a.credID), "rawId": b64.EncodeToString(a.credID), "type": "public-key",
		"authenticatorAttachment": "platform",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData),
			"authenticatorData": b64.EncodeToString(authData),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(a.user),
		},
		"clientExtensionResults": map[string]any{},
	})
	a.last = out
	return out, err
}

// Last is the last assertion Get made.
func (a *Authenticator) Last() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]byte(nil), a.last...)
}

func padded(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	return append(make([]byte, 32-len(b)), b...)
}

// A little CBOR: enough for a COSE key and a "none" attestation object.

func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 1<<8:
		return []byte{major<<5 | 24, byte(n)}
	case n < 1<<16:
		return binary.BigEndian.AppendUint16([]byte{major<<5 | 25}, uint16(n))
	default:
		return binary.BigEndian.AppendUint32([]byte{major<<5 | 26}, uint32(n))
	}
}

func cborInt(n int64) []byte {
	if n >= 0 {
		return cborHead(0, uint64(n))
	}
	return cborHead(1, uint64(-1-n))
}

func cborBytes(b []byte) []byte { return append(cborHead(2, uint64(len(b))), b...) }
func cborText(s string) []byte  { return append(cborHead(3, uint64(len(s))), s...) }

func cborMap(pairs [][2][]byte) []byte {
	out := cborHead(5, uint64(len(pairs)))
	for _, p := range pairs {
		out = append(out, p[0]...)
		out = append(out, p[1]...)
	}
	return out
}
