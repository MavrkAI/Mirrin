package backup

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/sigv4"
)

// S3Config names an S3-compatible bucket: AWS S3, Cloudflare R2, Backblaze
// B2, MinIO, Wasabi and the like. The keys themselves are never in it, only
// the names of the environment variables (or secrets.env entries, see
// config.Secret) that hold them.
type S3Config struct {
	// Endpoint is the store's address, such as
	// https://<account>.r2.cloudflarestorage.com. Empty means AWS S3 in
	// Region.
	Endpoint string
	// Region signs the requests. Empty is worked out from the endpoint
	// (auto for R2, the region in B2, Wasabi and AWS hostnames), else
	// us-east-1, which MinIO expects.
	Region string
	Bucket string
	// Prefix is the folder in the bucket objects go in ("" for the top).
	Prefix string
	// AccessKeyEnv and SecretKeyEnv name the variables holding the key pair.
	AccessKeyEnv, SecretKeyEnv string
	// PathStyle puts the bucket in the path (https://host/bucket/key)
	// instead of the hostname (https://bucket.host/key). MinIO usually
	// needs it; a bucket name that can't be a hostname gets it anyway.
	PathStyle bool
}

// Defaults for S3 settings the owner leaves out.
const (
	DefaultS3AccessKeyEnv = config.DefaultS3AccessKeyEnv
	DefaultS3SecretKeyEnv = config.DefaultS3SecretKeyEnv
	defaultS3Region       = "us-east-1"
)

// Upload sizes: one PUT below s3MultipartThreshold, parts of at least
// s3PartSize above it (bigger when an object would need over 10,000).
const (
	s3MultipartThreshold = 64 << 20
	s3PartSize           = 16 << 20
	s3MaxParts           = 10000
	s3Attempts           = 5
	s3MaxErrorBody       = 64 << 10
	// s3Idle is how long a request may go without a byte sent or received
	// before it is dropped and tried again (a download resumes).
	s3Idle = 2 * time.Minute
)

// s3Target keeps objects in an S3-compatible bucket.
type s3Target struct {
	cfg    S3Config
	region string
	// base is the bucket's address: https://bucket.host[/path] or
	// https://host[/path]/bucket. Keys go after basePath + "/".
	base     url.URL
	basePath string
	prefix   string // "" or "a/b/"
	hc       *http.Client
	err      error // a config that can't work; every call returns it

	threshold, partSize int64
	idle                time.Duration
	secret              func(name string) string
	sleep               func(ctx context.Context, d time.Duration) error
	now                 func() time.Time
	skew                atomic.Int64 // store clock minus ours, in ns
	noConditional       atomic.Bool  // the store refused If-None-Match
}

// S3 is a target in an S3-compatible bucket, using hc (nil for a default
// client). A config that can't work gives a target whose every call says
// why. Objects above 64 MiB go up in parts; a failed request is tried
// again; a download that breaks off resumes where it stopped.
func S3(c S3Config, hc *http.Client) Target {
	t, err := newS3(c, hc)
	if err != nil {
		return &s3Target{cfg: c, err: err}
	}
	return t
}

func newS3(c S3Config, hc *http.Client) (*s3Target, error) {
	c.Bucket = strings.TrimSpace(c.Bucket)
	if c.AccessKeyEnv == "" {
		c.AccessKeyEnv = DefaultS3AccessKeyEnv
	}
	if c.SecretKeyEnv == "" {
		c.SecretKeyEnv = DefaultS3SecretKeyEnv
	}
	if !reBucket.MatchString(c.Bucket) || strings.Contains(c.Bucket, "..") {
		return nil, fmt.Errorf("%q isn't a bucket name: 3 to 63 letters, digits, dots, dashes or underscores", c.Bucket)
	}
	for _, n := range []string{c.AccessKeyEnv, c.SecretKeyEnv} {
		if !reEnvName.MatchString(n) {
			return nil, fmt.Errorf("%q isn't an environment variable name", n)
		}
	}
	prefix, err := cleanPrefix(c.Prefix)
	if err != nil {
		return nil, err
	}
	ep, err := S3Endpoint(c.Endpoint, c.Region)
	if err != nil {
		return nil, err
	}
	region := c.Region
	if region == "" {
		region = S3Region(ep.Hostname())
	}
	if region == "" || len(region) > 64 || strings.Trim(region, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return nil, fmt.Errorf("%q isn't a region name (such as us-east-1, or auto for R2)", region)
	}
	t := &s3Target{
		cfg: c, region: region, prefix: prefix,
		threshold: s3MultipartThreshold, partSize: s3PartSize, idle: s3Idle,
		secret: config.Secret, sleep: sleepCtx, now: time.Now,
	}
	t.base = *ep
	t.basePath = strings.TrimSuffix(ep.Path, "/")
	if c.PathStyle || !hostableBucket(c.Bucket, ep) {
		t.basePath += "/" + c.Bucket
	} else {
		t.base.Host = c.Bucket + "." + ep.Host
	}
	t.base.Path, t.base.RawPath = "", ""
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = 2 * time.Minute
		hc = &http.Client{Transport: tr}
	}
	// A signed request can't follow a redirect to another host; say where
	// the bucket is instead.
	own := *hc
	own.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.hc = &own
	return t, nil
}

var (
	reBucket  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,61}[A-Za-z0-9]$`)
	reEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	reDNSName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)
)

// hostableBucket reports whether the bucket can go in the hostname: a DNS
// label (no dots over HTTPS, where the certificate wouldn't match) on a
// named host, not an IP address or localhost.
func hostableBucket(b string, ep *url.URL) bool {
	h := ep.Hostname()
	if net.ParseIP(h) != nil || h == "localhost" || !strings.Contains(h, ".") {
		return false
	}
	if ep.Scheme == "https" && strings.Contains(b, ".") {
		return false
	}
	for l := range strings.SplitSeq(b, ".") {
		if !reDNSName.MatchString(l) {
			return false
		}
	}
	return true
}

// cleanPrefix checks a folder path in a bucket and returns it with one
// trailing slash, or "".
func cleanPrefix(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	for s := range strings.SplitSeq(p, "/") {
		if !reSegment.MatchString(s) || s == "." || s == ".." {
			return "", fmt.Errorf("%q isn't a folder in a bucket: use letters, digits, '.', '-' and '_' between slashes", p)
		}
	}
	return p + "/", nil
}

// S3Endpoint is the store's address: https:// is assumed, and AWS S3 in
// region is the default.
func S3Endpoint(endpoint, region string) (*url.URL, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		if region == "" {
			region = defaultS3Region
		}
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("%q isn't a store address; give one like https://s3.us-west-004.backblazeb2.com", endpoint)
	}
	if p := u.Port(); p == "443" && u.Scheme == "https" || p == "80" && u.Scheme == "http" {
		u.Host = u.Hostname()
	}
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "http" && !localHost(u.Hostname()) {
		return nil, fmt.Errorf("%s would carry the bucket's key id and signed requests in the clear; use https://, or http:// only for a store on this machine or the local network (localhost, a private IP, or a name like minio or nas.local)", endpoint)
	}
	return u, nil
}

// localHost reports whether h is on this machine or the local network: a
// loopback, private or link-local address, a name without dots, or one
// under a suffix that never reaches the public internet.
func localHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if !strings.Contains(h, ".") {
		return true
	}
	for _, sfx := range []string{".localhost", ".local", ".lan", ".internal", ".home.arpa", ".test"} {
		if strings.HasSuffix(h, sfx) {
			return true
		}
	}
	return false
}

// S3Region works a signing region out of a store's hostname: auto for
// Cloudflare R2, the region in AWS, Backblaze B2 and Wasabi hostnames, and
// us-east-1 otherwise (what MinIO expects by default).
func S3Region(host string) string {
	h := strings.ToLower(host)
	if strings.HasSuffix(h, ".r2.cloudflarestorage.com") {
		return "auto"
	}
	if !strings.HasSuffix(h, ".amazonaws.com") && !strings.HasSuffix(h, ".backblazeb2.com") && !strings.HasSuffix(h, ".wasabisys.com") {
		return defaultS3Region
	}
	// s3.<region>.…, s3-<region>.…, s3.dualstack.<region>.…,
	// bucket.s3.<region>.…; plain s3.amazonaws.com is us-east-1.
	labels := strings.Split(h, ".")
	labels = labels[:len(labels)-2]
	for i, l := range labels {
		switch {
		case l == "s3" || l == "s3-fips":
			rest := labels[i+1:]
			if len(rest) > 0 && rest[0] == "dualstack" {
				rest = rest[1:]
			}
			if len(rest) > 0 {
				return rest[0]
			}
			return defaultS3Region
		case l == "s3-external-1" || l == "s3-accelerate":
			return defaultS3Region
		case strings.HasPrefix(l, "s3-"):
			return strings.TrimPrefix(l, "s3-")
		}
	}
	return defaultS3Region
}

func (s *s3Target) String() string {
	b := s.cfg.Bucket
	where := strings.TrimSuffix("s3://"+b+"/"+strings.Trim(s.prefix, "/"), "/")
	if s.err != nil {
		return where
	}
	if !strings.HasSuffix(s.base.Hostname(), ".amazonaws.com") {
		host := strings.TrimPrefix(s.base.Host, b+".")
		where += " on " + host
	}
	return where
}

// key is the object key for a backup name.
func (s *s3Target) key(name string) (string, error) {
	if _, _, ok := parseName(name); !ok {
		return "", fmt.Errorf("%q isn't a backup name", name)
	}
	return s.prefix + name, nil
}

// s3Body is a request body that can be sent again: open makes a fresh
// reader over the same bytes.
type s3Body struct {
	open func() io.Reader
	size int64
	hash string // SHA-256, hex, for the signature
	md5  string // MD5, hex: the ETag a store without SSE-KMS gives it
}

func bytesBody(b []byte) s3Body {
	m := md5.Sum(b)
	return s3Body{open: func() io.Reader { return bytes.NewReader(b) }, size: int64(len(b)), hash: sigv4.PayloadHash(b), md5: hex.EncodeToString(m[:])}
}

// sectionBody is size bytes of ra from off, hashed by reading them once.
func sectionBody(ctx context.Context, ra io.ReaderAt, off, size int64) (s3Body, error) {
	h, m := sha256.New(), md5.New()
	n, err := io.Copy(io.MultiWriter(h, m), ctxReader{ctx, io.NewSectionReader(ra, off, size)})
	if err != nil {
		return s3Body{}, err
	}
	return s3Body{
		open: func() io.Reader { return io.NewSectionReader(ra, off, n) },
		size: n, hash: hex.EncodeToString(h.Sum(nil)), md5: hex.EncodeToString(m.Sum(nil)),
	}, nil
}

// s3Request is one call to the store.
type s3Request struct {
	method string
	key    string // "" for the bucket itself
	query  url.Values
	header http.Header
	body   *s3Body
	// conditional marks a request carrying If-None-Match: *, which is
	// dropped for stores that don't support it.
	conditional bool
}

// do sends rq, trying again after network trouble, throttling, the
// store's own errors and a clock that is off, and returns a response with
// a 2xx or 3xx status, or an *s3Error.
func (s *s3Target) do(ctx context.Context, rq s3Request) (*http.Response, error) {
	var last error
	for attempt := 0; attempt < s3Attempts; attempt++ {
		if attempt > 0 {
			if err := s.sleep(ctx, backoff(attempt, last)); err != nil {
				return nil, err
			}
		}
		w := s.watch(ctx)
		req, err := s.request(w.ctx, rq, w.kick)
		if err != nil {
			w.stop()
			return nil, err
		}
		resp, err := s.hc.Do(req)
		if err != nil {
			w.stop()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(context.Cause(w.ctx), errStalled) {
				last = fmt.Errorf("%s stopped answering for %v", s.base.Host, s.idle)
			} else {
				last = fmt.Errorf("couldn't reach %s (%v)", s.base.Host, netReason(err))
			}
			continue
		}
		resp.Body = &watchedBody{ReadCloser: resp.Body, w: w}
		if resp.StatusCode < 300 {
			return resp, nil
		}
		e := s.readError(resp)
		last = e
		switch {
		case (e.Code == "RequestTimeTooSkewed" || e.Status == http.StatusForbidden) && s.adjustClock(resp):
			// Signed again with the store's time on the next try.
		case rq.conditional && !s.noConditional.Load() && (e.Status == http.StatusNotImplemented ||
			e.Code == "NotImplemented" && strings.Contains(strings.ToLower(e.Message), "if-none-match")):
			s.noConditional.Store(true)
			attempt-- // not the store's fault; try once more without it
		case retryable(e):
		default:
			return nil, e
		}
	}
	if e, ok := last.(*s3Error); ok {
		e.tries = s3Attempts
		return nil, e
	}
	return nil, fmt.Errorf("%v, %d times", last, s3Attempts)
}

// errStalled ends a request that went s3Target.idle without progress.
var errStalled = errors.New("the store stopped sending")

// watchdog cancels one attempt's context when no byte has gone either way
// for the target's idle time, so a store that stalls mid-body can't hang a
// backup or restore; the attempt is then tried again or resumed.
type watchdog struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	idle   time.Duration
	last   atomic.Int64 // UnixNano of the last progress
	mu     sync.Mutex
	done   bool
	t      *time.Timer
}

func (s *s3Target) watch(ctx context.Context) *watchdog {
	w := &watchdog{idle: s.idle}
	w.ctx, w.cancel = context.WithCancelCause(ctx)
	if w.idle > 0 {
		w.kick()
		w.mu.Lock()
		w.t = time.AfterFunc(w.idle, w.fire)
		w.mu.Unlock()
	}
	return w
}

func (w *watchdog) kick() { w.last.Store(time.Now().UnixNano()) }

func (w *watchdog) fire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return
	}
	if left := w.idle - time.Since(time.Unix(0, w.last.Load())); left > 0 {
		w.t.Reset(left)
		return
	}
	w.cancel(errStalled)
}

func (w *watchdog) stop() {
	w.mu.Lock()
	w.done = true
	if w.t != nil {
		w.t.Stop()
	}
	w.mu.Unlock()
	w.cancel(nil)
}

// watchedBody counts reads of a response as progress, and ends the
// attempt when closed.
type watchedBody struct {
	io.ReadCloser
	w    *watchdog
	once sync.Once
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.w.kick()
	}
	if err != nil && err != io.EOF && errors.Is(context.Cause(b.w.ctx), errStalled) {
		err = fmt.Errorf("%w for %v", errStalled, b.w.idle)
	}
	return n, err
}

func (b *watchedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.w.stop)
	return err
}

// progressReader calls kick whenever a request body is read.
type progressReader struct {
	r    io.Reader
	kick func()
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.kick()
	}
	return n, err
}

// request builds and signs one attempt at rq; kick is called as its body
// is read.
func (s *s3Target) request(ctx context.Context, rq s3Request, kick func()) (*http.Request, error) {
	creds, err := s.creds()
	if err != nil {
		return nil, err
	}
	u := s.base
	p := s.basePath + "/" + rq.key
	u.Path, u.RawPath = p, encodePath(p)
	u.RawQuery = rq.query.Encode()
	req, err := http.NewRequestWithContext(ctx, rq.method, s.base.String(), nil)
	if err != nil {
		return nil, err
	}
	req.URL, req.Host = &u, u.Host
	for k, v := range rq.header {
		req.Header[k] = append([]string(nil), v...)
	}
	if rq.conditional && !s.noConditional.Load() {
		req.Header.Set("If-None-Match", "*")
	}
	hash := sigv4.EmptyPayloadHash
	if rq.body != nil {
		hash = rq.body.hash
		req.ContentLength = rq.body.size
		if rq.body.size > 0 {
			open := rq.body.open
			body := func() io.ReadCloser { return io.NopCloser(progressReader{ctxReader{ctx, open()}, kick}) }
			req.Body = body()
			req.GetBody = func() (io.ReadCloser, error) { return body(), nil }
		} else {
			req.Body = http.NoBody
		}
	}
	if err := sigv4.Sign(req, creds, s.region, "s3", s.clock(), hash); err != nil {
		return nil, err
	}
	return req, nil
}

// creds reads the key pair each time, so a key saved while the twin runs
// is used without a restart.
func (s *s3Target) creds() (sigv4.Creds, error) {
	id, key := s.secret(s.cfg.AccessKeyEnv), s.secret(s.cfg.SecretKeyEnv)
	switch {
	case id == "" && key == "":
		return sigv4.Creds{}, fmt.Errorf("the key for %s isn't set: put its access key id in %s and its secret in %s (exported, or saved with `mirrin backup target s3 …`)", s, s.cfg.AccessKeyEnv, s.cfg.SecretKeyEnv)
	case id == "":
		return sigv4.Creds{}, fmt.Errorf("the access key id for %s isn't set: put it in %s", s, s.cfg.AccessKeyEnv)
	case key == "":
		return sigv4.Creds{}, fmt.Errorf("the secret key for %s isn't set: put it in %s", s, s.cfg.SecretKeyEnv)
	}
	return sigv4.Creds{AccessKeyID: strings.TrimSpace(id), SecretAccessKey: strings.TrimSpace(key)}, nil
}

func (s *s3Target) clock() time.Time {
	return s.now().Add(time.Duration(s.skew.Load()))
}

// adjustClock takes the store's time from its Date header when this
// machine's clock is off by more than a signature allows (15 minutes; an
// answer to HEAD has no error code to say so). It reports whether it
// changed anything, so a refusal for another reason isn't tried again.
func (s *s3Target) adjustClock(resp *http.Response) bool {
	d, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return false
	}
	if off := d.Sub(s.clock()); off > -10*time.Minute && off < 10*time.Minute {
		return false
	}
	s.skew.Store(int64(d.Sub(s.now())))
	return true
}

// encodePath is AWS's UriEncode of each path segment.
func encodePath(p string) string {
	const hexdig = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("-_.~/", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexdig[c>>4])
		b.WriteByte(hexdig[c&15])
	}
	return b.String()
}

// backoff waits longer after each try, with jitter, and as long as the
// store asked when it did (capped).
func backoff(attempt int, last error) time.Duration {
	d := 250 * time.Millisecond << (attempt - 1)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	var e *s3Error
	if errors.As(last, &e) && e.retryAfter > d {
		d = min(e.retryAfter, 30*time.Second)
	}
	return d/2 + rand.N(d/2+1)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// netReason is a network error without the URL Go wraps it in.
func netReason(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// s3Error is an error the store returned.
type s3Error struct {
	Status            int
	Code, Message     string
	Region            string // where the bucket is, when the store says
	bucket, where     string
	accessEnv, secEnv string
	retryAfter        time.Duration
	tries             int
}

func (s *s3Target) readError(resp *http.Response) *s3Error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, s3MaxErrorBody))
	e := parseS3Error(resp.StatusCode, body)
	if e.Region == "" {
		e.Region = resp.Header.Get("X-Amz-Bucket-Region")
	}
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		e.retryAfter = time.Duration(secs) * time.Second
	}
	e.bucket, e.where = s.cfg.Bucket, s.String()
	e.accessEnv, e.secEnv = s.cfg.AccessKeyEnv, s.cfg.SecretKeyEnv
	return e
}

func parseS3Error(status int, body []byte) *s3Error {
	var x struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
		Region  string `xml:"Region"`
	}
	_ = xml.Unmarshal(body, &x)
	return &s3Error{Status: status, Code: clip(x.Code, 64), Message: clip(x.Message, 200), Region: clip(x.Region, 64)}
}

// clip keeps printable text only, and not much of it: what a store says is
// shown to the owner.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func retryable(e *s3Error) bool {
	switch e.Status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	switch e.Code {
	case "RequestTimeout", "SlowDown", "InternalError", "ServiceUnavailable", "OperationAborted":
		return true
	}
	return false
}

func (e *s3Error) Error() string {
	var msg string
	switch {
	case e.Code == "NoSuchBucket":
		msg = fmt.Sprintf("the bucket %s isn't there; make it first, or check its name and the store's address", e.bucket)
	case e.Code == "InvalidAccessKeyId":
		msg = fmt.Sprintf("the store doesn't know the access key id in %s; check it", e.accessEnv)
	case e.Code == "SignatureDoesNotMatch":
		msg = fmt.Sprintf("the secret key in %s doesn't match the access key id in %s; check both", e.secEnv, e.accessEnv)
	case e.Code == "AuthorizationHeaderMalformed" || e.Code == "PermanentRedirect" || e.Code == "IllegalLocationConstraintException" ||
		e.Status == http.StatusMovedPermanently || e.Status == http.StatusTemporaryRedirect:
		if e.Region != "" {
			msg = fmt.Sprintf("the bucket %s is in region %s; set that region (--region %s)", e.bucket, e.Region, e.Region)
		} else {
			msg = fmt.Sprintf("the bucket %s is in another region or needs its own address; set the region or endpoint", e.bucket)
		}
	case e.Code == "AccessDenied" || e.Status == http.StatusForbidden:
		msg = fmt.Sprintf("the store refused the key in %s for %s; it needs to read, write, list and delete in that bucket", e.accessEnv, e.where)
	case e.Code == "PreconditionFailed" || e.Status == http.StatusPreconditionFailed:
		msg = "an object of that name is already there"
	case e.Code == "QuotaExceeded" || e.Code == "EntityTooLarge" || e.Status == http.StatusRequestEntityTooLarge:
		msg = fmt.Sprintf("%s is full or won't take an object this big", e.where)
	default:
		msg = fmt.Sprintf("the store said %s", e.describe())
	}
	if e.tries > 1 {
		msg += fmt.Sprintf(" (after %d tries)", e.tries)
	}
	return msg
}

func (e *s3Error) describe() string {
	var parts []string
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	s := strings.Join(parts, ": ")
	if s == "" {
		s = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d)", s, e.Status)
}

// notFound reports a missing object, not a missing bucket.
func notFound(err error) bool {
	var e *s3Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code != "NoSuchBucket" && e.Code != "NoSuchUpload"
}

// exists reports whether key is in the bucket, and its size.
func (s *s3Target) exists(ctx context.Context, key string) (bool, int64, error) {
	h, n, err := s.head(ctx, key)
	return h != nil, n, err
}

// head is key's headers and size, or nil headers when it isn't there.
func (s *s3Target) head(ctx context.Context, key string) (http.Header, int64, error) {
	resp, err := s.do(ctx, s3Request{method: http.MethodHead, key: key})
	if notFound(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	resp.Body.Close()
	return resp.Header, resp.ContentLength, nil
}

func (s *s3Target) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	if s.err != nil {
		return s.err
	}
	key, err := s.key(name)
	if err != nil {
		return err
	}
	if there, _, err := s.exists(ctx, key); err != nil {
		return err
	} else if there {
		return fmt.Errorf("%s is already there", name)
	}
	// A file (what the engine hands over) is read in place, twice: once to
	// hash each piece and once to send it. Anything else is read a piece
	// at a time into memory.
	if ra, ok := r.(interface {
		io.ReaderAt
		io.Seeker
	}); ok && size >= 0 {
		base, err := ra.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		var one [1]byte
		if n, _ := ra.ReadAt(one[:], base+size); n > 0 {
			return fmt.Errorf("the backup is longer than the %d bytes it should be", size)
		}
		piece := func(off, n int64) (s3Body, error) { return sectionBody(ctx, ra, base+off, n) }
		if size < s.threshold {
			b, err := piece(0, size)
			if err != nil {
				return err
			}
			if b.size != size {
				return fmt.Errorf("the backup ended after %d of %d bytes", b.size, size)
			}
			return s.putOne(ctx, key, b)
		}
		return s.putParts(ctx, key, size, func(off, n int64) (s3Body, error) {
			b, err := piece(off, n)
			if err == nil && b.size != n {
				err = fmt.Errorf("the backup ended early, at %d of %d bytes", off+b.size, size)
			}
			return b, err
		})
	}
	r = ctxReader{ctx, r}
	if size >= 0 && size < s.threshold {
		buf := make([]byte, size)
		if n, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("the backup ended after %d of %d bytes (%v)", n, size, err)
		}
		if err := noMore(r, size); err != nil {
			return err
		}
		return s.putOne(ctx, key, bytesBody(buf))
	}
	ps := s.partFor(size)
	buf := make([]byte, ps)
	if size < 0 {
		// Unknown length: one PUT if it all fits in the first piece.
		n, err := io.ReadFull(r, buf)
		switch {
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
			return s.putOne(ctx, key, bytesBody(buf[:n]))
		case err != nil:
			return err
		}
		first := true
		return s.putParts(ctx, key, -1, func(off, _ int64) (s3Body, error) {
			if first {
				first = false
				return bytesBody(buf), nil
			}
			n, err := io.ReadFull(r, buf)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				err = nil
			}
			return bytesBody(buf[:n]), err
		})
	}
	return s.putParts(ctx, key, size, func(off, n int64) (s3Body, error) {
		got, err := io.ReadFull(r, buf[:n])
		if err != nil {
			return s3Body{}, fmt.Errorf("the backup ended early, at %d of %d bytes (%v)", off+int64(got), size, err)
		}
		if off+n == size {
			if err := noMore(r, size); err != nil {
				return s3Body{}, err
			}
		}
		return bytesBody(buf[:n]), nil
	})
}

// noMore fails if r has more than the size bytes already read.
func noMore(r io.Reader, size int64) error {
	var one [1]byte
	if n, _ := io.ReadFull(r, one[:]); n > 0 {
		return fmt.Errorf("the backup is longer than the %d bytes it should be", size)
	}
	return nil
}

// partFor is the part size for an object of size bytes (-1: unknown).
func (s *s3Target) partFor(size int64) int64 {
	ps := s.partSize
	if size > ps*s3MaxParts {
		ps = (size + s3MaxParts - 1) / s3MaxParts
		ps = (ps + 1<<20 - 1) &^ (1<<20 - 1)
	}
	return ps
}

// putOne stores key in one request.
func (s *s3Target) putOne(ctx context.Context, key string, b s3Body) error {
	h := http.Header{"Content-Type": {"application/octet-stream"}}
	resp, err := s.do(ctx, s3Request{method: http.MethodPut, key: key, header: h, body: &b, conditional: true})
	if err != nil {
		return s.settle(ctx, key, b.size, `"`+b.md5+`"`, err)
	}
	resp.Body.Close()
	return nil
}

// settle decides a refused write. When an earlier try that seemed to fail
// had in fact stored the object, the retry is refused: a PUT or Complete
// because the precondition fails, a Complete also because the upload is
// finished and gone (NoSuchUpload). The object is ours when it has the
// size and ETag (etag, "" when unknown) of what was sent.
func (s *s3Target) settle(ctx context.Context, key string, size int64, etag string, err error) error {
	var e *s3Error
	if !errors.As(err, &e) || !(e.Status == http.StatusPreconditionFailed || e.Code == "PreconditionFailed" || e.Code == "NoSuchUpload") {
		return err
	}
	if h, n, herr := s.head(ctx, key); herr == nil && h != nil && n == size && sameETag(h, etag) {
		return nil
	}
	return err
}

// sameETag reports whether the object with headers h has the ETag etag.
// An object encrypted with a KMS or customer key has an ETag that isn't
// an MD5, so only its size can say.
func sameETag(h http.Header, etag string) bool {
	got := strings.TrimPrefix(h.Get("ETag"), "W/")
	if etag == "" || got == "" {
		return true
	}
	if strings.EqualFold(strings.Trim(got, `"`), strings.Trim(etag, `"`)) {
		return true
	}
	sse := strings.ToLower(h.Get("X-Amz-Server-Side-Encryption"))
	return strings.HasPrefix(sse, "aws:kms") || h.Get("X-Amz-Server-Side-Encryption-Customer-Algorithm") != ""
}

// multipartETag is the ETag S3 gives an object made of parts: the MD5 of
// the parts' MD5s, a dash and how many there were; "" when a part's ETag
// isn't an MD5.
func multipartETag(parts []completedPart) string {
	m := md5.New()
	for _, p := range parts {
		raw, err := hex.DecodeString(strings.Trim(p.ETag, `"`))
		if err != nil || len(raw) != md5.Size {
			return ""
		}
		m.Write(raw)
	}
	return fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(m.Sum(nil)), len(parts))
}

// putParts stores key as a multipart upload. next returns the piece at
// off, n bytes long (n is the part size, less for the last; for an unknown
// size the piece is as long as it is, and an empty one ends the upload).
func (s *s3Target) putParts(ctx context.Context, key string, size int64, next func(off, n int64) (s3Body, error)) (err error) {
	id, err := s.startUpload(ctx, key)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// Leave no parts behind to be billed for; this runs even when
			// ctx has ended.
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if resp, aerr := s.do(actx, s3Request{method: http.MethodDelete, key: key, query: url.Values{"uploadId": {id}}}); aerr == nil {
				resp.Body.Close()
			}
		}
	}()
	ps := s.partFor(size)
	var parts []completedPart
	var off int64
	for num := 1; num <= s3MaxParts; num++ {
		n := ps
		if size >= 0 {
			if off >= size {
				break
			}
			n = min(ps, size-off)
		}
		b, err := next(off, n)
		if err != nil {
			return err
		}
		if size < 0 && b.size == 0 && num > 1 {
			break
		}
		etag, err := s.putPart(ctx, key, id, num, b)
		if err != nil {
			return fmt.Errorf("part %d: %w", num, err)
		}
		parts = append(parts, completedPart{Number: num, ETag: etag})
		off += b.size
		if size < 0 && b.size < ps {
			break
		}
		if num == s3MaxParts && (size < 0 || off < size) {
			return fmt.Errorf("the backup is too big for %d parts", s3MaxParts)
		}
	}
	return s.complete(ctx, key, id, off, parts)
}

func (s *s3Target) startUpload(ctx context.Context, key string) (string, error) {
	h := http.Header{"Content-Type": {"application/octet-stream"}}
	resp, err := s.do(ctx, s3Request{method: http.MethodPost, key: key, query: url.Values{"uploads": {""}}, header: h})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var x struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, s3MaxErrorBody)).Decode(&x); err != nil || x.UploadID == "" {
		return "", fmt.Errorf("the store didn't start an upload (%v)", err)
	}
	return x.UploadID, nil
}

func (s *s3Target) putPart(ctx context.Context, key, id string, num int, b s3Body) (string, error) {
	q := url.Values{"partNumber": {strconv.Itoa(num)}, "uploadId": {id}}
	resp, err := s.do(ctx, s3Request{method: http.MethodPut, key: key, query: q, body: &b})
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", errors.New("the store gave no ETag for the part")
	}
	return etag, nil
}

type completedPart struct {
	Number int    `xml:"PartNumber"`
	ETag   string `xml:"ETag"`
}

func (s *s3Target) complete(ctx context.Context, key, id string, size int64, parts []completedPart) error {
	var doc bytes.Buffer
	doc.WriteString(`<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for _, p := range parts {
		doc.WriteString("<Part><PartNumber>" + strconv.Itoa(p.Number) + "</PartNumber><ETag>")
		_ = xml.EscapeText(&doc, []byte(p.ETag))
		doc.WriteString("</ETag></Part>")
	}
	doc.WriteString("</CompleteMultipartUpload>")
	b := bytesBody(doc.Bytes())
	h := http.Header{"Content-Type": {"application/xml"}}
	rq := s3Request{method: http.MethodPost, key: key, query: url.Values{"uploadId": {id}}, header: h, body: &b, conditional: true}
	for attempt := 1; ; attempt++ {
		resp, err := s.do(ctx, rq)
		if err != nil {
			return s.settle(ctx, key, size, multipartETag(parts), err)
		}
		// S3 can answer 200 and put an error in the body, after it has
		// started to reply.
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, s3MaxErrorBody))
		resp.Body.Close()
		if rerr == nil && !bytes.Contains(body, []byte("<Error>")) {
			return nil
		}
		e := parseS3Error(http.StatusInternalServerError, body)
		if rerr != nil {
			e.Message = rerr.Error()
		}
		e.bucket, e.where = s.cfg.Bucket, s.String()
		if attempt >= s3Attempts || e.Code != "" && !retryable(e) {
			return e
		}
		if err := s.sleep(ctx, backoff(attempt, e)); err != nil {
			return err
		}
	}
}

func (s *s3Target) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if s.err != nil {
		return nil, s.err
	}
	key, err := s.key(name)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(ctx, s3Request{method: http.MethodGet, key: key})
	if notFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s3Reader{ctx: ctx, s: s, key: key, rc: resp.Body, size: resp.ContentLength, etag: resp.Header.Get("ETag")}, nil
}

// s3Reader reads an object, picking up where it stopped when the
// connection breaks.
type s3Reader struct {
	ctx     context.Context
	s       *s3Target
	key     string
	rc      io.ReadCloser
	size    int64 // -1 when the store didn't say
	etag    string
	off     int64
	resumes int
}

func (r *s3Reader) Read(p []byte) (int, error) {
	for {
		n, err := r.rc.Read(p)
		r.off += int64(n)
		switch {
		case err == nil:
			return n, nil
		case r.size >= 0 && r.off >= r.size:
			return n, io.EOF // all of it came
		case err == io.EOF && r.size < 0:
			return n, io.EOF
		}
		// It broke off early.
		if r.size < 0 || r.ctx.Err() != nil || r.resumes >= s3Attempts {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return n, err
		}
		if rerr := r.resume(); rerr != nil {
			return n, fmt.Errorf("the download broke off at %d of %d bytes and couldn't resume (%v)", r.off, r.size, rerr)
		}
		if n > 0 {
			return n, nil
		}
	}
}

// resume asks for the rest of the same object (If-Match its ETag, so a
// replaced object isn't spliced in).
func (r *s3Reader) resume() error {
	r.rc.Close()
	r.resumes++
	if err := r.s.sleep(r.ctx, backoff(r.resumes, nil)); err != nil {
		return err
	}
	h := http.Header{"Range": {fmt.Sprintf("bytes=%d-", r.off)}}
	if r.etag != "" {
		h.Set("If-Match", r.etag)
	}
	resp, err := r.s.do(r.ctx, s3Request{method: http.MethodGet, key: r.key, header: h})
	if err != nil {
		r.rc = io.NopCloser(bytes.NewReader(nil))
		return err
	}
	if resp.StatusCode != http.StatusPartialContent || !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", r.off)) {
		resp.Body.Close()
		r.rc = io.NopCloser(bytes.NewReader(nil))
		return fmt.Errorf("the store sent %s instead of the rest", resp.Status)
	}
	r.rc = resp.Body
	return nil
}

func (r *s3Reader) Close() error { return r.rc.Close() }

func (s *s3Target) List(ctx context.Context) ([]Object, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []Object
	token := ""
	for page := 0; ; page++ {
		q := url.Values{"list-type": {"2"}, "prefix": {s.prefix}, "delimiter": {"/"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, s3Request{method: http.MethodGet, query: q})
		if err != nil {
			return nil, err
		}
		var x struct {
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
			Contents              []struct {
				Key          string `xml:"Key"`
				Size         int64  `xml:"Size"`
				LastModified string `xml:"LastModified"`
			} `xml:"Contents"`
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&x)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("the store's list of %s didn't read (%v)", s, err)
		}
		for _, c := range x.Contents {
			name, ok := strings.CutPrefix(c.Key, s.prefix)
			if _, _, valid := parseName(name); !ok || !valid {
				continue
			}
			mod, _ := time.Parse(time.RFC3339Nano, c.LastModified)
			out = append(out, Object{Name: name, Size: c.Size, Modified: mod})
		}
		if !x.IsTruncated {
			break
		}
		if x.NextContinuationToken == "" || x.NextContinuationToken == token || page > 100000 {
			return nil, fmt.Errorf("the store's list of %s doesn't end", s)
		}
		token = x.NextContinuationToken
	}
	sortObjects(out)
	return out, nil
}

func (s *s3Target) Delete(ctx context.Context, name string) error {
	if s.err != nil {
		return s.err
	}
	key, err := s.key(name)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, s3Request{method: http.MethodDelete, key: key})
	if notFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// check lists the folder once, so a wrong key, bucket or region shows up
// when the target is chosen rather than at 03:30.
func (s *s3Target) check(ctx context.Context) error {
	if s.err != nil {
		return s.err
	}
	q := url.Values{"list-type": {"2"}, "prefix": {s.prefix}, "max-keys": {"1"}}
	resp, err := s.do(ctx, s3Request{method: http.MethodGet, query: q})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// checkS3 tries the bucket when it is chosen (a variable for tests of the
// setup around it).
var checkS3 = func(s *s3Target) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return s.check(ctx)
}

// CheckBucket tries the bucket an "s3" setting names, with its key, so a
// mistake shows before any words are made. Other targets pass.
func CheckBucket(ctx context.Context, s config.Backup) error {
	if s.Target != TargetS3 {
		return nil
	}
	t, err := openS3(s, "")
	if err != nil {
		return err
	}
	return t.(*s3Target).check(ctx)
}

// s3ConfigOf is the S3 target settings in config, with the namespace
// folder below the owner's prefix.
func s3ConfigOf(s config.BackupS3, ns string) S3Config {
	prefix := strings.Trim(s.Prefix, "/")
	if ns != "" {
		if prefix != "" {
			prefix += "/"
		}
		prefix += ns
	}
	access, secret := s.KeyEnvs()
	return S3Config{
		Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Prefix: prefix,
		AccessKeyEnv: access, SecretKeyEnv: secret, PathStyle: s.PathStyle,
	}
}

// openS3 is the S3 target the settings name.
func openS3(s config.Backup, ns string) (Target, error) {
	if s.S3.Bucket == "" {
		return nil, errors.New("backup.s3.bucket is empty; run `mirrin backup target s3 s3://<bucket>/<folder>`")
	}
	t, err := newS3(s3ConfigOf(s.S3, ns), nil)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// whereS3 names an S3 target setting in the owner's words.
func whereS3(s config.BackupS3, ns string) string {
	c := s3ConfigOf(s, ns)
	if t, err := newS3(c, nil); err == nil {
		return strings.TrimSuffix(t.String(), "/")
	}
	return strings.TrimSuffix("s3://"+c.Bucket+"/"+c.Prefix, "/")
}

// ParseS3URL reads s3://bucket[/folder] into the bucket and folder.
func ParseS3URL(raw string) (bucket, prefix string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(raw), "s3://")
	if !ok {
		return "", "", fmt.Errorf("%q isn't an s3://bucket/folder address", raw)
	}
	bucket, prefix, _ = strings.Cut(rest, "/")
	if !reBucket.MatchString(bucket) {
		return "", "", fmt.Errorf("%q isn't a bucket name", bucket)
	}
	if _, err := cleanPrefix(prefix); err != nil {
		return "", "", err
	}
	return bucket, strings.Trim(prefix, "/"), nil
}
