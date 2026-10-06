package httpsig

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

const skew = 300 * time.Second

func testKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("httpsig test key " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

func idOf(k ed25519.PrivateKey) string { return KeyID(k.Public().(ed25519.PublicKey)) }

func lookupOf(keys ...ed25519.PrivateKey) func(string) (ed25519.PublicKey, error) {
	m := map[string]ed25519.PublicKey{}
	for _, k := range keys {
		pub := k.Public().(ed25519.PublicKey)
		m[KeyID(pub)] = pub
	}
	return func(id string) (ed25519.PublicKey, error) {
		if k, ok := m[id]; ok {
			return k, nil
		}
		return nil, errors.New("unknown device")
	}
}

// signed returns a signed request as a client would send it.
func signed(t testing.TB, method, url, body string, priv ed25519.PrivateKey, at time.Time) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if err := Sign(r, idOf(priv), priv, at); err != nil {
		t.Fatal(err)
	}
	return r
}

// profileParams are the parameters Sign would use at t0, with a fresh nonce.
func profileParams(priv ed25519.PrivateKey) []param {
	n := make([]byte, 16)
	rand.Read(n)
	return []param{{"created", t0.Unix()}, {"expires", t0.Unix() + 300}, {"nonce", b64url.EncodeToString(n)}, {"keyid", idOf(priv)}, {"tag", Tag}}
}

func components(names ...string) []item {
	var out []item
	for _, n := range names {
		out = append(out, item{v: n})
	}
	return out
}

// signRaw adds a signature under label to r, over whatever components and
// parameters it is given, so tests can make signatures that verify but are
// off the profile. Components the base refuses get a signature over nothing,
// so Verify still finds one to judge.
func signRaw(t testing.TB, r *http.Request, label string, priv ed25519.PrivateKey, comps []item, ps []param) {
	t.Helper()
	o, err := clientOrigin(r)
	if err != nil {
		t.Fatal(err)
	}
	base, err := signatureBase(r, o, signature{label: label, components: comps, params: ps})
	if err != nil {
		base = ""
	}
	r.Header.Add("Signature-Input", serializeDictionary([]member{{key: label, list: comps, item: item{params: ps}, isList: true}}))
	r.Header.Add("Signature", serializeDictionary([]member{{key: label, item: item{v: ed25519.Sign(priv, []byte(base))}}}))
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

func TestKeyID(t *testing.T) {
	id := idOf(testKey("a"))
	if !strings.HasPrefix(id, "dev:") || len(id) != 4+22 {
		t.Fatalf("KeyID = %q", id)
	}
	if idOf(testKey("b")) == id {
		t.Fatal("two keys, one id")
	}
}

func TestSignedRequestShape(t *testing.T) {
	priv := testKey("a")
	r := signed(t, "POST", "https://cloud.mirrin.app/v1/link/start", `{"device_pub":"x"}`, priv, t0)
	in := r.Header.Get("Signature-Input")
	want := fmt.Sprintf(`sig1=("@method" "@target-uri" "content-digest");created=%d;expires=%d;nonce="`, t0.Unix(), t0.Unix()+300)
	if !strings.HasPrefix(in, want) || !strings.HasSuffix(in, `";keyid="`+idOf(priv)+`";tag="antbot-cloud-v1"`) {
		t.Fatalf("Signature-Input: %s", in)
	}
	if d := r.Header.Get("Content-Digest"); !strings.HasPrefix(d, "sha-256=:") {
		t.Fatalf("Content-Digest: %s", d)
	}
	// The body is still there for the transport.
	if b, _ := io.ReadAll(r.Body); string(b) != `{"device_pub":"x"}` || r.ContentLength != int64(len(b)) {
		t.Fatalf("body %q, length %d", b, r.ContentLength)
	}
	if b, _ := r.GetBody(); b == nil {
		t.Fatal("GetBody")
	}
	// Two signatures never share a nonce.
	r2 := signed(t, "POST", "https://cloud.mirrin.app/v1/link/start", `{"device_pub":"x"}`, priv, t0)
	if r2.Header.Get("Signature-Input") == in {
		t.Fatal("nonce repeated")
	}
	if err := Sign(r2, "dev:someone-else", priv, t0); err == nil {
		t.Fatal("Sign accepted a key id for another key")
	}
	// Only http and https URLs can be signed.
	r3, _ := http.NewRequest("GET", "ftp://cloud.mirrin.app/x", nil)
	if err := Sign(r3, idOf(priv), priv, t0); err == nil {
		t.Fatal("Sign accepted an ftp URL")
	}
}

// End to end over real loopback HTTP and HTTPS: the client signs, the server
// checks the request against its own origin, and the handler still gets the
// body.
func TestRoundTrip(t *testing.T) {
	priv := testKey("a")
	seen := NewMemoryNonceCache(0, 0)
	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprint("tls=", useTLS), func(t *testing.T) {
			srv := httptest.NewUnstartedServer(nil)
			origin := "http://" + srv.Listener.Addr().String()
			if useTLS {
				origin = "https://" + srv.Listener.Addr().String()
			}
			srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, err := VerifyFor(r, []string{origin}, lookupOf(priv), time.Now(), skew, seen)
				if err != nil {
					http.Error(w, err.Error(), http.StatusUnauthorized)
					return
				}
				b, _ := io.ReadAll(r.Body)
				fmt.Fprintf(w, "%s %s", id, b)
			})
			if useTLS {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			for _, m := range []struct{ method, path, body string }{
				{"POST", "/v1/link/start", `{"handle":"ember-otter-42"}`},
				{"GET", "/v1/link/lk_1?wait=1&x=%2F", ""},
				{"DELETE", "/v1/account", ""},
			} {
				r := signed(t, m.method, srv.URL+m.path, m.body, priv, time.Now())
				res, err := srv.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}
				got, _ := io.ReadAll(res.Body)
				res.Body.Close()
				if res.StatusCode != 200 || string(got) != idOf(priv)+" "+m.body {
					t.Fatalf("%s %s: %d %s", m.method, m.path, res.StatusCode, got)
				}
			}
		})
	}
}

func TestVerifyRejects(t *testing.T) {
	priv, other := testKey("a"), testKey("b")
	const url = "https://cloud.mirrin.app/v1/entitlement/refresh?x=1"
	body := `{"gen":3}`
	verify := func(r *http.Request, at time.Time) error {
		_, err := Verify(r, lookupOf(priv), at, skew, NewMemoryNonceCache(0, 0))
		return err
	}
	fresh := func() *http.Request { return signed(t, "POST", url, body, priv, t0) }
	if err := verify(fresh(), t0); err != nil {
		t.Fatalf("control: %v", err)
	}
	// resign replaces the signature with one over r as it now is.
	resign := func(r *http.Request) {
		r.Header.Del("Signature-Input")
		r.Header.Del("Signature")
		signRaw(t, r, Label, priv, components(covered...), profileParams(priv))
	}

	cases := []struct {
		name string
		edit func(r *http.Request)
		at   time.Time
		want error
		msg  string // "" means want itself, with no detail
	}{
		{"target-uri path", func(r *http.Request) { r.URL.Path = "/v1/account" }, t0, ErrSignature, ""},
		{"target-uri query", func(r *http.Request) { r.URL.RawQuery = "x=2" }, t0, ErrSignature, ""},
		{"target-uri host", func(r *http.Request) { r.Host = "evil.example" }, t0, ErrSignature, ""},
		{"target-uri port", func(r *http.Request) { r.Host = "cloud.mirrin.app:8443" }, t0, ErrSignature, ""},
		{"target-uri scheme", func(r *http.Request) { r.URL.Scheme = "http" }, t0, ErrSignature, ""},
		{"method", func(r *http.Request) { r.Method = "PUT" }, t0, ErrSignature, ""},
		{"body", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`{"gen":4}`)) }, t0, ErrDigest, ""},
		{"body and digest", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{"gen":4}`))
			r.Header.Set("Content-Digest", contentDigest([]byte(`{"gen":4}`)))
		}, t0, ErrSignature, ""},
		{"stale created", nil, t0.Add(601 * time.Second), ErrStale, ""},
		{"created from the future", nil, t0.Add(-301 * time.Second), ErrStale, ""},
		{"a moment past the window", nil, t0.Add(600*time.Second + time.Nanosecond), ErrStale, ""},
		{"a moment before created-skew", nil, t0.Add(-300*time.Second - time.Nanosecond), ErrStale, ""},
		{"no signature", func(r *http.Request) { r.Header.Del("Signature-Input"); r.Header.Del("Signature") }, t0, ErrUnsigned, ""},
		{"no Signature field", func(r *http.Request) { r.Header.Del("Signature") }, t0, ErrSignature, "no Signature for sig1"},
		{"zero signature", func(r *http.Request) {
			r.Header.Set("Signature", serializeDictionary([]member{{key: Label, item: item{v: make([]byte, 64)}}}))
		}, t0, ErrSignature, ""},
		{"other tag only", func(r *http.Request) {
			r.Header.Set("Signature-Input", strings.Replace(r.Header.Get("Signature-Input"), Tag, "someone-else", 1))
		}, t0, ErrUnsigned, ""},
		{"second mirrin input, unsigned", func(r *http.Request) {
			r.Header.Add("Signature-Input", strings.Replace(r.Header.Get("Signature-Input"), "sig1=", "sig2=", 1))
		}, t0, ErrSignature, "two antbot-cloud-v1 signatures"},
		// Each of these two would verify on its own.
		{"two valid mirrin signatures", func(r *http.Request) {
			signRaw(t, r, "sig2", priv, components(covered...), profileParams(priv))
		}, t0, ErrSignature, "two antbot-cloud-v1 signatures"},
		{"repeated label", func(r *http.Request) { r.Header.Add("Signature-Input", r.Header.Get("Signature-Input")) }, t0, ErrSignature, `repeated key "sig1"`},
		// Content-Digest is covered, so these are signed after the edit and
		// fail on the digest rules, not the signature.
		{"digest with sha-512 too", func(r *http.Request) {
			r.Header.Set("Content-Digest", r.Header.Get("Content-Digest")+", sha-512=:AA==:")
			resign(r)
		}, t0, ErrDigest, "want exactly one sha-256 digest"},
		{"digest with a parameter", func(r *http.Request) {
			r.Header.Set("Content-Digest", r.Header.Get("Content-Digest")+";x=1")
			resign(r)
		}, t0, ErrDigest, "want exactly one sha-256 digest"},
		{"sha-512 digest only", func(r *http.Request) {
			r.Header.Set("Content-Digest", "sha-512=:AA==:")
			resign(r)
		}, t0, ErrDigest, "want exactly one sha-256 digest"},
		{"digest over other content", func(r *http.Request) {
			r.Header.Set("Content-Digest", contentDigest([]byte(`{"gen":4}`)))
			resign(r)
		}, t0, ErrDigest, ""},
	}
	for _, c := range cases {
		r := fresh()
		if c.edit != nil {
			c.edit(r)
		}
		wantErr(t, c.name, verify(r, c.at), c.want, c.msg)
	}

	// Inside the skew either way is fine, to the last instant.
	for _, at := range []time.Time{t0.Add(600 * time.Second), t0.Add(-300 * time.Second)} {
		if err := verify(fresh(), at); err != nil {
			t.Errorf("at %v: %v", at.Sub(t0), err)
		}
	}
	// Signed by a key the server does not know.
	wantErr(t, "unknown key", verify(signed(t, "POST", url, body, other, t0), t0), ErrSignature, "unknown device")
	// The key id is bound to the key: a signature by priv that names
	// other's id is refused even when the lookup hands back priv's key.
	r := fresh()
	r.Header.Del("Signature-Input")
	r.Header.Del("Signature")
	ps := profileParams(priv)
	ps[3] = param{"keyid", idOf(other)}
	signRaw(t, r, Label, priv, components(covered...), ps)
	anyID := func(string) (ed25519.PublicKey, error) { return priv.Public().(ed25519.PublicKey), nil }
	_, err := Verify(r, anyID, t0, skew, NewMemoryNonceCache(0, 0))
	wantErr(t, "key under another id", err, ErrSignature, "does not match its id")
	// And the lookup's answer must be the key for that id.
	liar := func(string) (ed25519.PublicKey, error) { return other.Public().(ed25519.PublicKey), nil }
	_, err = Verify(fresh(), liar, t0, skew, NewMemoryNonceCache(0, 0))
	wantErr(t, "lookup returns another key", err, ErrSignature, "does not match its id")

	if _, err := Verify(fresh(), lookupOf(priv), t0, skew, nil); err == nil || errors.Is(err, ErrSignature) {
		t.Errorf("Verify without a nonce cache: %v", err)
	}
}

// wire reads a request off the wire as a server would, carrying c's
// signature headers.
func wire(t testing.TB, c *http.Request, target, host, extra string) *http.Request {
	t.Helper()
	raw := "POST " + target + " HTTP/1.1\r\nHost: " + host + "\r\n" + extra +
		"Content-Digest: " + c.Header.Get("Content-Digest") + "\r\n" +
		"Signature-Input: " + c.Header.Get("Signature-Input") + "\r\n" +
		"Signature: " + c.Header.Get("Signature") + "\r\nContent-Length: 2\r\n\r\n{}"
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The server's view. An origin-form request read off the wire has no scheme
// or host in r.URL, so Verify takes the scheme from the listener (or from
// the server, which may set r.URL.Scheme) and the authority from Host. An
// absolute-form target puts the client's scheme in r.URL, so Verify refuses
// it; VerifyFor takes the scheme from the server's origin whatever the form.
// Forwarding headers change nothing, and default ports are ignored.
func TestServerSideTargetURI(t *testing.T) {
	priv := testKey("a")
	c := signed(t, "POST", "https://cloud.mirrin.app/v1/me?a=1", "{}", priv, t0)
	verify := func(r *http.Request) error {
		_, err := Verify(r, lookupOf(priv), t0, skew, NewMemoryNonceCache(0, 0))
		return err
	}
	verifyFor := func(r *http.Request) error {
		_, err := VerifyFor(r, []string{"https://cloud.mirrin.app"}, lookupOf(priv), t0, skew, NewMemoryNonceCache(0, 0))
		return err
	}
	// A plain HTTP listener: the scheme is http, so it fails…
	wantErr(t, "http listener", verify(wire(t, c, "/v1/me?a=1", "cloud.mirrin.app", "")), ErrSignature, "")
	// …and a forwarding header does not talk it round.
	wantErr(t, "X-Forwarded-Proto", verify(wire(t, c, "/v1/me?a=1", "cloud.mirrin.app", "X-Forwarded-Proto: https\r\n")), ErrSignature, "")
	// Nor does an absolute-form target naming https.
	abs := wire(t, c, "https://cloud.mirrin.app/v1/me?a=1", "whatever", "")
	if abs.URL.Scheme != "https" || abs.Host != "cloud.mirrin.app" {
		t.Fatalf("net/http changed: absolute-form gave scheme %q host %q", abs.URL.Scheme, abs.Host)
	}
	wantErr(t, "absolute-form", verify(abs), ErrSignature, "not origin-form")
	// A TLS listener, or a proxy-fronted server that sets the scheme itself.
	r := wire(t, c, "/v1/me?a=1", "Cloud.Mirrin.App:443", "")
	r.TLS = &tls.ConnectionState{}
	if err := verify(r); err != nil {
		t.Fatalf("tls listener: %v", err)
	}
	r = wire(t, c, "/v1/me?a=1", "cloud.mirrin.app", "")
	r.URL.Scheme = "https"
	if err := verify(r); err != nil {
		t.Fatalf("scheme set by the server: %v", err)
	}

	// VerifyFor: the origin supplies the scheme, on any listener and in
	// either form, and the host must be the server's.
	for _, w := range []struct{ target, host string }{
		{"/v1/me?a=1", "cloud.mirrin.app"},
		{"/v1/me?a=1", "Cloud.Mirrin.App:443"},
		{"https://cloud.mirrin.app/v1/me?a=1", "whatever"},
		{"http://cloud.mirrin.app/v1/me?a=1", "whatever"},
	} {
		if err := verifyFor(wire(t, c, w.target, w.host, "")); err != nil {
			t.Errorf("VerifyFor %s, Host %s: %v", w.target, w.host, err)
		}
	}
	wantErr(t, "absolute-form for another host", verifyFor(wire(t, c, "https://staging.mirrin.app/v1/me?a=1", "cloud.mirrin.app", "")), ErrWrongHost, "")
	wantErr(t, "another port", verifyFor(wire(t, c, "/v1/me?a=1", "cloud.mirrin.app:8443", "")), ErrWrongHost, "")
}

// A request signed for another server (staging, a self-hosted control plane)
// cannot be replayed at this one inside its window.
func TestVerifyForRefusesOtherHosts(t *testing.T) {
	priv := testKey("a")
	prod := []string{"https://cloud.mirrin.app"}
	verifyFor := func(r *http.Request, origins []string) error {
		_, err := VerifyFor(r, origins, lookupOf(priv), t0, skew, NewMemoryNonceCache(0, 0))
		return err
	}
	staging := signed(t, "POST", "https://staging.mirrin.app/v1/recover", "{}", priv, t0)
	// Replayed as it was, Host and all: refused for the host…
	err := verifyFor(wire(t, staging, "/v1/recover", "staging.mirrin.app", ""), prod)
	wantErr(t, "cross-host replay", err, ErrWrongHost, "")
	if !errors.Is(err, ErrSignature) {
		t.Errorf("ErrWrongHost does not wrap ErrSignature: %v", err)
	}
	// …and with the Host rewritten, refused for the signature.
	wantErr(t, "host rewritten", verifyFor(wire(t, staging, "/v1/recover", "cloud.mirrin.app", ""), prod), ErrSignature, "")
	// Staging itself accepts it, and a client request built in process
	// verifies against its own origin.
	if err := verifyFor(wire(t, staging, "/v1/recover", "staging.mirrin.app", ""), []string{"https://staging.mirrin.app"}); err != nil {
		t.Fatalf("at staging: %v", err)
	}
	if err := verifyFor(signed(t, "POST", "https://cloud.mirrin.app/v1/recover", "{}", priv, t0), prod); err != nil {
		t.Fatalf("in process: %v", err)
	}
	// Several origins: each host gets its own scheme.
	multi := []string{"https://cloud.mirrin.app", "http://127.0.0.1:8080"}
	if err := verifyFor(wire(t, signed(t, "POST", "http://127.0.0.1:8080/v1/me", "{}", priv, t0), "/v1/me", "127.0.0.1:8080", ""), multi); err != nil {
		t.Fatalf("second origin: %v", err)
	}
	// A bad list is the server's mistake: an error, but not a verdict on
	// the request.
	for _, origins := range [][]string{
		nil,
		{"cloud.mirrin.app"},
		{"https://cloud.mirrin.app/"},
		{"https://cloud.mirrin.app/v1"},
		{"https://cloud.mirrin.app?x=1"},
		{"https://cloud.mirrin.app#"},
		{"https://user@cloud.mirrin.app"},
		{"ftp://cloud.mirrin.app"},
		{"https://cloud.mirrin.app", "http://cloud.mirrin.app"},
		{"https://cloud.mirrin.app", "https://CLOUD.mirrin.app:443"},
	} {
		err := verifyFor(signed(t, "POST", "https://cloud.mirrin.app/v1/me", "{}", priv, t0), origins)
		if err == nil || errors.Is(err, ErrSignature) {
			t.Errorf("origins %q: err = %v, want a configuration error", origins, err)
		}
	}
}

// A signature made off the profile (different components, extra or missing
// parameters, too long a life) is refused even though it verifies.
func TestVerifyRejectsOffProfile(t *testing.T) {
	priv := testKey("a")
	std := profileParams(priv)
	with := func(ps ...param) []param { return append(append([]param{}, std...), ps...) }
	without := func(key string) []param {
		var out []param
		for _, p := range std {
			if p.key != key {
				out = append(out, p)
			}
		}
		return out
	}
	set := func(key string, v any) []param {
		out := append([]param{}, std...)
		for i := range out {
			if out[i].key == key {
				out[i].v = v
			}
		}
		return out
	}
	const (
		comps  = "covered components are not the profile's"
		nonce  = "nonce is not 16 bytes of base64url"
		window = "expires must be within 300 s after created"
		needed = "created, expires and keyid are required"
	)
	full := components(covered...)
	for _, c := range []struct {
		name   string
		comps  []item
		params []param
		msg    string // "" means it verifies
	}{
		{"profile", full, std, ""},
		{"alg ed25519", full, with(param{"alg", "ed25519"}), ""},
		{"alg hmac", full, with(param{"alg", "hmac-sha256"}), "bad parameter alg"},
		{"extra parameter", full, with(param{"x", int64(1)}), "unexpected parameter x"},
		{"no nonce", full, without("nonce"), nonce},
		{"no created", full, without("created"), needed},
		{"no expires", full, without("expires"), needed},
		{"no keyid", full, without("keyid"), needed},
		{"short nonce", full, set("nonce", "AAAA"), nonce},
		{"nonce not b64url", full, set("nonce", "AAAAAAAAAAAAAAAAAAAA=="), nonce},
		{"created as string", full, set("created", "1790510400"), "bad parameter created"},
		{"keyid as token", full, set("keyid", token("dev")), "bad parameter keyid"},
		{"expires too late", full, set("expires", t0.Unix()+301), window},
		{"expires at created", full, set("expires", t0.Unix()), window},
		{"expires before created", full, set("expires", t0.Unix()-1), window},
		{"no content-digest", components("@method", "@target-uri"), std, comps},
		{"reordered", components("@target-uri", "@method", "content-digest"), std, comps},
		{"extra component", components("@method", "@target-uri", "content-digest", "date"), std, comps},
		{"component parameter", []item{{v: "@method"}, {v: "@target-uri"}, {v: "content-digest", params: []param{{"sf", true}}}}, std, comps},
	} {
		r, _ := http.NewRequest("POST", "https://cloud.mirrin.app/v1/me", strings.NewReader("{}"))
		r.Header.Set("Content-Digest", contentDigest([]byte("{}")))
		r.Header.Set("Date", "x")
		signRaw(t, r, Label, priv, c.comps, c.params)
		_, err := Verify(r, lookupOf(priv), t0, skew, NewMemoryNonceCache(0, 0))
		if c.msg == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		wantErr(t, c.name, err, ErrSignature, c.msg)
	}
}

func TestReplay(t *testing.T) {
	priv := testKey("a")
	req := func() (*http.Request, *http.Request) {
		r := signed(t, "POST", "https://cloud.mirrin.app/v1/backup/presign", `{"op":"put"}`, priv, t0)
		replay := r.Clone(t.Context())
		replay.Body, _ = r.GetBody()
		return r, replay
	}
	verify := func(r *http.Request, at time.Time, seen NonceCache) error {
		_, err := Verify(r, lookupOf(priv), at, skew, seen)
		return err
	}
	seen := NewMemoryNonceCache(0, 0)
	r, replay := req()
	if err := verify(r, t0, seen); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "replay", verify(replay, t0.Add(time.Minute), seen), ErrReplay, "")

	// Right up to the end of the window, and past it, a replay never gets
	// in: the nonce is held through the last instant the clock check
	// allows, at full precision.
	r, _ = req()
	seen = NewMemoryNonceCache(0, 0)
	if err := verify(r, t0, seen); err != nil {
		t.Fatal(err)
	}
	for d := 599 * time.Second; d <= 601*time.Second; d += 50 * time.Millisecond {
		again, _ := r.GetBody()
		rr := r.Clone(t.Context())
		rr.Body = again
		if err := verify(rr, t0.Add(d), seen); !errors.Is(err, ErrReplay) && !errors.Is(err, ErrStale) {
			t.Fatalf("replay at t0+%v: %v", d, err)
		}
	}
	for d, want := range map[time.Duration]error{
		600 * time.Second:                           ErrReplay, // the last instant
		600*time.Second + 500*time.Millisecond:      ErrStale,  // the reviewer's case
		600*time.Second + 999999999*time.Nanosecond: ErrStale,
	} {
		rr := r.Clone(t.Context())
		rr.Body, _ = r.GetBody()
		wantErr(t, fmt.Sprint("replay at t0+", d), verify(rr, t0.Add(d), seen), want, "")
	}

	// A replay whose clock was read long before it reached the cache (a slow
	// body, say) is refused once a sweep may have dropped its entry.
	seen = NewMemoryNonceCache(0, 0)
	r, replay = req()
	if err := verify(r, t0, seen); err != nil {
		t.Fatal(err)
	}
	if err := seen.Use("other-key", "n", t0.Add(700*time.Second), t0.Add(900*time.Second)); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "stale clock after a sweep", verify(replay, t0.Add(599*time.Second), seen), ErrStale, "")

	// A failed request does not burn its nonce.
	seen = NewMemoryNonceCache(0, 0)
	r, good := req()
	r.Body = io.NopCloser(strings.NewReader("tampered"))
	wantErr(t, "tampered", verify(r, t0, seen), ErrDigest, "")
	if err := verify(good, t0, seen); err != nil {
		t.Fatalf("nonce burnt by a failed request: %v", err)
	}
}

func TestMemoryNonceCache(t *testing.T) {
	c := NewMemoryNonceCache(2, 0)
	until := t0.Add(10 * time.Minute)
	if err := c.Use("k", "n1", t0, until); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "at until", c.Use("k", "n1", until, until), ErrReplay, "")
	if err := c.Use("other", "n1", t0, until); err != nil {
		t.Fatalf("same nonce, other key: %v", err)
	}
	wantErr(t, "full", c.Use("k", "n2", t0, until), ErrNonceCacheFull, "")
	// Once past until, entries are swept and nonces are free again.
	later := until.Add(2 * time.Minute)
	if err := c.Use("k", "n2", later, later.Add(time.Minute)); err != nil {
		t.Fatalf("after sweep: %v", err)
	}
	if err := c.Use("k", "n1", later, later.Add(time.Minute)); err != nil {
		t.Fatalf("expired nonce: %v", err)
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d", c.Len())
	}
	// Anything that could have been swept is refused, not guessed at.
	wantErr(t, "until before the last sweep", c.Use("k", "n9", t0, until), ErrStale, "")
}

// One key cannot fill the cache for everyone else.
func TestNonceCachePerKey(t *testing.T) {
	c := NewMemoryNonceCache(10, 2)
	until := t0.Add(10 * time.Minute)
	for _, n := range []string{"n1", "n2"} {
		if err := c.Use("noisy", n, t0, until); err != nil {
			t.Fatal(err)
		}
	}
	err := c.Use("noisy", "n3", t0, until)
	wantErr(t, "noisy key", err, ErrNonceKeyFull, "")
	if !errors.Is(err, ErrNonceCacheFull) {
		t.Errorf("ErrNonceKeyFull does not wrap ErrNonceCacheFull")
	}
	if err := c.Use("quiet", "n1", t0, until); err != nil {
		t.Fatalf("another key: %v", err)
	}
	// Its nonces expire and it may go again.
	later := until.Add(2 * time.Minute)
	if err := c.Use("noisy", "n3", later, later.Add(time.Minute)); err != nil {
		t.Fatalf("after its nonces expired: %v", err)
	}
	// The whole-cache limit still holds, and says so.
	c = NewMemoryNonceCache(3, 2)
	for _, k := range []string{"a", "b", "c"} {
		if err := c.Use(k, "n", t0, until); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Use("d", "n", t0, until); !errors.Is(err, ErrNonceCacheFull) || errors.Is(err, ErrNonceKeyFull) {
		t.Fatalf("whole cache full: %v", err)
	}
}

// Of many concurrent uses of one nonce, exactly one wins.
func TestNonceCacheConcurrent(t *testing.T) {
	c := NewMemoryNonceCache(0, 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.Use("k", "n", t0, t0.Add(time.Minute)) == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d winners", wins)
	}
}

func TestBodyLimit(t *testing.T) {
	priv := testKey("a")
	r, _ := http.NewRequest("POST", "https://cloud.mirrin.app/v1/x", strings.NewReader(strings.Repeat("a", MaxBody+1)))
	if err := Sign(r, idOf(priv), priv, t0); err == nil {
		t.Fatal("signed an oversized body")
	}
	r = signed(t, "POST", "https://cloud.mirrin.app/v1/x", "ok", priv, t0)
	r.Body = io.NopCloser(strings.NewReader(strings.Repeat("a", MaxBody+1)))
	if _, err := Verify(r, lookupOf(priv), t0, skew, NewMemoryNonceCache(0, 0)); err == nil {
		t.Fatal("verified an oversized body")
	}
}
