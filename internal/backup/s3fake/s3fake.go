// Package s3fake is an in-process S3-compatible store for tests: one
// bucket, kept on disk, that checks every request's SigV4 signature and
// payload hash the way S3 does. It speaks the part of the API the backup
// target uses (PUT, GET with Range, HEAD, DELETE, ListObjectsV2 and
// multipart uploads), with path-style and virtual-host addressing, and
// can be told to fail in the ways real stores do. Nothing here contacts a
// real service.
package s3fake

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/sigv4"
)

// Test credentials: never a real account's.
const (
	Bucket    = "mirrin-test"
	Region    = "us-east-1"
	AccessKey = "AKIDFAKES3EXAMPLE"
	SecretKey = "fake/secret/key/for/tests/only+0123456789"
)

// MinPartSize is the smallest part S3 takes, except the last.
const MinPartSize = 5 << 20

// Server is a fake S3 store.
type Server struct {
	*httptest.Server
	Bucket, Region, AccessKey, SecretKey string
	// MaxKeys caps a list page (default 1000), to test paging.
	MaxKeys int
	// Now is the store's clock (default time.Now).
	Now func() time.Time
	// NoConditional answers If-None-Match with 501, as some stores do.
	NoConditional bool
	// Fault, when set, sees each request after its signature checks and
	// may answer it instead of the store: it returns true when it did.
	Fault func(w http.ResponseWriter, r *http.Request) bool
	// CutGets breaks off that many GETs after CutAt bytes of the body.
	CutGets int
	CutAt   int64
	// StallGets stops sending, without closing, that many GETs after CutAt
	// bytes of the body, until the client gives up.
	StallGets int

	dir     string
	mu      sync.Mutex
	objects map[string]*object
	uploads map[string]*upload
	counts  map[string]int
}

type object struct {
	file string
	size int64
	etag string
	mod  time.Time
}

type upload struct {
	key   string
	dir   string
	parts map[int]part
}

type part struct {
	file string
	size int64
	etag string
}

// New starts a fake store that stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		Bucket: Bucket, Region: Region, AccessKey: AccessKey, SecretKey: SecretKey,
		MaxKeys: 1000, dir: t.TempDir(),
		objects: map[string]*object{}, uploads: map[string]*upload{}, counts: map[string]int{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Client reaches the store whatever hostname a request names, so a
// virtual-host bucket (bucket.s3.fake.test) needs no DNS.
func (s *Server) Client() *http.Client {
	addr := s.Listener.Addr().String()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
	}
	return &http.Client{Transport: tr}
}

// Port is the port the store listens on.
func (s *Server) Port() string {
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	return p
}

// Keys are the stored object keys, sorted.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for k := range s.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Object returns a stored object's bytes.
func (s *Server) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	o := s.objects[key]
	s.mu.Unlock()
	if o == nil {
		return nil, false
	}
	b, err := os.ReadFile(o.file)
	return b, err == nil
}

// PendingUploads counts multipart uploads neither completed nor aborted.
func (s *Server) PendingUploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

// Count is how many requests of a kind reached the store, after their
// signatures checked: "PUT", "GET", "HEAD", "DELETE", "LIST", "CREATE",
// "PART", "COMPLETE" or "ABORT".
func (s *Server) Count(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[op]
}

// Contains reports whether any byte the store holds, objects and parts
// alike, includes needle.
func (s *Server) Contains(needle []byte) bool {
	found := false
	_ = filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, needle) {
			found = true
		}
		return nil
	})
	return found
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func writeErr(w http.ResponseWriter, status int, code, msg string, extra ...string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>`)
	_ = xml.EscapeText(&b, []byte(msg))
	b.WriteString(`</Message>`)
	for i := 0; i+1 < len(extra); i += 2 {
		b.WriteString("<" + extra[i] + ">" + extra[i+1] + "</" + extra[i] + ">")
	}
	b.WriteString(`<RequestId>fake</RequestId></Error>`)
	if status != http.StatusNotModified {
		_, _ = io.WriteString(w, b.String())
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	key, ok := s.route(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	if !s.authorized(w, r) {
		return
	}
	if s.Fault != nil && s.Fault(w, r) {
		return
	}
	q := r.URL.Query()
	switch {
	case key == "" && r.Method == http.MethodGet && q.Get("list-type") == "2":
		s.count("LIST")
		s.list(w, q)
	case key == "":
		writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "not on the bucket")
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.count("CREATE")
		s.create(w, key)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		s.count("PART")
		s.putPart(w, r, key, q)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		s.count("COMPLETE")
		s.complete(w, r, key, q.Get("uploadId"))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		s.count("ABORT")
		s.abort(w, q.Get("uploadId"))
	case r.Method == http.MethodPut:
		s.count("PUT")
		s.put(w, r, key)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		s.count(r.Method)
		s.get(w, r, key)
	case r.Method == http.MethodDelete:
		s.count("DELETE")
		s.mu.Lock()
		if o := s.objects[key]; o != nil {
			os.Remove(o.file)
			delete(s.objects, key)
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (s *Server) count(op string) {
	s.mu.Lock()
	s.counts[op]++
	s.mu.Unlock()
}

// route finds the key in a virtual-host (bucket.host/key) or path-style
// (host/bucket/key) request; ok is false for another bucket.
func (s *Server) route(r *http.Request) (key string, ok bool) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	p := r.URL.Path
	if strings.HasPrefix(host, s.Bucket+".") {
		return strings.TrimPrefix(p, "/"), true
	}
	rest, found := strings.CutPrefix(p, "/"+s.Bucket)
	if !found || rest != "" && rest[0] != '/' {
		return "", false
	}
	return strings.TrimPrefix(rest, "/"), true
}

// authorized checks the signature as S3 does: the key, the region, the
// time, and the signature itself, recomputed from what arrived.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(auth, "AWS4-HMAC-SHA256 ")
	if !ok {
		writeErr(w, http.StatusForbidden, "AccessDenied", "Anonymous access is not allowed")
		return false
	}
	fields := map[string]string{}
	for f := range strings.SplitSeq(rest, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		fields[k] = v
	}
	cred := strings.Split(fields["Credential"], "/")
	if len(cred) != 5 || cred[0] != s.AccessKey {
		writeErr(w, http.StatusForbidden, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.")
		return false
	}
	if cred[2] != s.Region || cred[3] != "s3" {
		writeErr(w, http.StatusBadRequest, "AuthorizationHeaderMalformed", "the region '"+cred[2]+"' is wrong", "Region", s.Region)
		return false
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		writeErr(w, http.StatusForbidden, "AccessDenied", "no X-Amz-Date")
		return false
	}
	if d := s.now().Sub(at); d > 15*time.Minute || d < -15*time.Minute {
		w.Header().Set("Date", s.now().UTC().Format(http.TimeFormat))
		writeErr(w, http.StatusForbidden, "RequestTimeTooSkewed", "The difference between the request time and the current time is too large.")
		return false
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	check := &http.Request{
		Method: r.Method,
		URL:    &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery},
		Host:   r.Host,
		Header: http.Header{},
	}
	for _, h := range strings.Split(fields["SignedHeaders"], ";") {
		if h != "host" {
			check.Header[http.CanonicalHeaderKey(h)] = r.Header.Values(h)
		}
	}
	creds := sigv4.Creds{AccessKeyID: s.AccessKey, SecretAccessKey: s.SecretKey}
	if err := sigv4.Sign(check, creds, s.Region, "s3", at, payload); err != nil ||
		check.Header.Get("Authorization") != auth {
		writeErr(w, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		return false
	}
	return true
}

// receive writes the body to a new file in the store, checking it against
// the signed payload hash; it returns the file, size and MD5 ETag.
func (s *Server) receive(w http.ResponseWriter, r *http.Request) (file string, size int64, etag string, ok bool) {
	f, err := os.CreateTemp(s.dir, "body-*")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return "", 0, "", false
	}
	sh, mh := sha256.New(), md5.New()
	n, err := io.Copy(io.MultiWriter(f, sh, mh), r.Body)
	f.Close()
	if err != nil {
		os.Remove(f.Name())
		writeErr(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return "", 0, "", false
	}
	if p := r.Header.Get("X-Amz-Content-Sha256"); p != sigv4.UnsignedPayload && p != hex.EncodeToString(sh.Sum(nil)) {
		os.Remove(f.Name())
		writeErr(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
		return "", 0, "", false
	}
	return f.Name(), n, `"` + hex.EncodeToString(mh.Sum(nil)) + `"`, true
}

// conditional answers If-None-Match: * for key; ok is false when it did.
// The caller holds s.mu.
func (s *Server) conditional(w http.ResponseWriter, r *http.Request, key string) bool {
	inm := r.Header.Get("If-None-Match")
	if inm == "" {
		return true
	}
	if s.NoConditional {
		writeErr(w, http.StatusNotImplemented, "NotImplemented", "A header you provided implies functionality that is not implemented: If-None-Match")
		return false
	}
	if inm == "*" && s.objects[key] != nil {
		writeErr(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
		return false
	}
	return true
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, key string) {
	if s.NoConditional && r.Header.Get("If-None-Match") != "" {
		writeErr(w, http.StatusNotImplemented, "NotImplemented", "A header you provided implies functionality that is not implemented: If-None-Match")
		return
	}
	file, size, etag, ok := s.receive(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.conditional(w, r, key) {
		os.Remove(file)
		return
	}
	s.store(key, file, size, etag)
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// store keeps file as key; the caller holds s.mu.
func (s *Server) store(key, file string, size int64, etag string) {
	if old := s.objects[key]; old != nil {
		os.Remove(old.file)
	}
	s.objects[key] = &object{file: file, size: size, etag: etag, mod: s.now().UTC()}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	o := s.objects[key]
	cut, stall := false, false
	if o != nil && r.Method == http.MethodGet && s.CutGets > 0 && r.Header.Get("Range") == "" {
		s.CutGets--
		cut = true
	} else if o != nil && r.Method == http.MethodGet && s.StallGets > 0 && r.Header.Get("Range") == "" {
		s.StallGets--
		stall = true
	}
	s.mu.Unlock()
	if o == nil {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeErr(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}
	if im := r.Header.Get("If-Match"); im != "" && im != o.etag {
		writeErr(w, http.StatusPreconditionFailed, "PreconditionFailed", "If-Match")
		return
	}
	f, err := os.Open(o.file)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("ETag", o.etag)
	h.Set("Last-Modified", o.mod.Format(http.TimeFormat))
	h.Set("Content-Type", "application/octet-stream")
	start, status := int64(0), http.StatusOK
	if rg := r.Header.Get("Range"); rg != "" {
		from, ok := strings.CutPrefix(rg, "bytes=")
		from, open := strings.CutSuffix(from, "-")
		n, err := strconv.ParseInt(from, 10, 64)
		if !ok || !open || err != nil || n < 0 || n >= o.size {
			writeErr(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable")
			return
		}
		start, status = n, http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", n, o.size-1, o.size))
	}
	h.Set("Content-Length", strconv.FormatInt(o.size-start, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return
	}
	if stall {
		_, _ = io.CopyN(w, f, s.CutAt)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
		return
	}
	if cut {
		_, _ = io.CopyN(w, f, s.CutAt)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		panic(http.ErrAbortHandler) // drops the connection mid-body
	}
	_, _ = io.Copy(w, f)
}

func (s *Server) list(w http.ResponseWriter, q url.Values) {
	prefix, delim, after := q.Get("prefix"), q.Get("delimiter"), q.Get("continuation-token")
	max := s.MaxKeys
	if m, err := strconv.Atoi(q.Get("max-keys")); err == nil && m >= 0 && m < max {
		max = m
	}
	s.mu.Lock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	objs := make(map[string]object, len(s.objects))
	for k, o := range s.objects {
		objs[k] = *o
	}
	s.mu.Unlock()
	sort.Strings(keys)
	type content struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	}
	type commonPrefix struct {
		Prefix string `xml:"Prefix"`
	}
	var res struct {
		XMLName               xml.Name       `xml:"ListBucketResult"`
		Name                  string         `xml:"Name"`
		Prefix                string         `xml:"Prefix"`
		KeyCount              int            `xml:"KeyCount"`
		MaxKeys               int            `xml:"MaxKeys"`
		IsTruncated           bool           `xml:"IsTruncated"`
		Contents              []content      `xml:"Contents"`
		CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
		NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	}
	res.Name, res.Prefix, res.MaxKeys = s.Bucket, prefix, max
	seen := map[string]bool{}
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || after != "" && k <= after {
			continue
		}
		if res.KeyCount >= max {
			res.IsTruncated = true
			break
		}
		if delim != "" {
			if i := strings.Index(k[len(prefix):], delim); i >= 0 {
				cp := k[:len(prefix)+i+len(delim)]
				if !seen[cp] {
					seen[cp] = true
					res.CommonPrefixes = append(res.CommonPrefixes, commonPrefix{cp})
				}
				continue
			}
		}
		o := objs[k]
		res.Contents = append(res.Contents, content{k, o.mod.Format("2006-01-02T15:04:05.000Z"), o.etag, o.size, "STANDARD"})
		res.KeyCount++
		res.NextContinuationToken = k
	}
	if !res.IsTruncated {
		res.NextContinuationToken = ""
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(res)
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Server) create(w http.ResponseWriter, key string) {
	id := newID()
	dir := filepath.Join(s.dir, "upload-"+id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	s.mu.Lock()
	s.uploads[id] = &upload{key: key, dir: dir, parts: map[int]part{}}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `%s<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`,
		xml.Header, s.Bucket, key, id)
}

func (s *Server) putPart(w http.ResponseWriter, r *http.Request, key string, q url.Values) {
	num, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil || num < 1 || num > 10000 {
		writeErr(w, http.StatusBadRequest, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive")
		return
	}
	s.mu.Lock()
	up := s.uploads[q.Get("uploadId")]
	s.mu.Unlock()
	if up == nil || up.key != key {
		writeErr(w, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.")
		return
	}
	file, size, etag, ok := s.receive(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	if old, ok := up.parts[num]; ok {
		os.Remove(old.file)
	}
	up.parts[num] = part{file: file, size: size, etag: etag}
	s.mu.Unlock()
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request, key, id string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	if p := r.Header.Get("X-Amz-Content-Sha256"); p != sigv4.UnsignedPayload && p != sigv4.PayloadHash(body) {
		writeErr(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "payload hash")
		return
	}
	var doc struct {
		Parts []struct {
			Number int    `xml:"PartNumber"`
			ETag   string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil || len(doc.Parts) == 0 {
		writeErr(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	up := s.uploads[id]
	if up == nil || up.key != key {
		writeErr(w, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.")
		return
	}
	if !s.conditional(w, r, key) {
		return
	}
	out, err := os.CreateTemp(s.dir, "object-*")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	var size int64
	var sums hash.Hash = md5.New()
	fail := func(status int, code, msg string) {
		out.Close()
		os.Remove(out.Name())
		writeErr(w, status, code, msg)
	}
	for i, p := range doc.Parts {
		got, ok := up.parts[p.Number]
		switch {
		case !ok || got.etag != p.ETag:
			fail(http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found.")
			return
		case i > 0 && p.Number <= doc.Parts[i-1].Number:
			fail(http.StatusBadRequest, "InvalidPartOrder", "The list of parts was not in ascending order.")
			return
		case i < len(doc.Parts)-1 && got.size < MinPartSize:
			fail(http.StatusBadRequest, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.")
			return
		}
		f, err := os.Open(got.file)
		if err != nil {
			fail(http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		n, err := io.Copy(out, f)
		f.Close()
		if err != nil {
			fail(http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		size += n
		raw, _ := hex.DecodeString(strings.Trim(got.etag, `"`))
		sums.Write(raw)
	}
	if err := out.Close(); err != nil {
		fail(http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	etag := fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(sums.Sum(nil)), len(doc.Parts))
	s.store(key, out.Name(), size, etag)
	os.RemoveAll(up.dir)
	for _, p := range up.parts {
		os.Remove(p.file)
	}
	delete(s.uploads, id)
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `%s<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`,
		xml.Header, s.Bucket, key, strings.ReplaceAll(etag, `"`, "&quot;"))
}

func (s *Server) abort(w http.ResponseWriter, id string) {
	s.mu.Lock()
	up := s.uploads[id]
	delete(s.uploads, id)
	s.mu.Unlock()
	if up == nil {
		writeErr(w, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.")
		return
	}
	for _, p := range up.parts {
		os.Remove(p.file)
	}
	os.RemoveAll(up.dir)
	w.WriteHeader(http.StatusNoContent)
}

// ErrorBody is an S3 error document, for a Fault that answers with one.
func ErrorBody(code, msg string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>`)
	_ = xml.EscapeText(&b, []byte(msg))
	b.WriteString(`</Message></Error>`)
	return b.String()
}

// WriteError answers with an S3 error, for a Fault.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	writeErr(w, status, code, msg)
}
