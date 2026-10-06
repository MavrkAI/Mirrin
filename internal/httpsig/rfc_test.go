package httpsig

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// rfcLines reads an RFC excerpt from testdata: our # lines and the RFC's
// three-space indent are dropped, and RFC 8792 backslash wrapping is undone
// (the backslash and the next line's leading spaces go).
func rfcLines(t testing.TB, name string) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "#") {
			continue
		}
		l = strings.TrimPrefix(l, "   ")
		if n := len(out); n > 0 && strings.HasSuffix(out[n-1], `\`) {
			out[n-1] = strings.TrimSuffix(out[n-1], `\`) + strings.TrimLeft(l, " ")
			continue
		}
		out = append(out, l)
	}
	return out
}

// between returns the lines from the first starting with from through the
// first after it starting with to.
func between(t testing.TB, lines []string, from, to string) []string {
	t.Helper()
	for i, l := range lines {
		if !strings.HasPrefix(l, from) {
			continue
		}
		for j := i; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], to) {
				return lines[i : j+1]
			}
		}
	}
	t.Fatalf("no %q … %q", from, to)
	return nil
}

type b26 struct {
	req      *http.Request
	body     []byte
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	base     string
	sigInput string
	sig      string
}

// loadB26 builds RFC 9421's test-request and B.2.6 values from the excerpt.
func loadB26(t testing.TB) b26 {
	t.Helper()
	lines := rfcLines(t, "rfc9421-b26.txt")
	var v b26

	rest := []byte(strings.Join(lines, "\n"))
	for {
		var blk *pem.Block
		if blk, rest = pem.Decode(rest); blk == nil {
			break
		}
		switch blk.Type {
		case "PUBLIC KEY":
			k, err := x509.ParsePKIXPublicKey(blk.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			v.pub = k.(ed25519.PublicKey)
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			v.priv = k.(ed25519.PrivateKey)
		}
	}
	if v.pub == nil || v.priv == nil || !v.pub.Equal(v.priv.Public()) {
		t.Fatal("test-key-ed25519 did not load")
	}

	msg := between(t, lines, "POST /foo", `{"hello": "world"}`)
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(strings.Join(msg, "\r\n"))))
	if err != nil {
		t.Fatal(err)
	}
	if v.body, err = io.ReadAll(req.Body); err != nil {
		t.Fatal(err)
	}
	req.Body = io.NopCloser(bytes.NewReader(v.body))
	v.req = req

	v.base = strings.Join(between(t, lines, `"date": `, `"@signature-params": `), "\n")
	v.sigInput = strings.TrimPrefix(between(t, lines, "Signature-Input: ", "Signature-Input: ")[0], "Signature-Input: ")
	v.sig = strings.TrimPrefix(between(t, lines, "Signature: ", "Signature: ")[0], "Signature: ")
	return v
}

// RFC 9421 Appendix B.2.6: the signature base is rebuilt byte for byte from
// the test request, the published signature verifies with test-key-ed25519,
// and signing that base reproduces it (Ed25519 is deterministic).
func TestRFC9421B26(t *testing.T) {
	v := loadB26(t)
	v.req.Header.Set("Signature-Input", v.sigInput)
	v.req.Header.Set("Signature", v.sig)

	sigs, err := parseSignatures(v.req.Header)
	if err != nil || len(sigs) != 1 || sigs[0].label != "sig-b26" {
		t.Fatalf("parse: %+v, %v", sigs, err)
	}
	o, err := listenerOrigin(v.req)
	if err != nil {
		t.Fatal(err)
	}
	base, err := signatureBase(v.req, o, sigs[0])
	if err != nil {
		t.Fatal(err)
	}
	if base != v.base {
		t.Fatalf("signature base:\n got %q\nwant %q", base, v.base)
	}
	if !verifyBase(v.pub, base, sigs[0].sig) {
		t.Fatal("RFC 9421 B.2.6 signature does not verify")
	}
	if got := serializeDictionary([]member{{key: "sig-b26", item: item{v: ed25519.Sign(v.priv, []byte(base))}}}); got != v.sig {
		t.Fatalf("re-signed:\n got %s\nwant %s", got, v.sig)
	}

	// Any covered change breaks it.
	for name, edit := range map[string]func(r *http.Request){
		"date":      func(r *http.Request) { r.Header.Set("Date", "Tue, 20 Apr 2021 02:07:56 GMT") },
		"method":    func(r *http.Request) { r.Method = "PUT" },
		"path":      func(r *http.Request) { r.URL.Path = "/bar" },
		"authority": func(r *http.Request) { r.Host = "example.org" },
		"length":    func(r *http.Request) { r.Header.Set("Content-Length", "19") },
	} {
		r := v.req.Clone(t.Context())
		edit(r)
		o, err := listenerOrigin(r)
		if err != nil {
			t.Fatal(err)
		}
		base, err := signatureBase(r, o, sigs[0])
		if err == nil && verifyBase(v.pub, base, sigs[0].sig) {
			t.Errorf("%s changed and it still verified", name)
		}
	}
}

// The test request's own Content-Digest (sha-512, RFC 9530) matches its body,
// which exercises the dictionary parser on published data.
func TestRFC9421TestRequestDigest(t *testing.T) {
	v := loadB26(t)
	ms, err := parseDictionary(v.req.Header.Get("Content-Digest"))
	if err != nil || len(ms) != 1 || ms[0].key != "sha-512" {
		t.Fatalf("%+v, %v", ms, err)
	}
	sum := sha512.Sum512(v.body)
	if !bytes.Equal(ms[0].item.v.([]byte), sum[:]) {
		t.Fatal("sha-512 digest does not match the body")
	}
}

// RFC 9530 Appendix B.1 and B.2: sha-256 of {"hello": "world"} and LF, and of
// empty content.
func TestRFC9530Digests(t *testing.T) {
	lines := rfcLines(t, "rfc9530-b1-b2.txt")
	var got []string
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "Content-Digest: "); ok {
			got = append(got, v)
		}
	}
	if len(got) != 2 {
		t.Fatalf("found %d Content-Digest examples, want 2", len(got))
	}
	for i, body := range [][]byte{[]byte("{\"hello\": \"world\"}\n"), nil} {
		if d := contentDigest(body); d != got[i] {
			t.Errorf("digest of %q = %s, RFC says %s", body, d, got[i])
		}
		h := http.Header{"Content-Digest": {got[i]}}
		if err := checkDigest(h, body); err != nil {
			t.Errorf("checkDigest(%s): %v", got[i], err)
		}
		if err := checkDigest(h, append(body, 'x')); err == nil {
			t.Errorf("checkDigest accepted other content for %s", got[i])
		}
	}
}

// The JWK form of test-key-ed25519 agrees with the PEM form.
func TestRFC9421KeyForms(t *testing.T) {
	v := loadB26(t)
	x := base64.RawURLEncoding.EncodeToString(v.pub)
	d := base64.RawURLEncoding.EncodeToString(v.priv.Seed())
	lines := strings.Join(rfcLines(t, "rfc9421-b26.txt"), "\n")
	if !strings.Contains(lines, `"x": "`+x+`"`) || !strings.Contains(lines, `"d": "`+d+`"`) {
		t.Fatalf("JWK x=%s d=%s not in the RFC text", x, d)
	}
}
