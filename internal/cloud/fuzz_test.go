package cloud

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json/v2"
	"encoding/pem"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// Everything in data/cloud and everything the control plane sends is read
// by a strict parser; these targets check that none of them panics, and
// that what parses means one thing.

// A ledger line that parses writes back as a line that parses to the same
// entry.
func FuzzParseEntry(f *testing.F) {
	f.Add([]byte(`{"at":"2026-09-27T10:00:00Z","method":"POST","host":"cloud.mirrin.app","path":"/v1/entitlement/refresh","req_bytes":0,"resp_bytes":612,"status":200}`))
	f.Add([]byte(`{"at":"2026-09-27T10:00:00Z","method":"GET","host":"127.0.0.1:1","path":"/v1/me","req_bytes":0,"resp_bytes":0,"status":0}`))
	f.Add([]byte(`{"at":"2026-09-27T10:00:00Z","method":"GET","host":"h","path":"/","req_bytes":-1,"resp_bytes":0,"status":0}`))
	f.Add([]byte(`{"at":"2026-09-27T10:00:00Z","method":"GET","method":"PUT","host":"h","path":"/","req_bytes":0,"resp_bytes":0,"status":0}`))
	f.Add([]byte(`{"at":"x"}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		e, err := parseEntry(line)
		if err != nil {
			return
		}
		var b strings.Builder
		if err := writeEntry(&b, e); err != nil {
			t.Fatal(err)
		}
		again, err := parseEntry([]byte(strings.TrimSuffix(b.String(), "\n")))
		if err != nil {
			t.Fatalf("re-encoded entry does not parse: %v", err)
		}
		e.At = e.At.UTC().Truncate(time.Second)
		if again != e {
			t.Fatalf("round trip: %+v became %+v", e, again)
		}
	})
}

// A device key file that parses holds an Ed25519 key that encodes back to
// the same key.
func FuzzParseKey(f *testing.F) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	good := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	f.Add(good)
	f.Add(append(append([]byte{}, good...), good...))
	f.Add([]byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, b []byte) {
		k, err := parseKey(b)
		if err != nil {
			return
		}
		if len(k) != ed25519.PrivateKeySize {
			t.Fatalf("parsed a %d-byte key", len(k))
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		again, err := parseKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		if err != nil || !again.Equal(k) {
			t.Fatalf("round trip: %v", err)
		}
	})
}

// link.json that parses names a canonical control plane origin.
func FuzzParseLinkRecord(f *testing.F) {
	f.Add([]byte(`{"api":"https://cloud.mirrin.app","handle":"ember-otter-42","gen":3,"linked_at":"2026-09-27T10:00:00Z"}`))
	f.Add([]byte(`{"api":"http://127.0.0.1:8080","handle":"","gen":1,"linked_at":"2026-09-27T10:00:00Z","superseded":{"gen":4,"at":"2026-10-01T00:00:00Z"}}`))
	f.Add([]byte(`{"api":"http://cloud.mirrin.app","gen":1}`))
	f.Add([]byte(`{"api":"https://CLOUD.mirrin.app:443","gen":1}`))
	f.Add([]byte(`{"api":"https://cloud.mirrin.app","gen":1,"extra":1}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		rec, err := parseLinkRecord(b)
		if err != nil {
			return
		}
		if api, err := parseAPI(rec.API); err != nil || api != rec.API {
			t.Fatalf("accepted api %q", rec.API)
		}
		out, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseLinkRecord(out); err != nil {
			t.Fatalf("re-encoded record does not parse: %v", err)
		}
	})
}

// Whatever the control plane answers, the client reads it without a panic,
// and any text it passes on to the user is short and holds only graphic
// runes: no C0 or C1 controls, no bidi overrides or other format
// characters.
func FuzzAnswer(f *testing.F) {
	f.Add(200, []byte(`{"id":"lk_1","checkout_url":"https://pay.example/1"}`))
	f.Add(409, []byte(`{"error":"superseded","gen":3,"at":"2026-10-01T00:00:00Z"}`))
	f.Add(402, []byte(`{"error":"lapsed","message":"Payment has lapsed."}`))
	f.Add(500, []byte("\x1b[2J\x00<html>"))
	f.Add(200, []byte(`{"id":"lk_1","id":"lk_2"}`))
	f.Add(400, []byte(`{"error":"x","message":"\u009b2J"}`))                          // C1 CSI, escaped
	f.Add(400, []byte("{\"error\":\"x\",\"message\":\"\u009b2J\"}"))                  // C1 CSI, raw
	f.Add(403, []byte(`{"error":"revoked\u202e","message":"txt.exe\u2066\u2028"}`))   // bidi and line separators
	f.Add(400, []byte(`{"error":"x","message":"\udb40\udc01tag"}`))                   // a Cf outside the BMP
	f.Add(200, []byte(`{"id":"lk_1","checkout_url":"https://pay.example/\u009b2J"}`)) // C1 in a page to print
	f.Fuzz(func(t *testing.T, status int, body []byte) {
		r := reply{status: status, body: body}
		var out struct {
			ID          string `json:"id"`
			CheckoutURL string `json:"checkout_url"`
		}
		err := decode(r, &out)
		if status < 200 || status > 299 {
			if err == nil {
				t.Fatal("an answer outside 2xx decoded")
			}
			ae := apiError(r).(*APIError)
			for _, s := range []string{ae.Code, ae.Message} {
				if len(s) > 300 || !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool {
					return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || !unicode.IsGraphic(r)
				}) {
					t.Fatalf("unsafe text from the server: %q", s)
				}
			}
			_ = ae.Error()
		}
		if err == nil {
			_ = checkID(out.ID)
			c := &Client{api: "https://cloud.example"}
			if c.checkBrowserURL(out.CheckoutURL) == nil && strings.ContainsFunc(out.CheckoutURL, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
				t.Fatalf("accepted a page with unprintable bytes: %q", out.CheckoutURL)
			}
		}
	})
}
