package main

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup/s3fake"
	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// The mirror publishes only lists that verify, never a lower seq than the
// bucket holds (even after a restart), and re-publishes an unchanged list
// only once Refresh has passed, so its age says the mirror is alive.
func TestMirror(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	_, other, _ := ed25519.GenerateKey(crand.Reader)
	var mu sync.Mutex
	var serving string
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		io.WriteString(w, serving+"\n")
	}))
	defer src.Close()
	serve := func(seq int64, key ed25519.PrivateKey, iat time.Time) {
		tok, err := entitle.SignDenyList(entitle.DenyList{Seq: seq, Iat: iat, Handles: []entitle.Entry{{Value: "bad-one", Why: "abuse"}}}, "dl-2026a", key)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		serving = tok
		mu.Unlock()
	}
	bucket := s3fake.New(t)
	t.Setenv("MIRROR_AK", s3fake.AccessKey)
	t.Setenv("MIRROR_SK", s3fake.SecretKey)
	cfg := &Config{DenyListKeys: map[string]string{"dl-2026a": entitle.EncodeKey(pub)},
		Mirror: MirrorConfig{Source: src.URL, Endpoint: bucket.URL, Bucket: s3fake.Bucket, Region: s3fake.Region, AccessKeyEnv: "MIRROR_AK", SecretKeyEnv: "MIRROR_SK"}}
	cfg.defaults()
	now := time.Now()
	newM := func() *mirror {
		m, err := newMirror(cfg, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		m.http = bucket.Client()
		m.now = func() time.Time { return now }
		return m
	}
	stored := func() entitle.DenyList {
		b, ok := bucket.Object("denylist.paseto")
		if !ok {
			t.Fatal("nothing mirrored")
		}
		d, err := entitle.VerifyDenyList(strings.TrimSpace(string(b)), map[string]ed25519.PublicKey{"dl-2026a": pub}, now)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	m := newM()
	m.http = &http.Client{Transport: splitTransport{src: src.Client().Transport, bucket: bucket.Client().Transport, bucketHost: strings.TrimPrefix(bucket.URL, "http://")}}

	serve(3, priv, now)
	if did, err := m.step(context.Background()); err != nil || !did || stored().Seq != 3 {
		t.Fatalf("first: %v %v", did, err)
	}
	// Unchanged, within refresh: nothing sent.
	now = now.Add(time.Minute)
	serve(3, priv, now)
	if did, err := m.step(context.Background()); err != nil || did {
		t.Fatalf("unchanged: %v %v", did, err)
	}
	// Unchanged, past refresh: sent again, with the newer iat.
	now = now.Add(5 * time.Minute)
	serve(3, priv, now)
	if did, err := m.step(context.Background()); err != nil || !did || !stored().Iat.Equal(now.UTC().Truncate(time.Second)) {
		t.Fatalf("refresh: %v %v", did, err)
	}
	// A list signed by another key is refused.
	serve(9, other, now)
	if did, err := m.step(context.Background()); err == nil || did || stored().Seq != 3 {
		t.Fatalf("forged: %v %v", did, err)
	}
	// A rolled-back control plane can't lower the mirror's seq, even
	// after the mirror restarts.
	serve(5, priv, now)
	m.step(context.Background())
	serve(4, priv, now.Add(10*time.Minute))
	now = now.Add(10 * time.Minute)
	m2 := newM()
	m2.http = m.http
	if did, err := m2.step(context.Background()); err == nil || did || stored().Seq != 5 {
		t.Fatalf("rollback: %v %v seq %d", did, err, stored().Seq)
	}
}

// splitTransport sends bucket requests to the fake bucket and everything
// else to the source.
type splitTransport struct {
	src, bucket http.RoundTripper
	bucketHost  string
}

func (s splitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == s.bucketHost {
		return s.bucket.RoundTrip(r)
	}
	return s.src.RoundTrip(r)
}
