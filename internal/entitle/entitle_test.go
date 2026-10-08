package entitle

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// vector is one entry of the official test-vector files.
type vector struct {
	Name       string  `json:"name"`
	ExpectFail bool    `json:"expect-fail"`
	PublicKey  string  `json:"public-key"`
	SecretKey  string  `json:"secret-key"`
	Key        string  `json:"key"`
	Token      string  `json:"token"`
	Payload    *string `json:"payload"`
	Footer     string  `json:"footer"`
	Implicit   string  `json:"implicit-assertion"`
}

func loadVectors(t testing.TB, name string) []vector {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Tests []vector `json:"tests"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Tests
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// devPriv derives a private key by the dev-key recipe in keys.go.
func devPriv(kid string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("mirrin dev key " + kid))
	return ed25519.NewKeyFromSeed(seed[:])
}

func devPub(kid string) ed25519.PublicKey { return devPriv(kid).Public().(ed25519.PublicKey) }

// Tests sign and verify with their own keys, not the compiled-in ones, so
// putting the launch keys into keys.go breaks nothing here.
var (
	testEnt = map[string]ed25519.PublicKey{"ent-test-a": devPub("ent-test-a"), "ent-test-b": devPub("ent-test-b")}
	testDL  = map[string]ed25519.PublicKey{"dl-test-a": devPub("dl-test-a")}
)

func TestVectorFilesAreVerbatim(t *testing.T) {
	for name, want := range map[string]string{
		"paseto-v4-public.json": "0b72948b65d1f73f574c9ad2aa3481ec27bf8c632f5f6e1596cd41f5b9703387",
		"paseto-v3.json":        "acc983640edf3ec6115aaeadf3c15a1556aac87f8300b5e8cbd264da7b996cbe",
	} {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(sha256Sum(b)); got != want {
			t.Errorf("%s changed: sha256 %s, want %s (see testdata/SOURCE.txt)", name, got, want)
		}
	}
}

func sha256Sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

// The examples from the spec's Common.md.
func TestPAE(t *testing.T) {
	for _, c := range []struct {
		in   [][]byte
		want string
	}{
		{nil, "\x00\x00\x00\x00\x00\x00\x00\x00"},
		{[][]byte{{}}, "\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"},
		{[][]byte{[]byte("test")}, "\x01\x00\x00\x00\x00\x00\x00\x00\x04\x00\x00\x00\x00\x00\x00\x00test"},
	} {
		if got := pae(c.in...); string(got) != c.want {
			t.Errorf("PAE(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := le64(nil, -1); got[7] != 0x7f {
		t.Errorf("LE64 must clear the top bit: %x", got)
	}
}

func TestOfficialPublicVectors(t *testing.T) {
	var ran int
	for _, v := range loadVectors(t, "paseto-v4-public.json") {
		if !strings.HasPrefix(v.Name, "4-S-") {
			continue
		}
		ran++
		t.Run(v.Name, func(t *testing.T) {
			if v.ExpectFail {
				t.Fatal("4-S vectors are expected to pass")
			}
			pub := ed25519.PublicKey(unhex(t, v.PublicKey))
			m, sig, f, err := parseV4(v.Token, MaxTokenSize)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyV4(pub, m, sig, f, []byte(v.Implicit)); err != nil {
				t.Fatal(err)
			}
			if string(m) != *v.Payload || string(f) != v.Footer {
				t.Fatalf("payload %q footer %q", m, f)
			}
			// Ed25519 is deterministic, so signing reproduces the token.
			tok, err := signV4(ed25519.PrivateKey(unhex(t, v.SecretKey)), []byte(*v.Payload), []byte(v.Footer), []byte(v.Implicit), MaxTokenSize)
			if err != nil || tok != v.Token {
				t.Fatalf("sign = %q, %v\nwant  %q", tok, err, v.Token)
			}
			// The implicit assertion is bound: dropping or changing it fails.
			if err := verifyV4(pub, m, sig, f, []byte(v.Implicit+"x")); err == nil {
				t.Fatal("wrong implicit assertion verified")
			}
			bad := bytes.Clone(m)
			bad[0] ^= 1
			if err := verifyV4(pub, bad, sig, f, []byte(v.Implicit)); err == nil {
				t.Fatal("tampered payload verified")
			}
		})
	}
	if ran != 3 {
		t.Fatalf("ran %d 4-S vectors, want 3", ran)
	}
}

// The 4-F vectors must fail. Those with a public key are tried with it; those
// with a symmetric key are tried with those bytes posing as a public key (the
// algorithm-confusion case) and with the 4-S key.
func TestOfficialFailureVectors(t *testing.T) {
	sPub := ed25519.PublicKey(unhex(t, "1eb9dbbbbc047c03fd70604e0071f0987e16b28b757225c11f00415d0e20b1a2"))
	var ran int
	for _, v := range loadVectors(t, "paseto-v4-public.json") {
		if !strings.HasPrefix(v.Name, "4-F-") {
			continue
		}
		ran++
		t.Run(v.Name, func(t *testing.T) {
			if !v.ExpectFail {
				t.Fatal("4-F vectors are expected to fail")
			}
			keys := []ed25519.PublicKey{sPub}
			if v.PublicKey != "" {
				keys = append(keys, unhex(t, v.PublicKey))
			}
			if v.Key != "" {
				keys = append(keys, unhex(t, v.Key))
			}
			for _, k := range keys {
				m, sig, f, err := parseV4(v.Token, MaxTokenSize)
				if err == nil {
					err = verifyV4(k, m, sig, f, []byte(v.Implicit))
				}
				if err == nil {
					t.Fatalf("accepted with key %x", k)
				}
				ks := map[string]ed25519.PublicKey{"ent-x": k, "dl-x": k}
				if _, err := Verify(v.Token, ks, time.Now()); err == nil {
					t.Fatal("Verify accepted it")
				}
				if _, err := VerifyDenyList(v.Token, ks, time.Now()); err == nil {
					t.Fatal("VerifyDenyList accepted it")
				}
			}
		})
	}
	if ran != 5 {
		t.Fatalf("ran %d 4-F vectors, want 5", ran)
	}
}

// Every v4.local token (4-E) and every v3 token is refused by its header,
// before any decoding.
func TestLocalAndV3Rejected(t *testing.T) {
	var n int
	for _, file := range []string{"paseto-v4-public.json", "paseto-v3.json"} {
		for _, v := range loadVectors(t, file) {
			if strings.HasPrefix(v.Token, "v4.public.") {
				continue
			}
			n++
			if _, _, _, err := parseV4(v.Token, MaxTokenSize); !errors.Is(err, ErrUnsupported) {
				t.Errorf("%s: parse err = %v, want ErrUnsupported", v.Name, err)
			}
			if _, err := Verify(v.Token, testEnt, time.Now()); !errors.Is(err, ErrUnsupported) {
				t.Errorf("%s: Verify err = %v", v.Name, err)
			}
			if _, err := VerifyDenyList(v.Token, testDL, time.Now()); !errors.Is(err, ErrUnsupported) {
				t.Errorf("%s: VerifyDenyList err = %v", v.Name, err)
			}
		}
	}
	// 9 4-E, 3 v4.local 4-F, one v3.local 4-F, and all 17 v3 vectors.
	if n != 30 {
		t.Fatalf("checked %d non-v4.public tokens, want 30", n)
	}
}

var t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func claims(t testing.TB) Claims {
	t.Helper()
	return Claims{
		Iss: "cloud.mirrin.app", Sub: "acct_1", Aud: Audience,
		Iat: t0, Nbf: t0, Exp: t0.Add(35 * 24 * time.Hour), PaidThrough: t0.Add(30 * 24 * time.Hour),
		Gen: 3, Plan: "cloud", Feat: []string{"reach", "backup"},
		Handle: "ember-otter-42", Hosts: []string{"ember-otter-42.mirrin.link"},
		Cnf:         EncodeKey(devPriv("device").Public().(ed25519.PublicKey)),
		Relays:      []Relay{{ID: "r1", URL: "wss://r1.relay.mirrin.app/v1/tunnel", IPs: []string{"192.0.2.1", "2001:db8::1"}}},
		BackupQuota: 21474836480, WakeCredits: 0,
	}
}

func TestEntitlementRoundTrip(t *testing.T) {
	c := claims(t)
	tok, err := Sign(c, "ent-test-a", devPriv("ent-test-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "v4.public.") || !strings.HasSuffix(tok, "."+b64.EncodeToString([]byte(`{"kid":"ent-test-a"}`))) {
		t.Fatalf("token shape: %s", tok)
	}
	got, err := Verify(tok, testEnt, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, c)
	}
	if k, err := got.Key(); err != nil || !k.Equal(devPriv("device").Public()) {
		t.Fatalf("Key() = %v, %v", k, err)
	}
	if !got.Has("reach") || got.Has("wake") || !got.Covers([]string{"ember-otter-42.mirrin.link"}) || got.Covers([]string{"other.mirrin.link"}) {
		t.Fatal("Has/Covers")
	}
	// The next key works too.
	tok2, _ := Sign(c, "ent-test-b", devPriv("ent-test-b"))
	if _, err := Verify(tok2, testEnt, t0); err != nil {
		t.Fatal(err)
	}
	// The payload is the documented wire format.
	m, _, _, _ := parseV4(tok, MaxTokenSize)
	var wire map[string]any
	if err := json.Unmarshal(m, &wire); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"iss", "sub", "aud", "iat", "nbf", "exp", "gen", "plan", "feat", "handle", "hosts", "cnf", "relays", "bq", "wk", "paid_through"} {
		if _, ok := wire[k]; !ok {
			t.Errorf("payload lacks %q", k)
		}
	}
	if wire["exp"] != "2026-11-01T12:00:00Z" {
		t.Errorf("exp = %v", wire["exp"])
	}
}

func TestEntitlementClock(t *testing.T) {
	c := claims(t)
	c.Nbf = t0.Add(10 * time.Minute)
	tok, err := Sign(c, "ent-test-a", devPriv("ent-test-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(tok, testEnt, t0); !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("early: %v", err)
	}
	if _, err := Verify(tok, testEnt, t0.Add(6*time.Minute)); err != nil {
		t.Fatalf("within leeway: %v", err)
	}
	if _, err := Verify(tok, testEnt, c.Exp.Add(-time.Second)); err != nil {
		t.Fatalf("last second: %v", err)
	}
	if _, err := Verify(tok, testEnt, c.Exp); !errors.Is(err, ErrExpired) {
		t.Fatalf("at exp: %v", err)
	}
	if got, err := Inspect(tok, testEnt); err != nil || got.Gen != 3 {
		t.Fatalf("Inspect of an expired token: %v", err)
	}

	// iat is checked on its own: issued ten minutes ahead of us is too far,
	// even though nbf is now.
	c = claims(t)
	c.Iat = t0.Add(10 * time.Minute)
	tok, err = Sign(c, "ent-test-a", devPriv("ent-test-a"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Verify(tok, testEnt, t0)
	wantErr(t, "iat ahead", err, ErrNotYetValid, "")
	if _, err := Verify(tok, testEnt, t0.Add(5*time.Minute)); err != nil {
		t.Fatalf("iat within leeway: %v", err)
	}

	// nbf inside the leeway but after exp: the clock checks alone would let
	// it through, so the shape check must not.
	now := t0.Add(time.Hour)
	tok = edited(t, `"nbf":"2026-09-27T12:00:00Z"`, `"nbf":"2026-09-27T13:02:00Z"`, `"exp":"2026-11-01T12:00:00Z"`, `"exp":"2026-09-27T13:01:00Z"`)
	_, err = Verify(tok, testEnt, now)
	wantErr(t, "nbf after exp inside the leeway", err, ErrMalformed, "nbf is not before exp")
	// nbf equal to exp is empty, and refused too.
	tok = edited(t, `"nbf":"2026-09-27T12:00:00Z"`, `"nbf":"2026-11-01T12:00:00Z"`)
	_, err = Verify(tok, testEnt, now)
	wantErr(t, "nbf at exp", err, ErrMalformed, "nbf is not before exp")
}

func TestTokensOver8KiBRejected(t *testing.T) {
	// A validly signed entitlement just over the limit.
	c := claims(t)
	for len(c.Hosts) < 300 {
		c.Hosts = append(c.Hosts, "h"+strings.Repeat("x", 20)+".mirrin.link")
	}
	if _, err := Sign(c, "ent-test-a", devPriv("ent-test-a")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Sign of a large entitlement: %v", err)
	}
	payload, _ := json.Marshal(c) // any payload; the size check comes first
	big, err := seal(payload, entitlementPrefix, "ent-test-a", devPriv("ent-test-a"), 1<<20)
	if err != nil || len(big) <= MaxTokenSize {
		t.Fatalf("setup: %d bytes, %v", len(big), err)
	}
	if _, err := Verify(big, testEnt, t0); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := Verify(strings.Repeat("A", MaxTokenSize+1), testEnt, t0); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("junk: %v", err)
	}
	// Exactly at the limit is judged on its merits, not refused for size.
	edge := "v4.public." + strings.Repeat("A", MaxTokenSize-len("v4.public."))
	if _, err := Verify(edge, testEnt, t0); errors.Is(err, ErrTooLarge) {
		t.Fatal("8 KiB exactly was refused for size")
	}
}

// wantErr checks that err is want for the intended reason: its message
// contains msg or, when msg is empty, is want's own message and nothing more.
func wantErr(t testing.TB, name string, err, want error, msg string) {
	t.Helper()
	switch {
	case !errors.Is(err, want):
		t.Errorf("%s: err = %v, want %v", name, err, want)
	case msg == "" && err.Error() != want.Error():
		t.Errorf("%s: err = %q, want exactly %q", name, err, want)
	case !strings.Contains(err.Error(), msg):
		t.Errorf("%s: err = %q, want it to say %q", name, err, msg)
	}
}

// entFooter is the footer Sign writes for ent-test-a.
const entFooter = `{"kid":"ent-test-a"}`

// edited signs the payload of a valid entitlement with each from/to pair
// replaced, which Sign itself would refuse to do.
func edited(t testing.TB, pairs ...string) string {
	t.Helper()
	good, err := Sign(claims(t), "ent-test-a", devPriv("ent-test-a"))
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, _ := parseV4(good, MaxTokenSize)
	payload := string(m)
	for i := 0; i < len(pairs); i += 2 {
		if !strings.Contains(payload, pairs[i]) {
			t.Fatalf("payload lacks %q", pairs[i])
		}
		payload = strings.Replace(payload, pairs[i], pairs[i+1], 1)
	}
	return resigned(t, payload, entFooter)
}

// resigned signs payload and footer with ent-test-a's key.
func resigned(t testing.TB, payload, footer string) string {
	t.Helper()
	tok, err := signV4(devPriv("ent-test-a"), []byte(payload), []byte(footer), nil, MaxTokenSize)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// nonCanonical sets a trailing bit that unpadded base64url must leave zero.
// s must end in a partial group.
func nonCanonical(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	return s[:len(s)-1] + string(alphabet[strings.IndexByte(alphabet, s[len(s)-1])|1])
}

func TestEntitlementRejects(t *testing.T) {
	priv := devPriv("ent-test-a")
	good, err := Sign(claims(t), "ent-test-a", priv)
	if err != nil {
		t.Fatal(err)
	}
	body, foot, _ := strings.Cut(strings.TrimPrefix(good, "v4.public."), ".")
	m, _, _, _ := parseV4(good, MaxTokenSize)
	payload := string(m)
	if len(foot)%4 == 0 {
		t.Fatal("setup: the footer has no partial group")
	}
	long := func(n int) string { return `{"kid":"ent-test-a"` + strings.Repeat(" ", n-len(entFooter)) + `}` }
	const (
		unpaddedBody = "payload is not unpadded base64url"
		unpaddedFoot = "footer is not unpadded base64url"
		someTime     = "iat, nbf, exp and paid_through are required"
		feat         = "empty feature or host"
		relay        = "relay without id or url"
	)
	for name, c := range map[string]struct {
		tok  string
		want error
		msg  string // "" means want itself, with no detail
	}{
		"padding":                {good + "=", ErrMalformed, unpaddedFoot},
		"newline in body":        {"v4.public." + body[:10] + "\n" + body[10:] + "." + foot, ErrMalformed, unpaddedBody},
		"trailing period":        {"v4.public." + body + ".", ErrMalformed, unpaddedFoot},
		"no footer":              {"v4.public." + body, ErrMalformed, "no key id"},
		"two footers":            {good + "." + foot, ErrMalformed, unpaddedFoot},
		"v4.public no body":      {"v4.public.", ErrMalformed, unpaddedBody},
		"no room for signature":  {"v4.public." + b64.EncodeToString(make([]byte, 63)) + "." + foot, ErrMalformed, "too short to hold a signature"},
		"payload non-canonical":  {"v4.public." + nonCanonical(b64.EncodeToString(make([]byte, 65))) + "." + foot, ErrMalformed, "payload is not canonical base64url"},
		"footer non-canonical":   {"v4.public." + body + "." + nonCanonical(foot), ErrMalformed, "footer is not canonical base64url"},
		"uppercase header":       {"V4.public." + body + "." + foot, ErrUnsupported, ""},
		"swapped footer":         {"v4.public." + body + "." + b64.EncodeToString([]byte(`{"kid":"ent-test-b"}`)), ErrSignature, ""},
		"footer not json":        {resigned(t, payload, "ent-test-a"), ErrMalformed, "footer: jsontext: invalid character"},
		"footer extra":           {resigned(t, payload, `{"kid":"ent-test-a","x":1}`), ErrMalformed, `unknown object member name "x"`},
		"footer dup kid":         {resigned(t, payload, `{"kid":"ent-test-b","kid":"ent-test-a"}`), ErrMalformed, `duplicate object member name "kid"`},
		"footer over 128 bytes":  {resigned(t, payload, long(129)), ErrMalformed, "footer too long"},
		"kid uppercase":          {resigned(t, payload, `{"kid":"ent-DEV-a"}`), ErrMalformed, `bad key id "ent-DEV-a"`},
		"unknown kid":            {resigned(t, payload, `{"kid":"ent-2031z"}`), ErrSignature, `unknown key "ent-2031z"`},
		"unknown claim":          {edited(t, `"iss":`, `"admin":true,"iss":`), ErrMalformed, `unknown object member name "admin"`},
		"duplicate claim":        {edited(t, `"gen":3`, `"gen":3,"gen":99`), ErrMalformed, `duplicate object member name "gen"`},
		"case-folded claim":      {edited(t, `"gen":`, `"GEN":`), ErrMalformed, `unknown object member name "GEN"`},
		"trailing data":          {resigned(t, payload+" {}", entFooter), ErrMalformed, "after top-level value"},
		"payload not object":     {resigned(t, `[]`, entFooter), ErrMalformed, "unmarshal JSON array"}, // json/v2 says "cannot" or "unable to"
		"wrong aud":              {edited(t, `"aud":"mirrin"`, `"aud":"other"`), ErrMalformed, "aud is not mirrin"},
		"gen zero":               {edited(t, `"gen":3`, `"gen":0`), ErrMalformed, "gen starts at 1"},
		"lowercase z":            {edited(t, `"iat":"2026-09-27T12:00:00Z"`, `"iat":"2026-09-27T12:00:00z"`), ErrMalformed, "iat is not an RFC 3339 time"},
		"unix time":              {edited(t, `"iat":"2026-09-27T12:00:00Z"`, `"iat":1790510400`), ErrMalformed, `within "/iat"`},
		"year zero exp":          {edited(t, `"exp":"2026-11-01T12:00:00Z"`, `"exp":"0000-01-01T00:00:00Z"`), ErrMalformed, someTime},
		"bad cnf":                {edited(t, `"cnf":"`, `"cnf":"x`), ErrMalformed, "cnf is not a public key"},
		"missing iss":            {edited(t, `"iss":"cloud.mirrin.app"`, `"iss":""`), ErrMalformed, "iss and sub are required"},
		"missing sub":            {edited(t, `"sub":"acct_1"`, `"sub":""`), ErrMalformed, "iss and sub are required"},
		"nbf after exp":          {edited(t, `"nbf":"2026-09-27T12:00:00Z"`, `"nbf":"2027-09-27T12:00:00Z"`), ErrMalformed, "nbf is not before exp"},
		"negative bq":            {edited(t, `"bq":21474836480`, `"bq":-1`), ErrMalformed, "negative quota"},
		"negative wk":            {edited(t, `"wk":0`, `"wk":-1`), ErrMalformed, "negative quota"},
		"empty feature":          {edited(t, `"feat":["reach","backup"]`, `"feat":["reach",""]`), ErrMalformed, feat},
		"empty host":             {edited(t, `"hosts":["ember-otter-42.mirrin.link"]`, `"hosts":[""]`), ErrMalformed, feat},
		"relay without id":       {edited(t, `"id":"r1"`, `"id":""`), ErrMalformed, relay},
		"relay without url":      {edited(t, `"url":"wss://r1.relay.mirrin.app/v1/tunnel"`, `"url":""`), ErrMalformed, relay},
		"relay member unknown":   {edited(t, `"id":"r1"`, `"id":"r1","key":"x"`), ErrMalformed, `unknown object member name "key"`},
		"string where list goes": {edited(t, `"feat":["reach","backup"]`, `"feat":"reach"`), ErrMalformed, `within "/feat"`},
	} {
		_, err := Verify(c.tok, testEnt, t0.Add(time.Hour))
		wantErr(t, name, err, c.want, c.msg)
	}
	if _, err := Verify(good, testEnt, t0.Add(time.Hour)); err != nil {
		t.Fatalf("control: %v", err)
	}
	// A footer of exactly 128 bytes is judged on its merits.
	if _, err := Verify(resigned(t, payload, long(128)), testEnt, t0.Add(time.Hour)); err != nil {
		t.Fatalf("128-byte footer: %v", err)
	}
	// A private key of the wrong size is an error, not a panic.
	if _, err := Sign(claims(t), "ent-test-a", priv[:32]); err == nil {
		t.Error("short private key accepted")
	}
	_, err = Verify(good, map[string]ed25519.PublicKey{"ent-test-a": {1, 2, 3}}, t0)
	wantErr(t, "short public key", err, ErrSignature, "")
}

// A deny list signed under an entitlement kid is refused, and the reverse,
// even when one map holds both sets of keys.
func TestDomainSeparation(t *testing.T) {
	both := maps.Clone(testEnt)
	maps.Copy(both, testDL)
	ent, dl := devPriv("ent-test-a"), devPriv("dl-test-a")

	if _, err := SignDenyList(denyList(), "ent-test-a", ent); !errors.Is(err, ErrPurpose) {
		t.Fatalf("SignDenyList under an ent kid: %v", err)
	}
	if _, err := Sign(claims(t), "dl-test-a", dl); !errors.Is(err, ErrPurpose) {
		t.Fatalf("Sign under a dl kid: %v", err)
	}

	entTok, _ := Sign(claims(t), "ent-test-a", ent)
	dlTok, err := SignDenyList(denyList(), "dl-test-a", dl)
	if err != nil {
		t.Fatal(err)
	}
	dlPayload, _, _, _ := parseV4(dlTok, MaxDenyListSize)
	entPayload, _, _, _ := parseV4(entTok, MaxTokenSize)

	// Forgeries by someone holding the other purpose's private key.
	dlUnderEnt, _ := seal(dlPayload, entitlementPrefix, "ent-test-a", ent, MaxDenyListSize)
	entUnderDl, _ := seal(entPayload, denyListPrefix, "dl-test-a", dl, MaxTokenSize)
	dlRelabelled, _ := signV4(ent, dlPayload, []byte(`{"kid":"dl-test-a"}`), nil, MaxDenyListSize)
	entRelabelled, _ := signV4(dl, entPayload, []byte(`{"kid":"ent-test-a"}`), nil, MaxTokenSize)

	for name, c := range map[string]struct {
		err  error
		want error
	}{
		"deny list under ent kid":         {second(VerifyDenyList(dlUnderEnt, both, t0)), ErrPurpose},
		"entitlement as deny list":        {second(VerifyDenyList(entTok, both, t0)), ErrPurpose},
		"deny list ent key, dl kid":       {second(VerifyDenyList(dlRelabelled, testDL, t0)), ErrSignature},
		"entitlement under dl kid":        {second(Verify(entUnderDl, both, t0)), ErrPurpose},
		"deny list as entitlement":        {second(Verify(dlTok, both, t0)), ErrPurpose},
		"entitlement dl key, ent kid":     {second(Verify(entRelabelled, testEnt, t0)), ErrSignature},
		"entitlement under dl kid, dlkey": {second(VerifyDenyList(entUnderDl, testDL, t0)), ErrMalformed},
	} {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, c.err, c.want)
		}
	}
	// And the honest tokens still verify.
	if _, err := Verify(entTok, testEnt, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDenyList(dlTok, testDL, t0); err != nil {
		t.Fatal(err)
	}
}

func second[T any](_ T, err error) error { return err }
