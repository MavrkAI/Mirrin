// Package wire is the mirrin.tunnel.v1 protocol between a daemon and a
// mirrin-relay: the JSON handshake, the control stream and the PROXY v2
// header in front of every relayed connection. docs/relay-protocol.md is
// the specification. The daemon's tunnel client and the relay both use this
// package, so they parse and check every message the same way. It uses the
// standard library only.
package wire

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Protocol constants. Changing any of them is a new protocol version.
const (
	Subprotocol = "mirrin.tunnel.v1" // the WebSocket subprotocol
	Path        = "/v1/tunnel"       // where a relay serves tunnels
	Version     = 1                  // the "v" of challenge and hello

	// HelloContext separates hello signatures from every other use of a
	// device key.
	HelloContext = "mirrin-relay-tunnel-v1"
	// ExporterLabel is the RFC 8446 exporter label that binds a hello to
	// its TLS session.
	ExporterLabel = "EXPORTER-mirrin-tunnel"

	ExporterSize  = 32
	NonceSize     = 32
	StatusKeySize = 32

	// ControlStreamID is the yamux stream that carries control messages:
	// the first stream the relay opens.
	ControlStreamID = 1
)

// Size limits. Everything is checked before it is used.
const (
	MaxMessage     = 16 << 10 // one handshake text frame
	MaxEntitlement = 8 << 10  // an entitlement token (entitle.MaxTokenSize)
	MaxControlLine = 4 << 10  // one control line, without its newline
	MaxRetryAfter  = 86400    // seconds
	MaxHostnames   = 64
	maxRelayID     = 32
	maxClient      = 128
	maxText        = 512 // a message shown to the user
	maxCode        = 32
)

// Refusal codes a relay may send in an error reply. A client treats a code
// it does not know like any other retryable refusal.
const (
	CodeEntitlementExpired = "entitlement_expired"
	CodeBadSignature       = "bad_signature"
	CodeHostnameNotAllowed = "hostname_not_allowed"
	CodeDenied             = "denied"
	CodeSuperseded         = "superseded"
	CodeSupersededRetry    = "superseded_retry"
	CodeRateLimited        = "rate_limited"
	CodeUpgradeRequired    = "upgrade_required"
)

// Control message types. Unknown types parse, so a relay can add one
// without breaking older daemons, which ignore it.
const (
	ControlSuperseded = "superseded"
	ControlDrain      = "drain"
	ControlNotice     = "notice"
	ControlLimits     = "limits"
)

var (
	// ErrMalformed means a message is not valid for the protocol.
	ErrMalformed = errors.New("wire: malformed message")
	// ErrVersion means the peer speaks another protocol version.
	ErrVersion = errors.New("wire: unsupported protocol version")
	// ErrBadSignature means a hello's signature does not verify.
	ErrBadSignature = errors.New("wire: bad hello signature")
)

// b64 is unpadded base64url that rejects non-canonical encodings.
var b64 = base64.RawURLEncoding.Strict()

// Challenge is the relay's first message.
type Challenge struct {
	Relay string // the relay's id, such as "r1"
	Nonce []byte // NonceSize fresh random bytes
}

// Hello is the daemon's answer to a challenge.
type Hello struct {
	Key           ed25519.PublicKey
	Sig           []byte // see HelloSigInput
	Ent           string // entitlement token; empty travels as null (self-hosted relays)
	StatusKeyHash []byte // SHA-256 of the status key
	Client        string // "mirrin/0.4.0 darwin/arm64"
}

// Welcome is the relay's reply to an accepted hello.
type Welcome struct {
	Hostnames  []string // the names this tunnel now carries
	Gen        int64    // the generation the relay holds for them
	Keepalive  int      // seconds between yamux pings
	MaxStreams int      // concurrent streams the relay will open
}

// Error is the relay's reply to a refused hello. It is an error, so a
// client can return it and callers can pick it out with errors.As.
type Error struct {
	Code       string // one of the Code constants, or a newer one
	Message    string // a sentence shown to the user verbatim
	RetryAfter int    // seconds; 0 when the relay gave none
}

func (e Error) Error() string {
	if e.Message == "" {
		return "relay refused the tunnel: " + e.Code
	}
	return "relay refused the tunnel: " + e.Code + ": " + e.Message
}

// Control is one line on the control stream. Only the fields its type uses
// are set.
type Control struct {
	T          string
	Gen        int64  // superseded: the generation that now holds the names
	Message    string // notice, drain, superseded
	RetryAfter int    // drain: seconds before reconnecting
	MaxStreams int    // limits
	BPS        int64  // limits: bits per second per tunnel
}

// The JSON shapes, exactly as they travel.
type (
	challengeJSON struct {
		T     string `json:"t"`
		V     int    `json:"v"`
		Relay string `json:"relay"`
		Nonce string `json:"nonce"`
	}
	helloJSON struct {
		T             string  `json:"t"`
		V             int     `json:"v"`
		Key           string  `json:"key"`
		Sig           string  `json:"sig"`
		Ent           *string `json:"ent"`
		StatusKeyHash string  `json:"status_key_hash"`
		Client        string  `json:"client"`
	}
	welcomeJSON struct {
		T          string   `json:"t"`
		Hostnames  []string `json:"hostnames"`
		Gen        int64    `json:"gen"`
		Keepalive  int      `json:"keepalive"`
		MaxStreams int      `json:"max_streams"`
	}
	errorJSON struct {
		T          string `json:"t"`
		Code       string `json:"code"`
		Message    string `json:"message"`
		RetryAfter int    `json:"retry_after,omitzero"`
	}
	controlJSON struct {
		T          string `json:"t"`
		Gen        int64  `json:"gen,omitzero"`
		Message    string `json:"message,omitzero"`
		RetryAfter int    `json:"retry_after,omitzero"`
		MaxStreams int    `json:"max_streams,omitzero"`
		BPS        int64  `json:"bps,omitzero"`
	}
	typeJSON struct {
		T string `json:"t"`
	}
)

// NewChallenge returns a challenge with a fresh nonce.
func NewChallenge(relay string) (Challenge, error) {
	if !ValidRelayID(relay) {
		return Challenge{}, fmt.Errorf("%w: relay id %q", ErrMalformed, relay)
	}
	n := make([]byte, NonceSize)
	if _, err := rand.Read(n); err != nil {
		return Challenge{}, err
	}
	return Challenge{Relay: relay, Nonce: n}, nil
}

// Exporter returns the value a hello is bound to: the TLS exporter of the
// daemon-relay session. The tunnel requires TLS 1.3, where the exporter is
// unique to the session.
func Exporter(cs tls.ConnectionState) ([]byte, error) {
	if cs.Version != tls.VersionTLS13 {
		return nil, errors.New("wire: tunnel needs TLS 1.3")
	}
	return cs.ExportKeyingMaterial(ExporterLabel, nil, ExporterSize)
}

// HelloSigInput is what a hello signs:
//
//	"mirrin-relay-tunnel-v1" 0x00 relay 0x00 nonce 0x00 exporter
//
// A relay id holds no 0x00 and the nonce and exporter have fixed sizes, so
// different inputs never produce the same bytes.
func HelloSigInput(relay string, nonce, exporter []byte) []byte {
	b := make([]byte, 0, len(HelloContext)+len(relay)+len(nonce)+len(exporter)+3)
	b = append(b, HelloContext...)
	b = append(b, 0)
	b = append(b, relay...)
	b = append(b, 0)
	b = append(b, nonce...)
	b = append(b, 0)
	return append(b, exporter...)
}

// SignHello answers ch on the TLS session whose exporter is given.
func SignHello(key ed25519.PrivateKey, ch Challenge, exporter []byte, ent string, statusKeyHash []byte, client string) (Hello, error) {
	if len(key) != ed25519.PrivateKeySize {
		return Hello{}, errors.New("wire: bad device key")
	}
	if err := checkBinding(ch.Relay, ch.Nonce, exporter); err != nil {
		return Hello{}, err
	}
	h := Hello{
		Key:           key.Public().(ed25519.PublicKey),
		Sig:           ed25519.Sign(key, HelloSigInput(ch.Relay, ch.Nonce, exporter)),
		Ent:           ent,
		StatusKeyHash: statusKeyHash,
		Client:        client,
	}
	return h, h.check()
}

// VerifyHello checks that h was signed for the challenge this relay sent
// on the session with this exporter, and returns the key it proves. It
// says nothing about whether that key may use the tunnel; the relay checks
// the entitlement or its allow list next.
func VerifyHello(h Hello, relay string, nonce, exporter []byte) (ed25519.PublicKey, error) {
	if err := checkBinding(relay, nonce, exporter); err != nil {
		return nil, err
	}
	if err := h.check(); err != nil {
		return nil, err
	}
	if !ed25519.Verify(h.Key, HelloSigInput(relay, nonce, exporter), h.Sig) {
		return nil, ErrBadSignature
	}
	return h.Key, nil
}

func checkBinding(relay string, nonce, exporter []byte) error {
	switch {
	case !ValidRelayID(relay):
		return fmt.Errorf("%w: relay id %q", ErrMalformed, relay)
	case len(nonce) != NonceSize:
		return fmt.Errorf("%w: nonce is %d bytes", ErrMalformed, len(nonce))
	case len(exporter) != ExporterSize:
		return fmt.Errorf("%w: exporter is %d bytes", ErrMalformed, len(exporter))
	}
	return nil
}

// Marshal encodes a challenge frame.
func (c Challenge) Marshal() ([]byte, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	return json.Marshal(challengeJSON{T: "challenge", V: Version, Relay: c.Relay, Nonce: b64.EncodeToString(c.Nonce)})
}

// ParseChallenge decodes and checks a challenge frame.
func ParseChallenge(b []byte) (Challenge, error) {
	var j challengeJSON
	if err := decode(b, &j); err != nil {
		return Challenge{}, err
	}
	if err := wantType(j.T, "challenge"); err != nil {
		return Challenge{}, err
	}
	if j.V != Version {
		return Challenge{}, fmt.Errorf("%w: v=%d", ErrVersion, j.V)
	}
	nonce, err := unb64(j.Nonce, NonceSize, "nonce")
	if err != nil {
		return Challenge{}, err
	}
	c := Challenge{Relay: j.Relay, Nonce: nonce}
	return c, c.check()
}

func (c Challenge) check() error {
	if !ValidRelayID(c.Relay) {
		return fmt.Errorf("%w: relay id %q", ErrMalformed, c.Relay)
	}
	if len(c.Nonce) != NonceSize {
		return fmt.Errorf("%w: nonce", ErrMalformed)
	}
	return nil
}

// Marshal encodes a hello frame.
func (h Hello) Marshal() ([]byte, error) {
	if err := h.check(); err != nil {
		return nil, err
	}
	j := helloJSON{
		T: "hello", V: Version,
		Key:           b64.EncodeToString(h.Key),
		Sig:           b64.EncodeToString(h.Sig),
		StatusKeyHash: b64.EncodeToString(h.StatusKeyHash),
		Client:        h.Client,
	}
	if h.Ent != "" {
		j.Ent = &h.Ent
	}
	return json.Marshal(j)
}

// ParseHello decodes and checks a hello frame. It does not verify the
// signature; see VerifyHello.
func ParseHello(b []byte) (Hello, error) {
	var j helloJSON
	if err := decode(b, &j); err != nil {
		return Hello{}, err
	}
	if err := wantType(j.T, "hello"); err != nil {
		return Hello{}, err
	}
	if j.V != Version {
		return Hello{}, fmt.Errorf("%w: v=%d", ErrVersion, j.V)
	}
	key, err := unb64(j.Key, ed25519.PublicKeySize, "key")
	if err != nil {
		return Hello{}, err
	}
	sig, err := unb64(j.Sig, ed25519.SignatureSize, "sig")
	if err != nil {
		return Hello{}, err
	}
	skh, err := unb64(j.StatusKeyHash, 32, "status_key_hash")
	if err != nil {
		return Hello{}, err
	}
	h := Hello{Key: key, Sig: sig, StatusKeyHash: skh, Client: j.Client}
	if j.Ent != nil {
		if *j.Ent == "" {
			return Hello{}, fmt.Errorf("%w: empty ent; send null", ErrMalformed)
		}
		h.Ent = *j.Ent
	}
	return h, h.check()
}

func (h Hello) check() error {
	switch {
	case len(h.Key) != ed25519.PublicKeySize:
		return fmt.Errorf("%w: key", ErrMalformed)
	case len(h.Sig) != ed25519.SignatureSize:
		return fmt.Errorf("%w: sig", ErrMalformed)
	case len(h.StatusKeyHash) != 32:
		return fmt.Errorf("%w: status_key_hash", ErrMalformed)
	case !printableASCII(h.Client, 1, maxClient):
		return fmt.Errorf("%w: client", ErrMalformed)
	case h.Ent != "" && !validToken(h.Ent):
		return fmt.Errorf("%w: ent", ErrMalformed)
	}
	return nil
}

// Marshal encodes a welcome frame.
func (w Welcome) Marshal() ([]byte, error) {
	if err := w.check(); err != nil {
		return nil, err
	}
	return json.Marshal(welcomeJSON{T: "welcome", Hostnames: w.Hostnames, Gen: w.Gen, Keepalive: w.Keepalive, MaxStreams: w.MaxStreams})
}

func (w Welcome) check() error {
	if len(w.Hostnames) == 0 || len(w.Hostnames) > MaxHostnames {
		return fmt.Errorf("%w: %d hostnames", ErrMalformed, len(w.Hostnames))
	}
	for _, h := range w.Hostnames {
		if !ValidHostname(h) {
			return fmt.Errorf("%w: hostname %q", ErrMalformed, h)
		}
	}
	switch {
	case w.Gen < 0:
		return fmt.Errorf("%w: gen", ErrMalformed)
	case w.Keepalive < 5 || w.Keepalive > 300:
		return fmt.Errorf("%w: keepalive %d", ErrMalformed, w.Keepalive)
	case w.MaxStreams < 1 || w.MaxStreams > 4096:
		return fmt.Errorf("%w: max_streams %d", ErrMalformed, w.MaxStreams)
	}
	return nil
}

// Marshal encodes an error frame.
func (e Error) Marshal() ([]byte, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	return json.Marshal(errorJSON{T: "error", Code: e.Code, Message: e.Message, RetryAfter: e.RetryAfter})
}

func (e Error) check() error {
	switch {
	case !validCode(e.Code):
		return fmt.Errorf("%w: code %q", ErrMalformed, e.Code)
	case !validText(e.Message):
		return fmt.Errorf("%w: message", ErrMalformed)
	case e.RetryAfter < 0 || e.RetryAfter > MaxRetryAfter:
		return fmt.Errorf("%w: retry_after", ErrMalformed)
	}
	return nil
}

// ParseReply decodes the relay's answer to a hello. A refusal comes back
// as an Error value in err.
func ParseReply(b []byte) (Welcome, error) {
	var t typeJSON
	if err := decode(b, &t); err != nil {
		return Welcome{}, err
	}
	switch t.T {
	case "welcome":
		var j welcomeJSON
		if err := decode(b, &j); err != nil {
			return Welcome{}, err
		}
		w := Welcome{Hostnames: j.Hostnames, Gen: j.Gen, Keepalive: j.Keepalive, MaxStreams: j.MaxStreams}
		return w, w.check()
	case "error":
		var j errorJSON
		if err := decode(b, &j); err != nil {
			return Welcome{}, err
		}
		e := Error{Code: j.Code, Message: j.Message, RetryAfter: j.RetryAfter}
		if err := e.check(); err != nil {
			return Welcome{}, err
		}
		return Welcome{}, e
	}
	return Welcome{}, fmt.Errorf("%w: reply type %q", ErrMalformed, t.T)
}

// decode parses one JSON object of at most MaxMessage bytes. JSON v2
// rejects duplicate names, invalid UTF-8 and trailing data; unknown names
// are ignored so a peer can add optional fields.
func decode(b []byte, v any) error {
	if len(b) > MaxMessage {
		return fmt.Errorf("%w: %d bytes", ErrMalformed, len(b))
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return nil
}

// wantType checks a message's "t".
func wantType(got, want string) error {
	if got != want {
		return fmt.Errorf("%w: got %q, want %q", ErrMalformed, got, want)
	}
	return nil
}

func unb64(s string, n int, field string) ([]byte, error) {
	if len(s) != b64.EncodedLen(n) {
		return nil, fmt.Errorf("%w: %s", ErrMalformed, field)
	}
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != n {
		return nil, fmt.Errorf("%w: %s", ErrMalformed, field)
	}
	return b, nil
}

// ValidRelayID reports whether s can name a relay: 1 to 32 characters of
// a-z, 0-9, '.', '_' or '-', starting with a letter or digit.
func ValidRelayID(s string) bool {
	if len(s) == 0 || len(s) > maxRelayID {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '.' || c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// ValidHostname reports whether s is a lower-case DNS name of LDH labels,
// as a relay announces it.
func ValidHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// validCode accepts snake_case identifiers: error codes and control types.
func validCode(s string) bool {
	if len(s) == 0 || len(s) > maxCode {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c == '_') {
			return false
		}
	}
	return true
}

// validText accepts what may be shown verbatim: printable UTF-8, with no
// control or formatting characters.
func validText(s string) bool {
	if len(s) > maxText || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func printableASCII(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// validToken accepts a v4.public PASETO in its URL-safe alphabet. The relay
// verifies it; this only bounds it.
func validToken(s string) bool {
	if len(s) > MaxEntitlement || !strings.HasPrefix(s, "v4.public.") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
