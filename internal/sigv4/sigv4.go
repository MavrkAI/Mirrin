// Package sigv4 signs HTTP requests with AWS Signature Version 4, for S3 and
// the S3-compatible stores (R2, B2, MinIO, Wasabi) and for the Route 53 API.
// It uses the standard library only.
//
// Sign puts the signature in the Authorization header; Presign puts it in the
// query string of a URL that someone else will fetch. Both follow the AWS
// test suite and the Amazon S3 API Reference, whose examples are in
// testdata. Two rules differ by service, as AWS specifies them: for "s3" the
// path is signed exactly as sent and the payload hash also travels in
// X-Amz-Content-Sha256; for every other service the path is normalized
// first (dot segments and empty segments removed). Paths are URI-encoded
// once, as the test suite does; the Route 53 and S3 paths this project signs
// need no second encoding.
package sigv4

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Creds are an access key pair, and a session token for temporary
// credentials.
type Creds struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // "" for long-term keys
}

const (
	// UnsignedPayload is the payload hash for a body that is not signed, such
	// as a presigned upload.
	UnsignedPayload = "UNSIGNED-PAYLOAD"
	// EmptyPayloadHash is the hex SHA-256 of an empty body.
	EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	// MaxPresignExpiry is the longest a presigned URL may last.
	MaxPresignExpiry = 7 * 24 * time.Hour
	// MaxHashedBody is the largest body Sign reads to hash itself. Pass the
	// hash of anything larger.
	MaxHashedBody = 32 << 20
)

const (
	algorithm  = "AWS4-HMAC-SHA256"
	timeFormat = "20060102T150405Z"
	dateFormat = "20060102"
	terminator = "aws4_request"
)

// ignoredHeaders are never signed: Go's transport writes Host, User-Agent,
// Content-Length, Transfer-Encoding and Trailer from elsewhere, and the
// others may change on the way.
var ignoredHeaders = []string{
	"authorization", "connection", "content-length", "expect", "host",
	"trailer", "transfer-encoding", "user-agent", "x-amzn-trace-id",
}

// PayloadHash is the hex SHA-256 of body, for Sign's payloadHash.
func PayloadHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Sign signs r as of now, setting X-Amz-Date, X-Amz-Security-Token when
// creds has one, X-Amz-Content-Sha256 for service "s3", and Authorization.
// Every other header already on r is signed too, apart from a few the
// transport rewrites. payloadHash is the hex SHA-256 of the body, or
// UnsignedPayload; "" makes Sign hash the body itself (up to
// MaxHashedBody), putting it back for sending. r.URL.RawQuery is rewritten
// to the canonical form, so what is sent is exactly what was signed.
func Sign(r *http.Request, creds Creds, region, service string, now time.Time, payloadHash string) error {
	_, _, err := sign(r, creds, region, service, now, payloadHash)
	return err
}

// sign is Sign, returning the canonical request and the string to sign.
func sign(r *http.Request, creds Creds, region, service string, now time.Time, payloadHash string) (creq, sts string, err error) {
	if r == nil || r.URL == nil {
		return "", "", errors.New("sigv4: no request")
	}
	if err := checkScope(creds, region, service); err != nil {
		return "", "", err
	}
	method, err := checkMethod(r.Method)
	if err != nil {
		return "", "", err
	}
	switch {
	case payloadHash == "":
		if payloadHash, err = hashBody(r); err != nil {
			return "", "", err
		}
	case !validPayloadHash(payloadHash):
		return "", "", fmt.Errorf("sigv4: payload hash %q is not 64 lowercase hex or %s", payloadHash, UnsignedPayload)
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	if host == "" || !printable(host) {
		return "", "", errors.New("sigv4: request has no host")
	}
	path, err := canonicalPath(r.URL, service != "s3")
	if err != nil {
		return "", "", err
	}
	query, err := canonicalQuery(r.URL.RawQuery)
	if err != nil {
		return "", "", err
	}

	now = now.UTC()
	amzDate := now.Format(timeFormat)
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header.Del("Authorization")
	r.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	if service == "s3" {
		r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	headers, signed, err := canonicalHeaders(host, r.Header)
	if err != nil {
		return "", "", err
	}
	creq = strings.Join([]string{method, path, query, headers, signed, payloadHash}, "\n")
	scope := credentialScope(now, region, service)
	sts = stringToSign(amzDate, scope, creq)
	r.URL.RawQuery = query
	r.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, creds.AccessKeyID, scope, signed, signature(creds.SecretAccessKey, now, region, service, sts)))
	return creq, sts, nil
}

// Presign returns rawURL with a signature in its query string, good for
// expires (at most seven days) from now. Only the host is signed, so whoever
// holds the URL can make the request with any headers. The payload is
// UnsignedPayload for "s3" and the hash of an empty body otherwise. The
// query comes back in canonical order, with X-Amz-Signature last.
func Presign(method, rawURL string, creds Creds, region, service string, expires time.Duration, now time.Time) (string, error) {
	u, _, _, err := presign(method, rawURL, creds, region, service, expires, now, -1)
	return u, err
}

// PresignSized is Presign with the Content-Length signed as well, so the
// request must carry exactly size bytes: a presigned PUT that storage holds
// to one size, whatever the holder of the URL sends.
func PresignSized(method, rawURL string, creds Creds, region, service string, expires time.Duration, now time.Time, size int64) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("sigv4: a signed Content-Length can't be %d", size)
	}
	u, _, _, err := presign(method, rawURL, creds, region, service, expires, now, size)
	return u, err
}

// presign is Presign, returning the canonical request and the string to
// sign as well. A size of 0 or more is signed as the Content-Length.
func presign(method, rawURL string, creds Creds, region, service string, expires time.Duration, now time.Time, size int64) (signedURL, creq, sts string, err error) {
	if err := checkScope(creds, region, service); err != nil {
		return "", "", "", err
	}
	m, err := checkMethod(method)
	if err != nil {
		return "", "", "", err
	}
	if expires < time.Second || expires > MaxPresignExpiry || expires%time.Second != 0 {
		return "", "", "", fmt.Errorf("sigv4: a presigned URL lasts whole seconds from 1 s to %s, not %s", MaxPresignExpiry, expires)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", fmt.Errorf("sigv4: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" || u.Host == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawFragment != "" || !printable(u.Host) {
		return "", "", "", fmt.Errorf("sigv4: %q is not an http or https URL with a host and no user info or fragment", rawURL)
	}
	// Browsers and most clients leave the default port out of Host, so it
	// is left out of what is signed too.
	if p := u.Port(); p == "443" && u.Scheme == "https" || p == "80" && u.Scheme == "http" {
		u.Host = strings.TrimSuffix(u.Host, ":"+p)
	}
	for part := range strings.SplitSeq(u.RawQuery, "&") {
		k, _, _ := strings.Cut(part, "=")
		if k, err := url.QueryUnescape(k); err == nil && strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			switch strings.ToLower(k) {
			case "x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-expires",
				"x-amz-signedheaders", "x-amz-signature", "x-amz-security-token":
				return "", "", "", fmt.Errorf("sigv4: %q already carries %s", rawURL, k)
			}
		}
	}
	now = now.UTC()
	amzDate := now.Format(timeFormat)
	scope := credentialScope(now, region, service)
	add := url.Values{
		"X-Amz-Algorithm":     {algorithm},
		"X-Amz-Credential":    {creds.AccessKeyID + "/" + scope},
		"X-Amz-Date":          {amzDate},
		"X-Amz-Expires":       {fmt.Sprint(int64(expires / time.Second))},
		"X-Amz-SignedHeaders": {"host"},
	}
	block, signed := "host:"+u.Host+"\n", "host"
	if size >= 0 {
		block, signed = "content-length:"+strconv.FormatInt(size, 10)+"\n"+block, "content-length;host"
		add.Set("X-Amz-SignedHeaders", signed)
	}
	if creds.SessionToken != "" {
		add.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	raw := u.RawQuery
	if raw != "" {
		raw += "&"
	}
	query, err := canonicalQuery(raw + add.Encode())
	if err != nil {
		return "", "", "", err
	}
	path, err := canonicalPath(u, service != "s3")
	if err != nil {
		return "", "", "", err
	}
	payload := EmptyPayloadHash
	if service == "s3" {
		payload = UnsignedPayload
	}
	creq = strings.Join([]string{m, path, query, block, signed, payload}, "\n")
	sts = stringToSign(amzDate, scope, creq)
	u.RawQuery = query + "&X-Amz-Signature=" + signature(creds.SecretAccessKey, now, region, service, sts)
	return u.String(), creq, sts, nil
}

// credentialScope is date/region/service/aws4_request.
func credentialScope(now time.Time, region, service string) string {
	return now.Format(dateFormat) + "/" + region + "/" + service + "/" + terminator
}

func stringToSign(amzDate, scope, creq string) string {
	sum := sha256.Sum256([]byte(creq))
	return algorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
}

// signature derives the signing key for the day, region and service, and
// signs sts with it.
func signature(secret string, now time.Time, region, service, sts string) string {
	k := hmacSHA256([]byte("AWS4"+secret), now.Format(dateFormat))
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, terminator)
	return hex.EncodeToString(hmacSHA256(k, sts))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// checkScope refuses credentials, regions and services that would break the
// Credential field or the scope.
func checkScope(creds Creds, region, service string) error {
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return errors.New("sigv4: no access key")
	}
	if !printable(creds.AccessKeyID) || strings.ContainsAny(creds.AccessKeyID, "/, ") {
		return errors.New("sigv4: access key id holds a character it may not")
	}
	if !printable(creds.SessionToken) {
		return errors.New("sigv4: session token holds a control character")
	}
	for _, s := range []string{region, service} {
		if len(s) == 0 || len(s) > 64 || strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return fmt.Errorf("sigv4: region and service are 1 to 64 of a-z, 0-9 and '-', not %q", s)
		}
	}
	return nil
}

// checkMethod returns the method, GET for "", if it is an HTTP token.
func checkMethod(m string) (string, error) {
	if m == "" {
		return http.MethodGet, nil
	}
	for i := 0; i < len(m); i++ {
		if !isTokenChar(m[i]) {
			return "", fmt.Errorf("sigv4: bad method %q", m)
		}
	}
	return m, nil
}

func validPayloadHash(h string) bool {
	if h == UnsignedPayload {
		return true
	}
	if len(h) != 64 {
		return false
	}
	return strings.Trim(h, "0123456789abcdef") == ""
}

// hashBody hashes r's body, through GetBody when it can so the body is not
// held twice, and otherwise by reading it (up to MaxHashedBody) and putting
// it back.
func hashBody(r *http.Request) (string, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return EmptyPayloadHash, nil
	}
	if r.GetBody != nil {
		b, err := r.GetBody()
		if err != nil {
			return "", fmt.Errorf("sigv4: %w", err)
		}
		defer b.Close()
		h := sha256.New()
		if _, err := io.Copy(h, b); err != nil {
			return "", fmt.Errorf("sigv4: reading the body: %w", err)
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxHashedBody+1))
	r.Body.Close()
	if err != nil {
		return "", fmt.Errorf("sigv4: reading the body: %w", err)
	}
	if len(body) > MaxHashedBody {
		return "", fmt.Errorf("sigv4: the body is over %d bytes; pass its hash", MaxHashedBody)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.ContentLength = int64(len(body))
	return PayloadHash(body), nil
}

// canonicalHeaders returns the canonical header block (each line ending in
// a newline) and the signed header names. Names are lowercased and sorted;
// each value is trimmed with runs of spaces and tabs squeezed to one; the
// values of a repeated name are joined with commas in the order they will be
// sent (Go writes header keys sorted).
func canonicalHeaders(host string, h http.Header) (block, signed string, err error) {
	vals := map[string][]string{"host": {host}}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		name := strings.ToLower(k)
		if name == "" || slices.Contains(ignoredHeaders, name) {
			continue
		}
		for i := 0; i < len(name); i++ {
			if !isTokenChar(name[i]) {
				return "", "", fmt.Errorf("sigv4: bad header name %q", k)
			}
		}
		for _, v := range h[k] {
			if strings.ContainsAny(v, "\r\n\x00") {
				return "", "", fmt.Errorf("sigv4: header %s holds a line break or NUL", k)
			}
			vals[name] = append(vals[name], squeeze(v))
		}
	}
	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + ":" + strings.Join(vals[n], ",") + "\n")
	}
	return b.String(), strings.Join(names, ";"), nil
}

// squeeze trims v and turns each run of spaces and tabs into one space.
func squeeze(v string) string {
	return strings.Join(strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == '\t' }), " ")
}

// canonicalPath URI-encodes each segment of u's path, keeping the slashes.
// A segment is decoded from the path as sent first, so an escaped slash
// stays inside its segment. With normalize (every service but S3), empty,
// "." and ".." segments are resolved as RFC 3986 and the AWS test suite do.
func canonicalPath(u *url.URL, normalize bool) (string, error) {
	if u.Opaque != "" {
		return "", errors.New("sigv4: opaque URLs are not supported")
	}
	p := u.EscapedPath()
	if p == "" {
		return "/", nil
	}
	if p[0] != '/' {
		return "", fmt.Errorf("sigv4: path %q is not absolute", p)
	}
	raw := strings.Split(p[1:], "/")
	segs := make([]string, 0, len(raw))
	for _, s := range raw {
		d, err := url.PathUnescape(s)
		if err != nil {
			return "", fmt.Errorf("sigv4: path: %w", err)
		}
		segs = append(segs, d)
	}
	if normalize {
		var out []string
		for _, s := range segs {
			switch s {
			case "", ".":
			case "..":
				if len(out) > 0 {
					out = out[:len(out)-1]
				}
			default:
				out = append(out, s)
			}
		}
		last := segs[len(segs)-1]
		if len(out) > 0 && (last == "" || last == "." || last == "..") {
			out = append(out, "")
		}
		segs = out
	}
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return "/" + strings.Join(segs, "/"), nil
}

// canonicalQuery decodes raw ('+' is a space, as in a form) and encodes it
// again, sorted by name and then value. A name with no '=' gets an empty
// value; empty parts are dropped.
func canonicalQuery(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	type pair struct{ k, v string }
	var ps []pair
	for part := range strings.SplitSeq(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		dk, err := url.QueryUnescape(k)
		if err != nil {
			return "", fmt.Errorf("sigv4: query: %w", err)
		}
		dv, err := url.QueryUnescape(v)
		if err != nil {
			return "", fmt.Errorf("sigv4: query: %w", err)
		}
		ps = append(ps, pair{uriEncode(dk), uriEncode(dv)})
	}
	slices.SortFunc(ps, func(a, b pair) int {
		if c := strings.Compare(a.k, b.k); c != 0 {
			return c
		}
		return strings.Compare(a.v, b.v)
	})
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&"), nil
}

// uriEncode is AWS's UriEncode: every byte but A-Z a-z 0-9 - _ . ~ becomes
// %XX with uppercase hex.
func uriEncode(s string) string {
	const hexdig = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexdig[c>>4])
		b.WriteByte(hexdig[c&15])
	}
	return b.String()
}

func isTokenChar(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// printable reports whether s is only printable ASCII.
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
