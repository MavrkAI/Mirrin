package wire

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// fixed binding inputs, so signatures in tests are reproducible.
var (
	testNonce    = bytes.Repeat([]byte{0x11}, NonceSize)
	testExporter = bytes.Repeat([]byte{0x22}, ExporterSize)
	testSKH      = sha256.New().Sum(nil)
)

func testKey(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
}

func signedHello(t testing.TB) Hello {
	t.Helper()
	h, err := SignHello(testKey(1), Challenge{Relay: "r1", Nonce: testNonce}, testExporter, "", testSKH, "mirrin/0.4.0 darwin/arm64")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHelloSigInputLayout(t *testing.T) {
	got := HelloSigInput("r1", []byte{0xAA, 0xBB}, []byte{0xCC})
	want := []byte("mirrin-relay-tunnel-v1\x00r1\x00\xAA\xBB\x00\xCC")
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// helloKAT is a regression vector, also printed in docs/relay-protocol.md:
// device key seed 32×0x01, relay "r1", nonce 32×0x11, exporter 32×0x22,
// status key 32×0x33. Ed25519 is deterministic, so any correct
// implementation produces exactly this frame.
const helloKAT = `{"t":"hello","v":1,"key":"iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w",` +
	`"sig":"JHW_KsJMf0dIlWMSfMpfx5k3SE39ulKh5lpYaGtY0Rfu8XD16nQR-6dxbnhj0HMQaTb5NJDe1qxYAKo8lYUTDA",` +
	`"ent":null,"status_key_hash":"3rDjjO0eQd5vkucOgMQY0tNWr6qpnib1k528fT70dyo","client":"mirrin/0.4.0 darwin/arm64"}`

func TestHelloKnownAnswer(t *testing.T) {
	skh := sha256.Sum256(bytes.Repeat([]byte{0x33}, 32))
	h, err := SignHello(testKey(1), Challenge{Relay: "r1", Nonce: testNonce}, testExporter, "", skh[:], "mirrin/0.4.0 darwin/arm64")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != helloKAT {
		t.Fatalf("hello frame:\n got %s\nwant %s", b, helloKAT)
	}
	parsed, err := ParseHello([]byte(helloKAT))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyHello(parsed, "r1", testNonce, testExporter); err != nil {
		t.Fatal(err)
	}
	wantInput := "6d697272696e2d72656c61792d74756e6e656c2d763100723100" + strings.Repeat("11", 32) + "00" + strings.Repeat("22", 32)
	if got := hex.EncodeToString(HelloSigInput("r1", testNonce, testExporter)); got != wantInput {
		t.Fatalf("signed input %s", got)
	}
}

func TestVerifyHello(t *testing.T) {
	h := signedHello(t)
	key, err := VerifyHello(h, "r1", testNonce, testExporter)
	if err != nil {
		t.Fatal(err)
	}
	if !key.Equal(testKey(1).Public()) {
		t.Fatal("VerifyHello returned the wrong key")
	}
}

// A hello proves possession of the key for one relay, one challenge and one
// TLS session, and nothing else.
func TestVerifyHelloRejectsOtherBindings(t *testing.T) {
	h := signedHello(t)
	flip := func(b []byte) []byte {
		c := bytes.Clone(b)
		c[len(c)-1] ^= 1
		return c
	}
	cases := []struct {
		name            string
		relay           string
		nonce, exporter []byte
		h               Hello
	}{
		{"other relay id", "r2", testNonce, testExporter, h},
		{"relay id prefix", "r", testNonce, testExporter, h},
		{"other nonce", "r1", flip(testNonce), testExporter, h},
		{"other exporter", "r1", testNonce, flip(testExporter), h},
		{"swapped nonce and exporter", "r1", testExporter, testNonce, h},
		{"other key", "r1", testNonce, testExporter, Hello{Key: testKey(2).Public().(ed25519.PublicKey), Sig: h.Sig, StatusKeyHash: h.StatusKeyHash, Client: h.Client}},
		{"tampered signature", "r1", testNonce, testExporter, Hello{Key: h.Key, Sig: flip(h.Sig), StatusKeyHash: h.StatusKeyHash, Client: h.Client}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := VerifyHello(c.h, c.relay, c.nonce, c.exporter); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("err = %v, want ErrBadSignature", err)
			}
		})
	}
	// Malformed bindings are refused before any signature check.
	for name, f := range map[string]func() error{
		"short nonce":    func() error { _, err := VerifyHello(h, "r1", testNonce[:31], testExporter); return err },
		"long exporter":  func() error { _, err := VerifyHello(h, "r1", testNonce, append(testExporter, 0)); return err },
		"NUL in relay":   func() error { _, err := VerifyHello(h, "r1\x00", testNonce, testExporter); return err },
		"empty relay id": func() error { _, err := VerifyHello(h, "", testNonce, testExporter); return err },
	} {
		if err := f(); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

func TestHelloRoundTrip(t *testing.T) {
	h := signedHello(t)
	b, err := h.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"ent":null`)) || !bytes.HasPrefix(b, []byte(`{"t":"hello","v":1,`)) {
		t.Fatalf("unexpected encoding: %s", b)
	}
	got, err := ParseHello(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, h) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, h)
	}
	h.Ent = "v4.public.eyJ4Ijp0cnVlfQ"
	b, _ = h.Marshal()
	if got, err = ParseHello(b); err != nil || got.Ent != h.Ent {
		t.Fatalf("ent round trip: %v %q", err, got.Ent)
	}
}

func TestParseHelloStrict(t *testing.T) {
	h := signedHello(t)
	good, _ := h.Marshal()
	key := b64.EncodeToString(h.Key)
	sub := func(old, new string) string { return strings.Replace(string(good), old, new, 1) }
	cases := map[string]string{
		"wrong type":          sub(`"t":"hello"`, `"t":"welcome"`),
		"wrong version":       sub(`"v":1`, `"v":2`),
		"duplicate name":      sub(`{"t":"hello",`, `{"t":"hello","t":"hello",`),
		"case-folded name":    sub(`"key"`, `"KEY"`),
		"padded key":          sub(key, key+"="),
		"std base64 alphabet": sub(key, "+"+key[1:]),
		"short key":           sub(key, key[:42]),
		"non-canonical key":   sub(key, key[:42]+"B"),
		"empty ent":           sub(`"ent":null`, `"ent":""`),
		"ent not a token":     sub(`"ent":null`, `"ent":"v2.local.xyz"`),
		"control in client":   sub(`mirrin/0.4.0`, `mirrin/0.4.0\n`),
		"empty client":        sub(`"mirrin/0.4.0 darwin/arm64"`, `""`),
		"trailing data":       string(good) + `{}`,
		"invalid UTF-8":       sub(`mirrin/`, "mirrin/\xff"),
		"not an object":       `[]`,
		"too large":           sub(`"client"`, `"pad":"`+strings.Repeat("x", MaxMessage)+`","client"`),
		"missing sig":         sub(`"sig"`, `"sgi"`),
		"null status hash":    sub(`"status_key_hash":"`+b64.EncodeToString(testSKH)+`"`, `"status_key_hash":null`),
		"oversized ent":       sub(`"ent":null`, `"ent":"v4.public.`+strings.Repeat("A", MaxEntitlement)+`"`),
		"number for version":  sub(`"v":1`, `"v":"1"`),
	}
	for name, in := range cases {
		if in == string(good) {
			t.Fatalf("%s: substitution did not apply", name)
		}
		_, err := ParseHello([]byte(in))
		if err == nil {
			t.Errorf("%s: accepted %s", name, in)
		}
		if name == "wrong version" && !errors.Is(err, ErrVersion) {
			t.Errorf("wrong version: err = %v", err)
		}
	}
	// Unknown members are ignored, so a newer daemon can add fields.
	if _, err := ParseHello([]byte(sub(`"client"`, `"extra":[1,2],"client"`))); err != nil {
		t.Errorf("unknown member: %v", err)
	}
}

func TestChallengeRoundTrip(t *testing.T) {
	ch, err := NewChallenge("r1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ch.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseChallenge(b)
	if err != nil || !reflect.DeepEqual(got, ch) {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	for _, in := range []string{
		`{"t":"challenge","v":1,"relay":"r1","nonce":"` + b64.EncodeToString(make([]byte, 31)) + `"}`,
		`{"t":"challenge","v":1,"relay":"R1","nonce":"` + b64.EncodeToString(make([]byte, 32)) + `"}`,
		`{"t":"challenge","v":1,"relay":"","nonce":"` + b64.EncodeToString(make([]byte, 32)) + `"}`,
		`{"t":"hello","v":1,"relay":"r1","nonce":"` + b64.EncodeToString(make([]byte, 32)) + `"}`,
	} {
		if _, err := ParseChallenge([]byte(in)); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
	if _, err := ParseChallenge([]byte(`{"t":"challenge","v":2,"relay":"r1","nonce":"` + b64.EncodeToString(make([]byte, 32)) + `"}`)); !errors.Is(err, ErrVersion) {
		t.Errorf("v2 challenge: %v", err)
	}
}

// The design's own examples parse.
func TestDesignExamples(t *testing.T) {
	w, err := ParseReply([]byte(`{"t":"welcome","hostnames":["ember-otter-42.mirrin.link"],"gen":3,"keepalive":25,"max_streams":64}`))
	if err != nil || w.Gen != 3 || w.Keepalive != 25 || w.MaxStreams != 64 || w.Hostnames[0] != "ember-otter-42.mirrin.link" {
		t.Fatalf("welcome: %v %+v", err, w)
	}
	_, err = ParseReply([]byte(`{"t":"error","code":"superseded_retry","message":"Another machine holds this name.","retry_after":3600}`))
	var e Error
	if !errors.As(err, &e) || e.Code != CodeSupersededRetry || e.RetryAfter != 3600 || e.Message != "Another machine holds this name." {
		t.Fatalf("error reply: %v", err)
	}
	for _, line := range []string{`{"t":"superseded","gen":4}`, `{"t":"drain","retry_after":30}`, `{"t":"notice","message":"Maintenance at 02:00 UTC."}`, `{"t":"limits","max_streams":32,"bps":20000000}`} {
		if _, err := ParseControl([]byte(line)); err != nil {
			t.Errorf("%s: %v", line, err)
		}
	}
}

func TestReplyStrict(t *testing.T) {
	for _, in := range []string{
		`{"t":"welcome","hostnames":[],"gen":1,"keepalive":25,"max_streams":64}`,
		`{"t":"welcome","hostnames":["UPPER.test"],"gen":1,"keepalive":25,"max_streams":64}`,
		`{"t":"welcome","hostnames":["-bad.test"],"gen":1,"keepalive":25,"max_streams":64}`,
		`{"t":"welcome","hostnames":["h.test"],"gen":-1,"keepalive":25,"max_streams":64}`,
		`{"t":"welcome","hostnames":["h.test"],"gen":1,"keepalive":0,"max_streams":64}`,
		`{"t":"welcome","hostnames":["h.test"],"gen":1,"keepalive":25,"max_streams":0}`,
		`{"t":"welcome","hostnames":["h.test"],"gen":1.5,"keepalive":25,"max_streams":64}`,
		`{"t":"error","code":"Bad-Code","message":""}`,
		`{"t":"error","code":"denied","message":"a\u202eb"}`,
		`{"t":"error","code":"denied","message":"ok","retry_after":-1}`,
		`{"t":"error","code":"denied","message":"ok","retry_after":86401}`,
		`{"t":"challenge"}`,
		`{}`,
	} {
		if _, err := ParseReply([]byte(in)); err == nil {
			t.Errorf("accepted %s", in)
		} else {
			var e Error
			if errors.As(err, &e) {
				t.Errorf("%s: malformed reply surfaced as a refusal: %v", in, err)
			}
		}
	}
}

func TestWelcomeErrorMarshal(t *testing.T) {
	w := Welcome{Hostnames: []string{"h.test"}, Gen: 1, Keepalive: 25, MaxStreams: 64}
	b, err := w.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseReply(b); err != nil || !reflect.DeepEqual(got, w) {
		t.Fatalf("welcome round trip: %v %+v", err, got)
	}
	e := Error{Code: CodeDenied, Message: "This name is under review.", RetryAfter: 60}
	b, err = e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var got Error
	if _, err := ParseReply(b); !errors.As(err, &got) || got != e {
		t.Fatalf("error round trip: %v", err)
	}
	if _, err := (Welcome{Hostnames: []string{"h.test"}, Keepalive: 25}).Marshal(); err == nil {
		t.Error("marshalled a welcome with max_streams 0")
	}
}

func TestControlReader(t *testing.T) {
	var buf bytes.Buffer
	msgs := []Control{
		{T: ControlNotice, Message: "hello"},
		{T: ControlLimits, MaxStreams: 32, BPS: 20e6},
		{T: ControlSuperseded, Gen: 4},
		{T: "future_thing"},
	}
	for _, m := range msgs {
		if err := WriteControl(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	cr := NewControlReader(&buf)
	for _, want := range msgs {
		got, err := cr.Next()
		if err != nil || got != want {
			t.Fatalf("got %+v %v, want %+v", got, err, want)
		}
	}
	if _, err := cr.Next(); err != io.EOF {
		t.Fatalf("end: %v", err)
	}

	long := `{"t":"notice","message":"` + strings.Repeat("a", MaxControlLine) + "\"}\n"
	if _, err := NewControlReader(strings.NewReader(long)).Next(); !errors.Is(err, ErrMalformed) {
		t.Errorf("long line: %v", err)
	}
	if _, err := NewControlReader(strings.NewReader(`{"t":"notice"}`)).Next(); err != io.ErrUnexpectedEOF {
		t.Errorf("unterminated line: %v", err)
	}
	if _, err := NewControlReader(strings.NewReader("\n")).Next(); err == nil {
		t.Error("empty line accepted")
	}
	// A line of exactly MaxControlLine bytes is allowed.
	pad := MaxControlLine - len(`{"t":"notice","pad":""}`)
	exact := `{"t":"notice","pad":"` + strings.Repeat("a", pad) + "\"}\n"
	if _, err := NewControlReader(strings.NewReader(exact)).Next(); err != nil {
		t.Errorf("line of exactly MaxControlLine: %v", err)
	}
}

func TestValidators(t *testing.T) {
	for s, want := range map[string]bool{
		"r1": true, "relay.eu-1": true, "r_2": true, "": false, "R1": false, "-r": false,
		"r 1": false, strings.Repeat("a", 32): true, strings.Repeat("a", 33): false,
	} {
		if ValidRelayID(s) != want {
			t.Errorf("ValidRelayID(%q) = %v", s, !want)
		}
	}
	for s, want := range map[string]bool{
		"ember-otter-42.mirrin.link": true, "localhost": true, "h.test": true,
		"H.test": false, "a..b": false, "a.": false, "-a.b": false, "a-.b": false, "a_b.c": false,
		strings.Repeat("a", 63) + ".b": true, strings.Repeat("a", 64) + ".b": false,
		strings.Repeat("abcdefg.", 32): false,
	} {
		if ValidHostname(s) != want {
			t.Errorf("ValidHostname(%q) = %v", s, !want)
		}
	}
}

func TestMaxEntitlementMatchesEntitle(t *testing.T) {
	if MaxEntitlement != entitle.MaxTokenSize {
		t.Fatalf("MaxEntitlement %d != entitle.MaxTokenSize %d", MaxEntitlement, entitle.MaxTokenSize)
	}
}

// Both ends of a real TLS 1.3 session derive the same exporter, and it
// differs between sessions; TLS 1.2 is refused.
func TestExporter(t *testing.T) {
	cert := selfSigned(t)
	pair := func(max uint16) (tls.ConnectionState, tls.ConnectionState, error) {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		srv := tls.Server(a, &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: max})
		cli := tls.Client(b, &tls.Config{InsecureSkipVerify: true, MaxVersion: max})
		errc := make(chan error, 1)
		go func() { errc <- srv.Handshake() }()
		if err := cli.Handshake(); err != nil {
			return tls.ConnectionState{}, tls.ConnectionState{}, err
		}
		if err := <-errc; err != nil {
			return tls.ConnectionState{}, tls.ConnectionState{}, err
		}
		return cli.ConnectionState(), srv.ConnectionState(), nil
	}
	c1, s1, err := pair(tls.VersionTLS13)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := Exporter(c1)
	if err != nil {
		t.Fatal(err)
	}
	es, err := Exporter(s1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ec, es) || len(ec) != ExporterSize {
		t.Fatalf("client and server exporters differ: %x %x", ec, es)
	}
	// The label and length as docs/relay-protocol.md §3.3 gives them,
	// spelled out so that an edit to the constant breaks this test.
	if want, err := c1.ExportKeyingMaterial("EXPORTER-mirrin-tunnel", nil, 32); err != nil || !bytes.Equal(ec, want) {
		t.Fatalf("exporter is not EXPORTER-mirrin-tunnel, 32 bytes: %x, want %x (%v)", ec, want, err)
	}
	c2, _, err := pair(tls.VersionTLS13)
	if err != nil {
		t.Fatal(err)
	}
	if e2, _ := Exporter(c2); bytes.Equal(ec, e2) {
		t.Fatal("two sessions share an exporter")
	}
	c3, _, err := pair(tls.VersionTLS12)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Exporter(c3); err == nil {
		t.Fatal("TLS 1.2 exporter accepted")
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay.test"},
		DNSNames:     []string{"relay.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}
