package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup/s3fake"
	"github.com/MavrkAI/Mirrin/internal/sigv4"
)

const ns = "abcdefghijklmnopqrstuvwxyz"

func key(t *testing.T, name string) string {
	t.Helper()
	k, err := Key(ns, name)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeyIsAlwaysUnderTheNamespace(t *testing.T) {
	k := key(t, "snap-20260927T033000Z-1a2b3c4d.age")
	if k != "ns/"+ns+"/snap-20260927T033000Z-1a2b3c4d.age" {
		t.Fatalf("key %q", k)
	}
	for _, bad := range [][2]string{
		{ns, "../snap-20260927T033000Z-1a2b3c4d.age"},
		{ns, "notes.txt"},
		{ns, "snap-20260927T033000Z-1A2B3C4D.age"},
		{ns, "a/snap-20260927T033000Z-1a2b3c4d.age"},
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ", "snap-20260927T033000Z-1a2b3c4d.age"},
		{"short", "snap-20260927T033000Z-1a2b3c4d.age"},
		{ns + "/x", "snap-20260927T033000Z-1a2b3c4d.age"},
	} {
		if _, err := Key(bad[0], bad[1]); err == nil {
			t.Errorf("Key(%q, %q) was accepted", bad[0], bad[1])
		}
	}
	if !ValidName("handover-20260927T033000Z-5e6f7a8b.age") {
		t.Error("a handover marker's name was refused")
	}
}

func fakeServer(t *testing.T) (*Fake, *httptest.Server) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	f, err := NewFake(t.TempDir(), "http://"+ts.Listener.Addr().String()+"/storage")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(f.Path(), f)
	ts.Config.Handler = mux
	ts.Start()
	t.Cleanup(ts.Close)
	return f, ts
}

func do(t *testing.T, s Signed, body []byte, length int64) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(s.Method, s.URL, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.ContentLength = length
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func TestFakeRoundTrip(t *testing.T) {
	ctx := context.Background()
	f, _ := fakeServer(t)
	k := key(t, "snap-20260927T033000Z-1a2b3c4d.age")
	body := []byte("ciphertext only")
	put, err := f.Presign(ctx, OpPut, k, int64(len(body)), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if put.Method != "PUT" || put.Headers["Content-Length"] != "15" || put.Expires.IsZero() {
		t.Fatalf("presign %+v", put)
	}
	if st, b := do(t, put, body, int64(len(body))); st != 200 {
		t.Fatalf("PUT: %d %s", st, b)
	}
	if n, err := f.Head(ctx, k); err != nil || n != int64(len(body)) {
		t.Fatalf("Head = %d, %v", n, err)
	}
	get, _ := f.Presign(ctx, OpGet, k, 0, time.Minute)
	if st, b := do(t, get, nil, 0); st != 200 || !bytes.Equal(b, body) {
		t.Fatalf("GET: %d %q", st, b)
	}
	objs, err := f.List(ctx, Prefix(ns))
	if err != nil || len(objs) != 1 || objs[0].Key != k || objs[0].Size != 15 {
		t.Fatalf("List = %+v, %v", objs, err)
	}
	if !f.Contains([]byte("ciphertext")) || f.Contains([]byte("plain secret")) {
		t.Fatal("Contains is wrong")
	}
	del, _ := f.Presign(ctx, OpDelete, k, 0, time.Minute)
	if st, _ := do(t, del, nil, 0); st != http.StatusNoContent {
		t.Fatalf("DELETE: %d", st)
	}
	if _, err := f.Head(ctx, k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Head after delete: %v", err)
	}
}

// The fake holds a PUT to the size signed, which R2 cannot: a longer or
// shorter Content-Length is refused, and so is any change to the URL.
func TestFakeRefusesWhatWasNotSigned(t *testing.T) {
	ctx := context.Background()
	f, _ := fakeServer(t)
	k := key(t, "snap-20260927T033000Z-1a2b3c4d.age")
	put, _ := f.Presign(ctx, OpPut, k, 10, time.Minute)
	if st, _ := do(t, put, []byte("0123456789ab"), 12); st != http.StatusBadRequest {
		t.Errorf("a longer Content-Length: %d", st)
	}
	if st, _ := do(t, put, []byte("01234"), 5); st != http.StatusBadRequest {
		t.Errorf("a shorter Content-Length: %d", st)
	}
	if _, err := f.Head(ctx, k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused PUT left an object: %v", err)
	}
	// Another key, another size, another method: all refused.
	u, _ := url.Parse(put.URL)
	other := *u
	other.Path = strings.Replace(other.Path, "1a2b3c4d", "00000000", 1)
	if st, _ := do(t, Signed{URL: other.String(), Method: "PUT"}, []byte("0123456789"), 10); st != http.StatusForbidden {
		t.Errorf("another key: %d", st)
	}
	q := u.Query()
	q.Set("size", "11")
	bigger := *u
	bigger.RawQuery = q.Encode()
	if st, _ := do(t, Signed{URL: bigger.String(), Method: "PUT"}, []byte("0123456789a"), 11); st != http.StatusForbidden {
		t.Errorf("another size: %d", st)
	}
	if st, _ := do(t, Signed{URL: put.URL, Method: "DELETE"}, nil, 0); st != http.StatusForbidden {
		t.Errorf("another method: %d", st)
	}
	// An expired URL.
	f.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if st, _ := do(t, put, []byte("0123456789"), 10); st != http.StatusForbidden {
		t.Errorf("an expired URL: %d", st)
	}
}

func TestR2(t *testing.T) {
	ctx := context.Background()
	srv := s3fake.New(t)
	srv.MaxKeys = 1 // lists come in pages
	r := &R2{Endpoint: srv.URL, Bucket: s3fake.Bucket, Region: s3fake.Region,
		Creds: sigv4.Creds{AccessKeyID: s3fake.AccessKey, SecretAccessKey: s3fake.SecretKey}, HTTP: srv.Client()}
	names := []string{"snap-20260927T033000Z-1a2b3c4d.age", "snap-20260928T033000Z-1a2b3c4e.age"}
	for i, n := range names {
		// Stored as the daemon's PUT would store it; s3fake checks
		// header signatures, so it is signed that way here.
		body := strings.Repeat("x", 10+i)
		req, _ := http.NewRequest("PUT", srv.URL+"/"+s3fake.Bucket+"/"+key(t, n), strings.NewReader(body))
		if err := sigv4.Sign(req, r.Creds, s3fake.Region, "s3", time.Now(), sigv4.PayloadHash([]byte(body))); err != nil {
			t.Fatal(err)
		}
		res, err := srv.Client().Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("seed PUT: %v %v", res, err)
		}
		res.Body.Close()
	}
	if n, err := r.Head(ctx, key(t, names[1])); err != nil || n != 11 {
		t.Fatalf("Head = %d, %v", n, err)
	}
	if _, err := r.Head(ctx, key(t, "snap-20260929T033000Z-1a2b3c4f.age")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Head of nothing: %v", err)
	}
	objs, err := r.List(ctx, Prefix(ns))
	if err != nil || len(objs) != 2 || objs[0].Key != key(t, names[0]) || objs[1].Size != 11 {
		t.Fatalf("List = %+v, %v", objs, err)
	}
	if err := r.DeletePrefix(ctx, Prefix(ns)); err != nil {
		t.Fatal(err)
	}
	if len(srv.Keys()) != 0 {
		t.Fatalf("left %v", srv.Keys())
	}
	if err := r.DeletePrefix(ctx, ""); err == nil {
		t.Fatal("DeletePrefix of the whole bucket was allowed")
	}

	// Presigned URLs: path-style, for the key, SigV4 in the query, and
	// exactly what internal/sigv4 makes for that request and time. A PUT's
	// signs its Content-Length, so R2 takes no other size.
	now := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	s, err := r.Presign(ctx, OpPut, key(t, names[0]), 1234, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := sigv4.PresignSized("PUT", srv.URL+"/"+s3fake.Bucket+"/"+key(t, names[0]), r.Creds, s3fake.Region, "s3", 15*time.Minute, now, 1234)
	if s.URL != want || s.Method != "PUT" || s.Headers["Content-Length"] != "1234" || !s.Expires.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("presign %+v, want %s", s, want)
	}
	if !strings.Contains(s.URL, "X-Amz-Expires=900") || !strings.Contains(s.URL, "/ns/"+ns+"/") || !strings.Contains(s.URL, "X-Amz-SignedHeaders=content-length%3Bhost") {
		t.Fatalf("presign URL %s", s.URL)
	}
	g, _ := r.Presign(ctx, OpGet, key(t, names[0]), 0, 15*time.Minute)
	wantGet, _ := sigv4.Presign("GET", srv.URL+"/"+s3fake.Bucket+"/"+key(t, names[0]), r.Creds, s3fake.Region, "s3", 15*time.Minute, now)
	if g.URL != wantGet {
		t.Fatalf("presigned get %s, want %s", g.URL, wantGet)
	}
	if _, err := r.Presign(ctx, "copy", key(t, names[0]), 0, time.Minute); err == nil {
		t.Fatal("an unknown op was presigned")
	}
	if _, err := r.Presign(ctx, OpGet, "other/key", 0, time.Minute); err == nil {
		t.Fatal("a key outside ns/ was presigned")
	}
}
