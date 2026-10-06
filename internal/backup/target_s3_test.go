package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup/s3fake"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// countingTransport counts the requests that go out, and can drop the
// answer to some after the store has acted on them.
type countingTransport struct {
	next http.RoundTripper
	n    atomic.Int64
	// lose, when set, drops the response to a request it returns true
	// for, as a connection that breaks after the request was sent would.
	lose func(r *http.Request) bool
	mu   sync.Mutex
	seen []string // method host path
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	c.mu.Lock()
	c.seen = append(c.seen, r.Method+" "+r.URL.Host+" "+r.URL.EscapedPath())
	c.mu.Unlock()
	resp, err := c.next.RoundTrip(r)
	if err == nil && c.lose != nil && c.lose(r) {
		resp.Body.Close()
		return nil, errors.New("connection reset by peer")
	}
	return resp, err
}

// s3For is an S3 target on srv with the fake's keys, parts above 6 MiB and
// no waiting between tries.
func s3For(t *testing.T, srv *s3fake.Server, c S3Config) (*s3Target, *countingTransport) {
	t.Helper()
	ct := &countingTransport{next: srv.Client().Transport}
	if c.Endpoint == "" {
		c.Endpoint = srv.URL
		c.PathStyle = true
	}
	if c.Bucket == "" {
		c.Bucket = s3fake.Bucket
	}
	tg, err := newS3(c, &http.Client{Transport: ct})
	if err != nil {
		t.Fatal(err)
	}
	tg.secret = func(name string) string {
		return map[string]string{DefaultS3AccessKeyEnv: s3fake.AccessKey, DefaultS3SecretKeyEnv: s3fake.SecretKey}[name]
	}
	tg.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	tg.threshold, tg.partSize = 6<<20, s3fake.MinPartSize
	return tg, ct
}

const s3Name = "snap-20260927T033000Z-1a2b3c4d.age"

func putBytes(t *testing.T, tg Target, name string, b []byte) error {
	t.Helper()
	return tg.Put(context.Background(), name, bytes.NewReader(b), int64(len(b)))
}

func getBytes(t *testing.T, tg Target, name string) []byte {
	t.Helper()
	rc, err := tg.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestS3Addressing(t *testing.T) {
	srv := s3fake.New(t)
	virt, vt := s3For(t, srv, S3Config{Endpoint: "http://s3.fake.test:" + srv.Port(), Prefix: "p"})
	if err := putBytes(t, virt, s3Name, []byte("v")); err != nil {
		t.Fatal(err)
	}
	for _, s := range vt.seen {
		if !strings.Contains(s, " "+s3fake.Bucket+".s3.fake.test:") {
			t.Errorf("virtual-host request %q doesn't name the bucket in the host", s)
		}
	}
	path, pt := s3For(t, srv, S3Config{Endpoint: srv.URL + "/", Prefix: "q", PathStyle: true})
	if err := putBytes(t, path, s3Name, []byte("p")); err != nil {
		t.Fatal(err)
	}
	for _, s := range pt.seen {
		if !strings.Contains(s, " /"+s3fake.Bucket+"/q/") {
			t.Errorf("path-style request %q doesn't put the bucket in the path", s)
		}
	}
	if got := srv.Keys(); len(got) != 2 || got[0] != "p/"+s3Name || got[1] != "q/"+s3Name {
		t.Fatalf("keys = %v", got)
	}
	// A bucket that can't be a hostname, or an IP address, goes in the path.
	for _, c := range []S3Config{
		{Endpoint: "https://s3.example.com", Bucket: "my.bucket"},
		{Endpoint: "http://10.0.0.2:9000", Bucket: "plain"},
		{Endpoint: "http://localhost:9000", Bucket: "plain"},
		{Endpoint: "https://s3.example.com", Bucket: "Upper"},
	} {
		tg, err := newS3(c, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(tg.basePath, "/"+c.Bucket) {
			t.Errorf("%+v: bucket not in the path (%q %q)", c, tg.base.Host, tg.basePath)
		}
	}
}

func TestS3ConfigChecks(t *testing.T) {
	for _, c := range []S3Config{
		{Bucket: ""},
		{Bucket: "ab"},
		{Bucket: "a..b"},
		{Bucket: "ok-bucket", Prefix: "../up"},
		{Bucket: "ok-bucket", Prefix: "a b"},
		{Bucket: "ok-bucket", Endpoint: "ftp://x"},
		{Bucket: "ok-bucket", Endpoint: "https://user:pw@host"},
		{Bucket: "ok-bucket", Region: "US EAST"},
		{Bucket: "ok-bucket", AccessKeyEnv: "not a name"},
	} {
		tg := S3(c, nil)
		if err := tg.Put(context.Background(), s3Name, strings.NewReader("x"), 1); err == nil {
			t.Errorf("%+v: accepted", c)
		}
		if tg.String() == "" {
			t.Errorf("%+v: no String", c)
		}
	}
}

func TestS3Region(t *testing.T) {
	for host, want := range map[string]string{
		"abc123.r2.cloudflarestorage.com":      "auto",
		"abc123.eu.r2.cloudflarestorage.com":   "auto",
		"s3.us-west-004.backblazeb2.com":       "us-west-004",
		"s3.eu-central-1.wasabisys.com":        "eu-central-1",
		"s3.wasabisys.com":                     "us-east-1",
		"s3.amazonaws.com":                     "us-east-1",
		"s3.ap-southeast-2.amazonaws.com":      "ap-southeast-2",
		"s3-eu-west-1.amazonaws.com":           "eu-west-1",
		"s3.dualstack.us-west-2.amazonaws.com": "us-west-2",
		"bucket.s3.eu-north-1.amazonaws.com":   "eu-north-1",
		"minio.home.lan":                       "us-east-1",
		"127.0.0.1":                            "us-east-1",
		"s3-external-1.amazonaws.com":          "us-east-1",
	} {
		if got := S3Region(host); got != want {
			t.Errorf("S3Region(%q) = %q, want %q", host, got, want)
		}
	}
	u, err := S3Endpoint("", "eu-west-2")
	if err != nil || u.String() != "https://s3.eu-west-2.amazonaws.com" {
		t.Fatalf("default endpoint = %v, %v", u, err)
	}
	u, err = S3Endpoint("minio.lan:9000", "")
	if err != nil || u.String() != "https://minio.lan:9000" {
		t.Fatalf("bare endpoint = %v, %v", u, err)
	}
	if u, _ = S3Endpoint("https://S3.Example.com:443/", ""); u.Host != "s3.example.com" {
		t.Fatalf("default port kept: %v", u)
	}
}

func TestParseS3URL(t *testing.T) {
	b, p, err := ParseS3URL("s3://my-bucket/mirrin/home/")
	if err != nil || b != "my-bucket" || p != "mirrin/home" {
		t.Fatalf("= %q %q %v", b, p, err)
	}
	if b, p, err = ParseS3URL("s3://my-bucket"); err != nil || b != "my-bucket" || p != "" {
		t.Fatalf("= %q %q %v", b, p, err)
	}
	for _, bad := range []string{"my-bucket", "s3://", "s3://x/../y", "https://b/p"} {
		if _, _, err := ParseS3URL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestS3RetriesThrottling(t *testing.T) {
	srv := s3fake.New(t)
	var fails atomic.Int64
	srv.Fault = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && fails.Add(1) <= 2 {
			w.Header().Set("Retry-After", "1")
			s3fake.WriteError(w, http.StatusServiceUnavailable, "SlowDown", "Please reduce your request rate.")
			return true
		}
		return false
	}
	tg, _ := s3For(t, srv, S3Config{})
	if err := putBytes(t, tg, s3Name, []byte("after a pause")); err != nil {
		t.Fatal(err)
	}
	if got := getBytes(t, tg, s3Name); string(got) != "after a pause" {
		t.Fatalf("got %q", got)
	}
	if fails.Load() != 3 || srv.Count("PUT") != 1 {
		t.Fatalf("%d PUTs, %d stored; want 3, 1", fails.Load(), srv.Count("PUT"))
	}
}

func TestS3GivesUpAndLeavesNoParts(t *testing.T) {
	srv := s3fake.New(t)
	srv.Fault = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("partNumber") == "2" {
			s3fake.WriteError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.")
			return true
		}
		return false
	}
	tg, _ := s3For(t, srv, S3Config{})
	err := tg.Put(context.Background(), s3Name, bytes.NewReader(make([]byte, 7<<20)), 7<<20)
	if err == nil || !strings.Contains(err.Error(), "5 tries") || !strings.Contains(err.Error(), "part 2") {
		t.Fatalf("err = %v", err)
	}
	if srv.PendingUploads() != 0 || srv.Count("ABORT") != 1 {
		t.Fatalf("%d uploads left open, %d aborts", srv.PendingUploads(), srv.Count("ABORT"))
	}
	if len(srv.Keys()) != 0 {
		t.Fatalf("stored %v", srv.Keys())
	}
}

// A PUT whose answer is lost is tried again; the store refuses it because
// the object is there now, and the target sees that it is its own.
func TestS3LostAnswer(t *testing.T) {
	srv := s3fake.New(t)
	tg, ct := s3For(t, srv, S3Config{})
	var lost atomic.Bool
	ct.lose = func(r *http.Request) bool { return r.Method == http.MethodPut && lost.CompareAndSwap(false, true) }
	if err := putBytes(t, tg, s3Name, []byte("stored once")); err != nil {
		t.Fatal(err)
	}
	if srv.Count("PUT") != 2 {
		t.Fatalf("%d PUTs, want 2", srv.Count("PUT"))
	}
	// A different object of the same name is still refused.
	if err := putBytes(t, tg, s3Name, []byte("other")); err == nil {
		t.Fatal("replaced an object")
	}
}

// The answer to CompleteMultipartUpload is lost after the store finished
// the upload: the retry is told NoSuchUpload, and the target sees the
// object is its own (size and multipart ETag) instead of calling the
// backup failed.
func TestS3LostCompleteAnswer(t *testing.T) {
	srv := s3fake.New(t)
	tg, ct := s3For(t, srv, S3Config{})
	var lost atomic.Bool
	ct.lose = func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.URL.Query().Has("uploadId") && lost.CompareAndSwap(false, true)
	}
	body := bytes.Repeat([]byte("part of a big backup "), (11<<20)/21)
	if err := putBytes(t, tg, s3Name, body); err != nil {
		t.Fatal(err)
	}
	if !lost.Load() {
		t.Fatal("no Complete answer was dropped")
	}
	got, ok := srv.Object(tg.prefix + s3Name)
	if !ok || !bytes.Equal(got, body) {
		t.Fatalf("stored %d bytes (%v), want %d", len(got), ok, len(body))
	}
	if srv.PendingUploads() != 0 {
		t.Fatalf("%d uploads left open", srv.PendingUploads())
	}
}

// A refused Complete isn't taken as success when what is under the name
// isn't what was uploaded.
func TestS3CompleteRefusedForAnotherObject(t *testing.T) {
	srv := s3fake.New(t)
	tg, ct := s3For(t, srv, S3Config{})
	size := int64(11 << 20)
	var lost atomic.Bool
	ct.lose = func(r *http.Request) bool {
		if r.Method != http.MethodPost || !r.URL.Query().Has("uploadId") || !lost.CompareAndSwap(false, true) {
			return false
		}
		// The upload went through, but before the retry someone else's
		// object of the same size takes the name.
		if err := tg.Delete(context.Background(), s3Name); err != nil {
			t.Error(err)
		}
		if err := putBytes(t, tg, s3Name, bytes.Repeat([]byte("x"), int(size))); err != nil {
			t.Error(err)
		}
		return true
	}
	err := tg.Put(context.Background(), s3Name, bytes.NewReader(bytes.Repeat([]byte("y"), int(size))), size)
	if err == nil || !strings.Contains(err.Error(), "NoSuchUpload") {
		t.Fatalf("err = %v", err)
	}
}

func TestSameETag(t *testing.T) {
	h := http.Header{"Etag": {`"abc"`}}
	if !sameETag(h, `"ABC"`) || !sameETag(h, "") || sameETag(h, `"def"`) {
		t.Fatal("plain ETags")
	}
	h.Set("X-Amz-Server-Side-Encryption", "aws:kms")
	if !sameETag(h, `"def"`) {
		t.Fatal("a KMS-encrypted object's ETag isn't its MD5")
	}
	if multipartETag([]completedPart{{1, `"not-md5"`}}) != "" {
		t.Fatal("made a multipart ETag from a part ETag that isn't an MD5")
	}
}

// A download whose store goes quiet mid-body is dropped after the idle
// time and resumed, rather than hanging the restore.
func TestS3StalledDownloadResumes(t *testing.T) {
	srv := s3fake.New(t)
	tg, _ := s3For(t, srv, S3Config{})
	tg.idle = 200 * time.Millisecond
	body := bytes.Repeat([]byte("0123456789"), 50_000)
	if err := putBytes(t, tg, s3Name, body); err != nil {
		t.Fatal(err)
	}
	srv.StallGets, srv.CutAt = 1, 100_000
	start := time.Now()
	if got := getBytes(t, tg, s3Name); !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
	if srv.Count("GET") != 2 || time.Since(start) > 30*time.Second {
		t.Fatalf("%d GETs in %v", srv.Count("GET"), time.Since(start))
	}
}

// An upload whose store stops answering is dropped and tried again.
func TestS3StalledUploadRetries(t *testing.T) {
	srv := s3fake.New(t)
	var stalled atomic.Bool
	srv.Fault = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && stalled.CompareAndSwap(false, true) {
			// Read it all (so the server notices the client leave), then
			// say nothing.
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(20 * time.Second):
			}
			return true
		}
		return false
	}
	tg, _ := s3For(t, srv, S3Config{})
	tg.idle = 200 * time.Millisecond
	if err := putBytes(t, tg, s3Name, []byte("went up the second time")); err != nil {
		t.Fatal(err)
	}
	if got := getBytes(t, tg, s3Name); string(got) != "went up the second time" {
		t.Fatalf("got %q", got)
	}
}

func TestS3PlainHTTPOnlyNearby(t *testing.T) {
	for _, ep := range []string{"http://127.0.0.1:9000", "http://192.168.1.5:9000", "http://minio:9000", "http://nas.local:9000", "http://s3.fake.test:1", "http://[::1]:9000"} {
		if _, err := S3Endpoint(ep, ""); err != nil {
			t.Errorf("%s: %v", ep, err)
		}
	}
	for _, ep := range []string{"http://s3.amazonaws.com", "http://8.8.8.8:9000", "http://minio.example.com"} {
		if _, err := S3Endpoint(ep, ""); err == nil || !strings.Contains(err.Error(), "https://") {
			t.Errorf("%s: err = %v", ep, err)
		}
	}
}

func TestS3ClockSkew(t *testing.T) {
	srv := s3fake.New(t)
	tg, _ := s3For(t, srv, S3Config{})
	tg.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	if err := putBytes(t, tg, s3Name, []byte("late clock")); err != nil {
		t.Fatal(err)
	}
	if d := time.Duration(tg.skew.Load()); d < 110*time.Minute || d > 130*time.Minute {
		t.Fatalf("skew = %v", d)
	}
}

func TestS3ResumesBrokenDownload(t *testing.T) {
	srv := s3fake.New(t)
	tg, _ := s3For(t, srv, S3Config{})
	body := bytes.Repeat([]byte("0123456789"), 50_000)
	if err := putBytes(t, tg, s3Name, body); err != nil {
		t.Fatal(err)
	}
	srv.CutGets, srv.CutAt = 1, 123_456
	if got := getBytes(t, tg, s3Name); !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
	if srv.Count("GET") != 2 {
		t.Fatalf("%d GETs, want 2", srv.Count("GET"))
	}
}

// A download that breaks off after the object was replaced doesn't splice
// two objects together.
func TestS3ResumeRefusesAChangedObject(t *testing.T) {
	srv := s3fake.New(t)
	tg, _ := s3For(t, srv, S3Config{})
	body := bytes.Repeat([]byte("a"), 300_000)
	if err := putBytes(t, tg, s3Name, body); err != nil {
		t.Fatal(err)
	}
	srv.CutGets, srv.CutAt = 1, 1000
	rc, err := tg.Get(context.Background(), s3Name)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if err := tg.Delete(context.Background(), s3Name); err != nil {
		t.Fatal(err)
	}
	if err := putBytes(t, tg, s3Name, bytes.Repeat([]byte("b"), 300_000)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err == nil || bytes.Contains(got, []byte("b")) {
		t.Fatalf("read %d bytes, err %v", len(got), err)
	}
}

func TestS3CompleteErrorInOK(t *testing.T) {
	srv := s3fake.New(t)
	var once atomic.Bool
	srv.Fault = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && r.URL.Query().Has("uploadId") && once.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, s3fake.ErrorBody("InternalError", "We encountered an internal error."))
			return true
		}
		return false
	}
	tg, _ := s3For(t, srv, S3Config{})
	body := bytes.Repeat([]byte{7}, 11<<20)
	if err := putBytes(t, tg, s3Name, body); err != nil {
		t.Fatal(err)
	}
	if got := getBytes(t, tg, s3Name); !bytes.Equal(got, body) {
		t.Fatal("differs")
	}
	if !once.Load() || srv.Count("COMPLETE") != 1 {
		t.Fatalf("fault used %v, %d completes", once.Load(), srv.Count("COMPLETE"))
	}
}

func TestS3StoreWithoutConditionalWrites(t *testing.T) {
	srv := s3fake.New(t)
	srv.NoConditional = true
	tg, _ := s3For(t, srv, S3Config{})
	if err := putBytes(t, tg, s3Name, []byte("small")); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte{1}, 11<<20)
	if err := putBytes(t, tg, "snap-20260928T033000Z-1a2b3c4d.age", big); err != nil {
		t.Fatal(err)
	}
	// Still never replaces: it looks first.
	if err := putBytes(t, tg, s3Name, []byte("again")); err == nil {
		t.Fatal("replaced an object")
	}
}

func TestS3UnknownSize(t *testing.T) {
	srv := s3fake.New(t)
	tg, _ := s3For(t, srv, S3Config{})
	small := []byte("no size given")
	if err := tg.Put(context.Background(), s3Name, bytes.NewBufferString(string(small)), -1); err != nil {
		t.Fatal(err)
	}
	if got := getBytes(t, tg, s3Name); !bytes.Equal(got, small) {
		t.Fatalf("got %q", got)
	}
	big := bytes.Repeat([]byte("xyz"), 4<<20) // 12 MiB: three parts
	name := "snap-20260928T033000Z-1a2b3c4d.age"
	if err := tg.Put(context.Background(), name, bytes.NewBuffer(big), -1); err != nil {
		t.Fatal(err)
	}
	if got := getBytes(t, tg, name); !bytes.Equal(got, big) {
		t.Fatal("differs")
	}
	if srv.Count("PART") != 3 {
		t.Fatalf("%d parts", srv.Count("PART"))
	}
}

func TestS3ClearErrors(t *testing.T) {
	srv := s3fake.New(t)
	for _, tc := range []struct {
		name  string
		c     S3Config
		keys  map[string]string
		wants []string
	}{
		{"no key", S3Config{}, map[string]string{}, []string{DefaultS3AccessKeyEnv, DefaultS3SecretKeyEnv, "isn't set"}},
		{"no secret", S3Config{}, map[string]string{DefaultS3AccessKeyEnv: s3fake.AccessKey}, []string{DefaultS3SecretKeyEnv, "isn't set"}},
		{"unknown key", S3Config{}, map[string]string{DefaultS3AccessKeyEnv: "AKIDWRONG", DefaultS3SecretKeyEnv: s3fake.SecretKey}, []string{"doesn't know", DefaultS3AccessKeyEnv}},
		{"wrong secret", S3Config{AccessKeyEnv: "MY_ID", SecretKeyEnv: "MY_SECRET"}, map[string]string{"MY_ID": s3fake.AccessKey, "MY_SECRET": "wrong"}, []string{"MY_SECRET", "doesn't match"}},
		{"wrong bucket", S3Config{Bucket: "nope-bucket"}, nil, []string{"nope-bucket", "isn't there"}},
		{"wrong region", S3Config{Region: "eu-west-1"}, nil, []string{"region us-east-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tg, ct := s3For(t, srv, tc.c)
			if tc.keys != nil {
				tg.secret = func(n string) string { return tc.keys[n] }
			}
			err := tg.check(context.Background())
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tc.wants {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%q doesn't say %q", err, w)
				}
			}
			if strings.Contains(err.Error(), s3fake.SecretKey) || strings.Contains(err.Error(), "<") {
				t.Errorf("%q shows a secret or raw XML", err)
			}
			if n := ct.n.Load(); n > 1 {
				t.Errorf("%d requests for an error trying again can't fix", n)
			}
		})
	}
}

// The keys come from the variables the config names, through the
// environment or secrets.env, and never reach the store: the store holds
// only ciphertext.
func TestS3BackupHoldsNoKeys(t *testing.T) {
	srv := s3fake.New(t)
	tw := newTwin(t)
	t.Setenv("MIRRIN_HOME", tw.home)
	t.Setenv("MY_S3_ID", s3fake.AccessKey)
	// The secret is only in secrets.env, as `mirrin backup target s3` saves it.
	if err := os.WriteFile(filepath.Join(tw.home, "secrets.env"),
		[]byte("ANTHROPIC_API_KEY="+plantedAPIKey+"\nMY_S3_SECRET=\""+s3fake.SecretKey+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if config.Secret("MY_S3_SECRET") != s3fake.SecretKey {
		t.Skip("config.Home doesn't follow MIRRIN_HOME here")
	}
	p := fixedPhrase(t)
	s := config.Backup{
		Recipient: p.Recipient(), RecoveryPub: p.RecoveryPub(), Target: TargetS3,
		S3: config.BackupS3{Endpoint: srv.URL, Bucket: s3fake.Bucket, Prefix: "home", PathStyle: true, AccessKeyEnv: "MY_S3_ID", SecretKeyEnv: "MY_S3_SECRET"},
	}
	if err := SaveSettings(filepath.Join(tw.home, "config.yaml"), s); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(tw.home, "config.yaml"))
	if bytes.Contains(raw, []byte(s3fake.SecretKey)) || bytes.Contains(raw, []byte(s3fake.AccessKey)) || !bytes.Contains(raw, []byte("MY_S3_SECRET")) {
		t.Fatalf("config.yaml holds a key, or not its variable's name:\n%s", raw)
	}
	tg, err := OpenTarget(s, p.Namespace())
	if err != nil {
		t.Fatal(err)
	}
	e := engineFor(t, tw, p, "")
	e.Settings, e.Target = s, tg
	_, name := runAt(t, e, time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC))
	if got := srv.Keys(); len(got) != 1 || got[0] != "home/"+p.Namespace()+"/"+name {
		t.Fatalf("keys = %v", got)
	}
	for _, secret := range []string{s3fake.SecretKey, s3fake.AccessKey, plantedAPIKey, "MY_S3_SECRET", "flat whites"} {
		if srv.Contains([]byte(secret)) {
			t.Errorf("the store holds %q in plaintext", secret)
		}
	}
	// And the words open what is there.
	b, _ := srv.Object("home/" + p.Namespace() + "/" + name)
	_, files := entries(t, bytes.NewReader(b), p.Identities()...)
	if manifestOf(t, files).Twin != "Jeeves" {
		t.Fatal("the snapshot doesn't open")
	}
}

func TestSecretEnvsNameTheBucketKeys(t *testing.T) {
	c := &config.Config{Backup: config.Backup{Target: TargetS3}}
	envs := strings.Join(c.SecretEnvs(), " ")
	if !strings.Contains(envs, DefaultS3AccessKeyEnv) || !strings.Contains(envs, DefaultS3SecretKeyEnv) {
		t.Fatalf("SecretEnvs = %s", envs)
	}
	c.Backup.S3.SecretKeyEnv = "R2_SECRET"
	if !strings.Contains(strings.Join(c.SecretEnvs(), " "), "R2_SECRET") {
		t.Fatal("a named variable is missing")
	}
}

func TestWhereS3(t *testing.T) {
	s := config.Backup{Target: TargetS3, S3: config.BackupS3{Endpoint: "https://acct.r2.cloudflarestorage.com", Bucket: "twin", Prefix: "home/"}}
	if got := Where(s); got != "s3://twin/home on acct.r2.cloudflarestorage.com" {
		t.Errorf("Where = %q", got)
	}
	if got := WhereNS(s, "abcd"); got != "s3://twin/home/abcd on acct.r2.cloudflarestorage.com" {
		t.Errorf("WhereNS = %q", got)
	}
	aws := config.Backup{Target: TargetS3, S3: config.BackupS3{Region: "eu-west-1", Bucket: "twin"}}
	if got := Where(aws); got != "s3://twin" {
		t.Errorf("Where = %q", got)
	}
}
