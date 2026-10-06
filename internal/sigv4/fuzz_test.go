package sigv4

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Nightly CI runs each of these for 60 s (.github/workflows/fuzz.yml).

// Canonicalizing a canonical query changes nothing, and a query that parses
// yields only unreserved characters, '%XX', '=' and '&'.
func FuzzCanonicalQuery(f *testing.F) {
	for _, s := range []string{"", "a=1", "Param2=value2&Param1=value1", "%E1%88%B4=bar", "a+b=c d", "lifecycle",
		"a=%zz", "&&a=&=b", "X-Amz-Credential=AKID%2F20150830%2Fus-east-1%2Fservice%2Faws4_request", "k=v;x", "a=%2B+%20"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		q, err := canonicalQuery(raw)
		if err != nil {
			return
		}
		again, err := canonicalQuery(q)
		if err != nil || again != q {
			t.Fatalf("canonicalQuery(%q) = %q, and again %q (%v)", raw, q, again, err)
		}
		if strings.Trim(q, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~%=&") != "" {
			t.Fatalf("canonical query %q holds a reserved character", q)
		}
	})
}

// Sign either refuses or produces a well-formed Authorization, the same
// one twice, for any path, query and header value.
func FuzzSign(f *testing.F) {
	f.Add("/", "", "v", "s3")
	f.Add("/a//b/../c%2Fd", "prefix=a+b&list-type=2", "bytes=0-9", "s3")
	f.Add("/2013-04-01/hostedzone/Z1/rrset", "", "  a   b  ", "route53")
	f.Add("/%E1%88%B4", "%E1%88%B4=bar", "x\ty", "service")
	f.Add("/./..//", "a=%zz", "a\r\nb", "service")
	f.Fuzz(func(t *testing.T, path, query, value, service string) {
		now := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
		creds := Creds{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}
		mk := func() *http.Request {
			return &http.Request{Method: http.MethodGet, Host: "h.example", Header: http.Header{"X-Test": {value}},
				URL: &url.URL{Scheme: "https", Host: "h.example", Path: path, RawQuery: query}}
		}
		r := mk()
		if err := Sign(r, creds, "us-east-1", service, now, EmptyPayloadHash); err != nil {
			return
		}
		a := r.Header.Get("Authorization")
		if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260927/us-east-1/"+service+"/aws4_request, SignedHeaders=") ||
			strings.ContainsAny(a, "\r\n") || len(a) < 64 {
			t.Fatalf("Authorization %q", a)
		}
		r2 := mk()
		if err := Sign(r2, creds, "us-east-1", service, now, EmptyPayloadHash); err != nil || r2.Header.Get("Authorization") != a {
			t.Fatalf("second signature differs (%v)", err)
		}
	})
}
