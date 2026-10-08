package httpsig

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// What parses serializes to something that parses back to the same thing.
func FuzzParseDictionary(f *testing.F) {
	for _, s := range []string{
		`sig1=("@method" "@target-uri" "content-digest");created=1790510400;expires=1790510700;nonce="AAAAAAAAAAAAAAAAAAAAAA";keyid="dev:x";tag="mirrin-cloud-v1"`,
		`sig-b26=("date" "@method" "@path" "@authority" "content-type" "content-length");created=1618884473;keyid="test-key-ed25519"`,
		`sig-b26=:wqcAqbmYJ2ji2glfAMaRy4gruYYnx2nEFN2HN6jrnDnQCK1u02Gb04v9EDgwUPiu4A0w6vuQv5lIp5WPpBKRCw==:`,
		`sha-256=:RK/0qy18MlBSVnWgjwz6lZEWjP/lF5HF9bvEF8FabDg=:`,
		`a=?0, b, c; foo=bar`, `a=(1 2), b=3, c=4;aa=bb, d=(5 6);valid`, `a=-12,b=*tok/en:x, c="q\"s\\"`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ms, err := parseDictionary(s)
		if err != nil {
			return
		}
		out := serializeDictionary(ms)
		ms2, err := parseDictionary(out)
		if err != nil {
			t.Fatalf("%q serialized as %q, which does not parse: %v", s, out, err)
		}
		if again := serializeDictionary(ms2); again != out {
			t.Fatalf("not stable: %q then %q", out, again)
		}
	})
}

// Verify and VerifyFor never panic on hostile signature fields, whatever
// they accept they accept once, and VerifyFor accepts only for its own host.
func FuzzVerify(f *testing.F) {
	priv := testKey("fuzz")
	lookup := lookupOf(priv)
	for _, body := range []string{"", `{"op":"put"}`} {
		r := signed(f, "POST", "https://cloud.mirrin.app/v1/backup/presign", body, priv, t0)
		f.Add(r.Header.Get("Signature-Input"), r.Header.Get("Signature"), r.Header.Get("Content-Digest"), "cloud.mirrin.app", body)
	}
	f.Add(`sig1=("@method" "@target-uri" "content-digest");created=1;expires=2;nonce="";keyid="";tag="mirrin-cloud-v1"`, `sig1=:AA==:`, `sha-256=:AA==:`, "", "")
	f.Fuzz(func(t *testing.T, input, sig, digest, host, body string) {
		r, err := http.NewRequest("POST", "https://cloud.mirrin.app/v1/backup/presign", strings.NewReader(body))
		if err != nil {
			t.Skip()
		}
		r.Host = host
		r.Header.Set("Signature-Input", input)
		r.Header.Set("Signature", sig)
		r.Header.Set("Content-Digest", digest)
		for _, verify := range []func(seen NonceCache) error{
			func(seen NonceCache) error { _, err := Verify(r, lookup, t0, skew, seen); return err },
			func(seen NonceCache) error {
				_, err := VerifyFor(r, []string{"https://cloud.mirrin.app"}, lookup, t0, skew, seen)
				return err
			},
		} {
			seen := NewMemoryNonceCache(0, 0)
			r.Body = io.NopCloser(strings.NewReader(body))
			if err := verify(seen); err != nil {
				continue
			}
			r.Body = io.NopCloser(strings.NewReader(body))
			if err := verify(seen); !errors.Is(err, ErrReplay) {
				t.Fatalf("accepted twice: %v", err)
			}
		}
		if _, err := VerifyFor(r, []string{"https://cloud.mirrin.app"}, lookup, t0, skew, NewMemoryNonceCache(0, 0)); err == nil {
			if o, _ := makeOrigin("https", host); o.authority != "cloud.mirrin.app" {
				t.Fatalf("VerifyFor accepted Host %q", host)
			}
		}
	})
}
