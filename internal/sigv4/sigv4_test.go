package sigv4

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// suiteContext is the AWS test suite's context.json: the credentials,
// scope, time and presign lifetime every case uses.
type suiteContext struct {
	Credentials struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		Token           string `json:"token"`
	} `json:"credentials"`
	Expiration int    `json:"expiration_in_seconds"`
	Normalize  bool   `json:"normalize"`
	Region     string `json:"region"`
	Service    string `json:"service"`
	Timestamp  string `json:"timestamp"`
}

func readContext(t *testing.T, dir string) (suiteContext, Creds, time.Time) {
	t.Helper()
	var c suiteContext
	if err := json.Unmarshal(readFile(t, filepath.Join(dir, "context.json")), &c); err != nil {
		t.Fatal(err)
	}
	ts, err := time.Parse(time.RFC3339, c.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	return c, Creds{AccessKeyID: c.Credentials.AccessKeyID, SecretAccessKey: c.Credentials.SecretAccessKey, SessionToken: c.Credentials.Token}, ts
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// parseRequest reads a raw request as the suites write them: a request line
// whose target may hold spaces and UTF-8, header lines ("Name:value", with
// indented continuation lines), a blank line and the body.
func parseRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	head, body, _ := strings.Cut(raw, "\n\n")
	lines := strings.Split(head, "\n")
	method, target, ok := strings.Cut(lines[0], " ")
	if !ok {
		t.Fatalf("bad request line %q", lines[0])
	}
	target = strings.TrimSuffix(strings.TrimRight(target, " "), " HTTP/1.1")
	if !strings.HasPrefix(target, "/") {
		target = "/" + target // the S3 examples write "PUT test$file.text" and "GET ?lifecycle"
	}
	path, query, _ := strings.Cut(target, "?")
	var names, values []string
	for _, l := range lines[1:] {
		if l == "" {
			continue
		}
		if l[0] == ' ' || l[0] == '\t' {
			values[len(values)-1] += " " + strings.TrimSpace(l)
			continue
		}
		n, v, ok := strings.Cut(l, ":")
		if !ok {
			t.Fatalf("bad header line %q", l)
		}
		names, values = append(names, n), append(values, v)
	}
	r := &http.Request{Method: method, Header: http.Header{}, URL: &url.URL{Scheme: "https", Path: path, RawQuery: query}}
	for i, n := range names {
		if strings.EqualFold(n, "host") {
			r.Host = strings.TrimSpace(values[i])
			r.URL.Host = r.Host
			continue
		}
		r.Header.Add(n, values[i])
	}
	if body != "" {
		r.Body = io.NopCloser(strings.NewReader(body))
	}
	return r
}

// suiteCases lists the aws4_testsuite case directories: every directory
// holding a <name>.req.
func suiteCases(t *testing.T) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir("testdata/aws4_testsuite", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".req") {
			dirs = append(dirs, filepath.Dir(p))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(dirs)
	return dirs
}

// TestAWSTestSuite signs every request of the AWS Signature Version 4 test
// suite and compares the canonical request, the string to sign and the
// Authorization header with the suite's. The suite's credentials, scope and
// time are the ones in the newer suite's context.json.
func TestAWSTestSuite(t *testing.T) {
	ctx, creds, now := readContext(t, "testdata/aws-signing-test-suite/v4/get-vanilla")
	cases := suiteCases(t)
	for _, want := range []string{"get-vanilla", "get-vanilla-query-order-key", "post-vanilla", "post-x-www-form-urlencoded", "get-slashes"} {
		if !slices.ContainsFunc(cases, func(d string) bool { return filepath.Base(d) == want }) {
			t.Fatalf("the suite has no %s case", want)
		}
	}
	for _, dir := range cases {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			file := func(ext string) string { return string(readFile(t, filepath.Join(dir, name+ext))) }
			r := parseRequest(t, file(".req"))
			c := creds
			switch {
			case name == "get-vanilla-with-session-token":
				// The token is not in the request; the newer suite's
				// context for the same case holds it.
				_, c, _ = readContext(t, "testdata/aws-signing-test-suite/v4/"+name)
			case r.Header.Get("X-Amz-Security-Token") != "":
				c.SessionToken = r.Header.Get("X-Amz-Security-Token")
			}
			// post-sts-header-after adds the token after signing, so it is
			// signed without one.
			creq, sts, err := sign(r, c, ctx.Region, ctx.Service, now, "")
			if err != nil {
				t.Fatal(err)
			}
			if want := file(".creq"); creq != want {
				t.Errorf("canonical request:\n%s\nwant:\n%s", creq, want)
			}
			if want := file(".sts"); sts != want {
				t.Errorf("string to sign:\n%s\nwant:\n%s", sts, want)
			}
			if got, want := r.Header.Get("Authorization"), file(".authz"); got != want {
				t.Errorf("Authorization:\n%s\nwant:\n%s", got, want)
			}
			// The suite's get-vanilla-with-session-token.sreq repeats
			// get-vanilla's signature, a slip upstream: its .creq, .sts and
			// .authz agree with each other and with this package.
			if name != "get-vanilla-with-session-token" && !strings.Contains(file(".sreq"), "Authorization: "+r.Header.Get("Authorization")) {
				t.Error("the signed request in .sreq carries another Authorization")
			}
		})
	}
}

// TestPresignSuite checks Presign against the query-string cases of the
// newer AWS suite: the canonical request, the signature, and the same query
// parameters as the suite's signed request.
func TestPresignSuite(t *testing.T) {
	dirs, err := filepath.Glob("testdata/aws-signing-test-suite/v4/*")
	if err != nil || len(dirs) == 0 {
		t.Fatal("no presign cases", err)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			ctx, creds, now := readContext(t, dir)
			r := parseRequest(t, string(readFile(t, filepath.Join(dir, "request.txt"))))
			if len(r.Header) != 0 {
				t.Fatal("Presign signs only the host; this case has headers")
			}
			// The suite writes paths and queries raw; a URL carries them
			// escaped.
			r.URL.RawQuery = escapeQuery(r.URL.RawQuery)
			got, creq, _, err := presign(r.Method, r.URL.String(), creds, ctx.Region, ctx.Service, time.Duration(ctx.Expiration)*time.Second, now, -1)
			if err != nil {
				t.Fatal(err)
			}
			if want := string(readFile(t, filepath.Join(dir, "query-canonical-request.txt"))); creq != want {
				t.Errorf("canonical request:\n%s\nwant:\n%s", creq, want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatal(err)
			}
			sig := strings.TrimSpace(string(readFile(t, filepath.Join(dir, "query-signature.txt"))))
			if g := u.Query().Get("X-Amz-Signature"); g != sig {
				t.Errorf("signature %s, want %s", g, sig)
			}
			line, _, _ := strings.Cut(string(readFile(t, filepath.Join(dir, "query-signed-request.txt"))), "\n")
			want := parseRequest(t, line+"\nHost:example.amazonaws.com")
			wq, err := url.ParseQuery(escapeQuery(want.URL.RawQuery))
			if err != nil {
				t.Fatal(err)
			}
			if gq := u.Query(); !equalValues(gq, wq) {
				t.Errorf("query %v, want %v", gq, wq)
			}
		})
	}
}

// escapeQuery percent-encodes the raw UTF-8 and spaces the suite writes
// into a query, leaving its escapes and separators alone.
func escapeQuery(q string) string {
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		if c := q[i]; c <= ' ' || c >= 0x7f {
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func equalValues(a, b url.Values) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w := slices.Clone(b[k])
		v = slices.Clone(v)
		slices.Sort(v)
		slices.Sort(w)
		if !slices.Equal(v, w) {
			return false
		}
	}
	return true
}

// sections reads a testdata file of "## name" sections, dropping our #
// comment lines.
func sections(t *testing.T, name string) map[string]string {
	t.Helper()
	out := map[string]string{}
	var cur string
	var buf []string
	flush := func() {
		if cur != "" {
			out[cur] = strings.TrimRight(strings.Join(buf, "\n"), "\n")
		}
	}
	sc := bufio.NewScanner(strings.NewReader(string(readFile(t, filepath.Join("testdata", name)))))
	for sc.Scan() {
		l := sc.Text()
		if n, ok := strings.CutPrefix(l, "## "); ok {
			flush()
			cur, buf = n, nil
			continue
		}
		if strings.HasPrefix(l, "# ") || cur == "" {
			continue
		}
		buf = append(buf, l)
	}
	flush()
	return out
}

// trimLines drops the trailing spaces the documentation's code blocks carry.
func trimLines(s string) string {
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		ls[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(ls, "\n")
}

// The four worked examples of Authorization-header signing in the Amazon S3
// API Reference.
func TestS3HeaderExamples(t *testing.T) {
	secs := sections(t, "s3-header-examples.txt")
	creds := Creds{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	for _, ex := range []string{"get-object", "put-object", "get-bucket-lifecycle", "list-objects"} {
		t.Run(ex, func(t *testing.T) {
			raw, ok := secs[ex+".req"]
			if !ok {
				t.Fatalf("no %s.req", ex)
			}
			r := parseRequest(t, raw)
			if ex == "put-object" {
				// The page stands "<Payload>" in for the body it names.
				r.Body = io.NopCloser(strings.NewReader("Welcome to Amazon S3."))
			} else {
				r.Body = nil
			}
			sent := r.Header.Get("X-Amz-Content-Sha256")
			creq, sts, err := sign(r, creds, "us-east-1", "s3", now, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Header.Get("X-Amz-Content-Sha256"); got != strings.TrimSpace(sent) {
				t.Errorf("x-amz-content-sha256 %s, want the page's %s", got, sent)
			}
			if want := trimLines(secs[ex+".creq"]); creq != want {
				t.Errorf("canonical request:\n%s\nwant:\n%s", creq, want)
			}
			if want := trimLines(secs[ex+".sts"]); sts != want {
				t.Errorf("string to sign:\n%s\nwant:\n%s", sts, want)
			}
			authz := r.Header.Get("Authorization")
			if !strings.HasSuffix(authz, "Signature="+secs[ex+".signature"]) {
				t.Errorf("Authorization %s, want signature %s", authz, secs[ex+".signature"])
			}
			// The page writes the Authorization fields without the space
			// after each comma; AWS accepts both.
			if got := strings.ReplaceAll(authz, ", ", ","); got != secs[ex+".authz"] {
				t.Errorf("Authorization:\n%s\nwant:\n%s", got, secs[ex+".authz"])
			}
		})
	}
}

// The presigned URL example in the Amazon S3 API Reference, byte for byte.
func TestS3PresignExample(t *testing.T) {
	secs := sections(t, "s3-presign-example.txt")
	creds := Creds{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got, creq, sts, err := presign(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", creds, "us-east-1", "s3", 86400*time.Second, now, -1)
	if err != nil {
		t.Fatal(err)
	}
	if creq != secs["presign.creq"] {
		t.Errorf("canonical request:\n%s\nwant:\n%s", creq, secs["presign.creq"])
	}
	if sts != secs["presign.sts"] {
		t.Errorf("string to sign:\n%s\nwant:\n%s", sts, secs["presign.sts"])
	}
	if got != secs["presign.presigned-url"] {
		t.Errorf("presigned URL:\n%s\nwant:\n%s", got, secs["presign.presigned-url"])
	}
	if pub, err := Presign(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", creds, "us-east-1", "s3", 86400*time.Second, now); err != nil || pub != got {
		t.Errorf("Presign: %q %v", pub, err)
	}
}

// TestVectorFilesAreVerbatim pins every vector file to MANIFEST.sha256,
// whose own digest is pinned here and in SOURCE.txt.
func TestVectorFilesAreVerbatim(t *testing.T) {
	const manifestSHA256 = "f70fca5e65bef4d29ce6d144de2e152941280752d8ba20dfacb3e2eb507f5119"
	m := readFile(t, "testdata/MANIFEST.sha256")
	if sum := sha256.Sum256(m); hex.EncodeToString(sum[:]) != manifestSHA256 {
		t.Errorf("MANIFEST.sha256 has SHA-256 %x, want %s", sum, manifestSHA256)
	}
	listed := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(m)), "\n") {
		want, p, ok := strings.Cut(l, "  ")
		if !ok {
			t.Fatalf("bad manifest line %q", l)
		}
		listed[p] = true
		sum := sha256.Sum256(readFile(t, filepath.Join("testdata", p)))
		if hex.EncodeToString(sum[:]) != want {
			t.Errorf("%s changed", p)
		}
	}
	err := filepath.WalkDir("testdata", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("testdata", p)
		rel = filepath.ToSlash(rel)
		if rel != "MANIFEST.sha256" && rel != "SOURCE.txt" && !strings.HasPrefix(rel, "fuzz/") && !listed[rel] {
			t.Errorf("%s is not in MANIFEST.sha256", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSignRules(t *testing.T) {
	creds := Creds{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}
	now := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
	newReq := func(method, u, body string) *http.Request {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		r, err := http.NewRequest(method, u, rd)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("s3 keeps the path and sends the payload hash", func(t *testing.T) {
		r := newReq("PUT", "https://bucket.r2.example/a//b/../c%2Fd", "hello")
		creq, _, err := sign(r, creds, "auto", "s3", now, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(creq, "PUT\n/a//b/../c%2Fd\n") {
			t.Errorf("canonical request starts %q", creq[:30])
		}
		if r.Header.Get("X-Amz-Content-Sha256") != PayloadHash([]byte("hello")) {
			t.Error("no x-amz-content-sha256")
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != "hello" {
			t.Errorf("body after signing: %q", b)
		}
	})

	t.Run("other services normalize the path and send no payload header", func(t *testing.T) {
		r := newReq("GET", "https://route53.example/2013-04-01//hostedzone/./Z1/../Z2/", "")
		creq, _, err := sign(r, creds, "us-east-1", "route53", now, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(creq, "GET\n/2013-04-01/hostedzone/Z2/\n") {
			t.Errorf("canonical request starts %q", creq[:40])
		}
		if r.Header.Get("X-Amz-Content-Sha256") != "" {
			t.Error("x-amz-content-sha256 set for a service other than s3")
		}
	})

	t.Run("the query is sent as signed", func(t *testing.T) {
		r := newReq("GET", "https://bucket.example/?prefix=a+b&list-type=2&marker", "")
		if err := Sign(r, creds, "auto", "s3", now, EmptyPayloadHash); err != nil {
			t.Fatal(err)
		}
		if r.URL.RawQuery != "list-type=2&marker=&prefix=a%20b" {
			t.Errorf("query %q", r.URL.RawQuery)
		}
	})

	t.Run("signing twice gives the same header", func(t *testing.T) {
		r := newReq("GET", "https://bucket.example/k", "")
		r.Header.Set("Range", "bytes=0-9")
		Sign(r, creds, "auto", "s3", now, UnsignedPayload)
		first := r.Header.Get("Authorization")
		if err := Sign(r, creds, "auto", "s3", now, UnsignedPayload); err != nil || r.Header.Get("Authorization") != first {
			t.Errorf("second signature differs: %v", err)
		}
	})

	t.Run("ignored headers are not signed", func(t *testing.T) {
		r := newReq("GET", "https://bucket.example/k", "")
		r.Header.Set("User-Agent", "x")
		r.Header.Set("Content-Length", "5")
		r.Header.Set("Authorization", "old")
		if err := Sign(r, creds, "auto", "s3", now, UnsignedPayload); err != nil {
			t.Fatal(err)
		}
		if a := r.Header.Get("Authorization"); !strings.Contains(a, "SignedHeaders=host;x-amz-content-sha256;x-amz-date,") {
			t.Errorf("Authorization %s", a)
		}
	})

	for _, tc := range []struct {
		name            string
		creds           Creds
		region, service string
		hash            string
		mutate          func(*http.Request)
	}{
		{"no access key", Creds{SecretAccessKey: "s"}, "auto", "s3", "", nil},
		{"no secret", Creds{AccessKeyID: "a"}, "auto", "s3", "", nil},
		{"slash in the key id", Creds{AccessKeyID: "a/b", SecretAccessKey: "s"}, "auto", "s3", "", nil},
		{"control in the token", Creds{AccessKeyID: "a", SecretAccessKey: "s", SessionToken: "t\n"}, "auto", "s3", "", nil},
		{"slash in the region", creds, "us/east", "s3", "", nil},
		{"empty service", creds, "auto", "", "", nil},
		{"uppercase service", creds, "auto", "S3", "", nil},
		{"bad payload hash", creds, "auto", "s3", "ABC", nil},
		{"uppercase payload hash", creds, "auto", "s3", strings.ToUpper(EmptyPayloadHash), nil},
		{"newline in a header", creds, "auto", "s3", "", func(r *http.Request) { r.Header["X-Evil"] = []string{"a\r\nb"} }},
		{"bad header name", creds, "auto", "s3", "", func(r *http.Request) { r.Header["X Evil"] = []string{"a"} }},
		{"bad method", creds, "auto", "s3", "", func(r *http.Request) { r.Method = "GE T" }},
		{"bad query escape", creds, "auto", "s3", "", func(r *http.Request) { r.URL.RawQuery = "a=%zz" }},
		{"no host", creds, "auto", "s3", "", func(r *http.Request) { r.Host, r.URL.Host = "", "" }},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			r := newReq("GET", "https://bucket.example/k", "")
			if tc.mutate != nil {
				tc.mutate(r)
			}
			if err := Sign(r, tc.creds, tc.region, tc.service, now, tc.hash); err == nil {
				t.Error("signed")
			}
		})
	}
}

func TestPresignRules(t *testing.T) {
	creds := Creds{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", SessionToken: "tok+en/=="}
	now := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
	u, err := Presign(http.MethodPut, "https://acct.r2.cloudflarestorage.com/bucket/ns/abc/snap%201.age?partNumber=2&uploadId=x+y", creds, "auto", "s3", 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := p.Query()
	if q.Get("X-Amz-Security-Token") != "tok+en/==" || q.Get("X-Amz-Expires") != "900" || q.Get("uploadId") != "x y" || q.Get("partNumber") != "2" {
		t.Errorf("query %v", q)
	}
	if !strings.HasSuffix(p.RawQuery, "&X-Amz-Signature="+q.Get("X-Amz-Signature")) || len(q.Get("X-Amz-Signature")) != 64 {
		t.Errorf("signature is not last: %s", p.RawQuery)
	}
	if p.EscapedPath() != "/bucket/ns/abc/snap%201.age" {
		t.Errorf("path %s", p.EscapedPath())
	}
	withPort, err := Presign(http.MethodGet, "https://h.example:443/x", creds, "auto", "s3", time.Minute, now)
	without, _ := Presign(http.MethodGet, "https://h.example/x", creds, "auto", "s3", time.Minute, now)
	if err != nil || withPort != without {
		t.Errorf("the default port changed the URL:\n%s\n%s", withPort, without)
	}
	if u, _ := Presign(http.MethodGet, "https://[2001:db8::1]:443/x", creds, "auto", "s3", time.Minute, now); !strings.HasPrefix(u, "https://[2001:db8::1]/x?") {
		t.Errorf("IPv6 host: %s", u)
	}
	for _, tc := range []struct {
		name, url string
		expires   time.Duration
	}{
		{"too long", "https://h/x", MaxPresignExpiry + time.Second},
		{"zero", "https://h/x", 0},
		{"fractional", "https://h/x", 1500 * time.Millisecond},
		{"already signed", "https://h/x?X-Amz-Signature=abc", time.Minute},
		{"already has a date", "https://h/x?x-amz-date=abc", time.Minute},
		{"not http", "ftp://h/x", time.Minute},
		{"no host", "https:///x", time.Minute},
		{"user info", "https://u:p@h/x", time.Minute},
		{"fragment", "https://h/x#f", time.Minute},
		{"relative", "/x", time.Minute},
		{"bad escape", "https://h/x?a=%zz", time.Minute},
	} {
		if _, err := Presign(http.MethodGet, tc.url, creds, "auto", "s3", tc.expires, now); err == nil {
			t.Errorf("%s: presigned", tc.name)
		}
	}
}

// A sized presign signs the Content-Length, so storage refuses a PUT of any
// other length.
func TestPresignSized(t *testing.T) {
	creds := Creds{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}
	now := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
	raw := "https://acct.r2.cloudflarestorage.com/bucket/ns/abc/snap.age"
	u, err := PresignSized(http.MethodPut, raw, creds, "auto", "s3", 15*time.Minute, now, 42)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := url.Parse(u)
	if got := p.Query().Get("X-Amz-SignedHeaders"); got != "content-length;host" {
		t.Errorf("signed headers %q", got)
	}
	_, creq, _, _ := presign(http.MethodPut, raw, creds, "auto", "s3", 15*time.Minute, now, 42)
	if !strings.Contains(creq, "\ncontent-length:42\nhost:acct.r2.cloudflarestorage.com\n\ncontent-length;host\nUNSIGNED-PAYLOAD") {
		t.Errorf("canonical request:\n%s", creq)
	}
	other, _ := PresignSized(http.MethodPut, raw, creds, "auto", "s3", 15*time.Minute, now, 43)
	plain, _ := Presign(http.MethodPut, raw, creds, "auto", "s3", 15*time.Minute, now)
	if other == u || plain == u {
		t.Error("the size didn't change the signature")
	}
	if _, err := PresignSized(http.MethodPut, raw, creds, "auto", "s3", time.Minute, now, -1); err == nil {
		t.Error("a negative size was signed")
	}
}
