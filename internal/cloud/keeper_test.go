package cloud

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// testKid signs the tokens in this file's tests.
const testKid = "ent-keeper-test"

func testSigner() ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("keeper test " + testKid))
	return ed25519.NewKeyFromSeed(seed[:])
}

func testKeys() map[string]ed25519.PublicKey {
	return map[string]ed25519.PublicKey{testKid: testSigner().Public().(ed25519.PublicKey)}
}

// token signs an entitlement for dataDir's device key, issued at iat.
func token(t *testing.T, dataDir string, iat time.Time, gen int64) (string, entitle.Claims) {
	t.Helper()
	priv, err := createKey(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	iat = iat.UTC().Truncate(time.Second)
	c := entitle.Claims{Iss: "test", Sub: "acct_test", Aud: entitle.Audience, Iat: iat, Nbf: iat,
		Exp: iat.Add(35 * 24 * time.Hour), PaidThrough: iat.Add(30 * 24 * time.Hour), Gen: gen,
		Plan: "cloud", Feat: []string{"reach"}, Handle: "quiet-wren-07", Hosts: []string{"quiet-wren-07." + TenantZone},
		Cnf: entitle.EncodeKey(priv.Public().(ed25519.PublicKey))}
	tok, err := entitle.Sign(c, testKid, testSigner())
	if err != nil {
		t.Fatal(err)
	}
	c, _ = entitle.Inspect(tok, testKeys())
	return tok, c
}

// linkedClient is a client for api whose machine linked at iat.
func linkedClient(t *testing.T, api string, iat time.Time) *Client {
	t.Helper()
	dataDir := t.TempDir()
	c, err := New(dataDir, api, testKeys())
	if err != nil {
		t.Fatal(err)
	}
	tok, cl := token(t, dataDir, iat, 1)
	if err := c.state.saveEntitlement(c.api, tok, cl, iat, true); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUntilRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	zero := func(time.Duration) time.Duration { return 0 }
	top := func(d time.Duration) time.Duration { return d - 1 }

	c := linkedClient(t, "https://cloud.example", now)
	c.Now = func() time.Time { return now }
	if d, err := c.untilRefresh(0, zero); err != nil || d != refreshEvery {
		t.Errorf("fresh token, no jitter: %v %v, want %v", d, err, refreshEvery)
	}
	if d, _ := c.untilRefresh(0, top); d != refreshEvery+refreshWindow-1 {
		t.Errorf("fresh token, most jitter: %v", d)
	}
	// A day and a half later it is overdue: refresh at once.
	c.Now = func() time.Time { return now.Add(36 * time.Hour) }
	if d, _ := c.untilRefresh(0, top); d != 0 {
		t.Errorf("overdue: %v, want 0", d)
	}
	// A 402 counts as the control plane having answered.
	c.state.update(func(l *linkRecord) { l.CheckedAt = now.Add(30 * time.Hour) })
	if d, _ := c.untilRefresh(0, zero); d != refreshEvery-6*time.Hour {
		t.Errorf("after a 402: %v, want %v", d, refreshEvery-6*time.Hour)
	}
	// A clock set back a long way does not stall refreshing: the wait is
	// never past the end of the window.
	c.state.update(func(l *linkRecord) { l.CheckedAt = now.Add(400 * 24 * time.Hour) })
	if d, _ := c.untilRefresh(0, top); d != refreshEvery+refreshWindow {
		t.Errorf("clock set back: %v, want at most %v", d, refreshEvery+refreshWindow)
	}
	// Failures back off: under 5 min, 10 min, … up to 6 h, fully jittered,
	// and never less than a second.
	for failures, ceil := range map[int]time.Duration{1: retryMin, 2: 2 * retryMin, 7: 64 * retryMin, 8: retryMax, 60: retryMax} {
		if d, _ := c.untilRefresh(failures, top); d != ceil-1+time.Second {
			t.Errorf("%d failures: %v, want just under %v", failures, d, ceil+time.Second)
		}
		if d, _ := c.untilRefresh(failures, zero); d != time.Second {
			t.Errorf("%d failures, no jitter: %v, want 1s", failures, d)
		}
	}
	// Revoked, superseded and unlinked machines stop.
	c.state.update(func(l *linkRecord) { l.RevokedAt = now })
	if _, err := c.untilRefresh(0, zero); !errors.Is(err, ErrRevoked) {
		t.Errorf("revoked: %v", err)
	}
	c.state.update(func(l *linkRecord) { l.RevokedAt, l.Superseded = time.Time{}, &Supersede{Gen: 2, At: now} })
	var sup *SupersededError
	if _, err := c.untilRefresh(0, zero); !errors.As(err, &sup) {
		t.Errorf("superseded: %v", err)
	}
	c.state.forget()
	if _, err := c.untilRefresh(0, zero); !errors.Is(err, ErrNotLinked) {
		t.Errorf("unlinked: %v", err)
	}
}

// Keep refreshes when due, carries on after a failure, and stops for good
// once the control plane says another machine holds the handle.
func TestKeepStopsWhenSuperseded(t *testing.T) {
	var c *Client
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1:
			tok, _ := token(t, c.dataDir, time.Now(), 1)
			b, _ := json.Marshal(map[string]string{"entitlement": tok})
			w.Write(b)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"superseded","gen":2,"at":"2026-10-01T09:00:00Z"}`))
		}
	}))
	defer srv.Close()
	c = linkedClient(t, srv.URL, time.Now().Add(-48*time.Hour))
	var waits []time.Duration
	after := func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	done := make(chan struct{})
	go func() {
		c.keep(context.Background(), nil, after, func(time.Duration) time.Duration { return 0 })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Keep did not stop after 409 superseded")
	}
	if calls != 3 {
		t.Fatalf("%d refreshes, want 3", calls)
	}
	if len(waits) != 3 || waits[0] != 0 || waits[1] < refreshEvery-time.Minute || waits[2] != time.Second {
		t.Errorf("waits %v: want overdue, then a day, then a retry", waits)
	}
	if _, kind := c.state.Current(time.Now()); kind != Superseded {
		t.Errorf("state %v, want superseded", kind)
	}
}

// Keep returns at once for a machine that is not linked, and when ctx ends.
func TestKeepIdlesOnlyWhileLinked(t *testing.T) {
	c, err := New(t.TempDir(), "https://cloud.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.Keep(context.Background(), nil); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Keep ran for a machine that never linked")
	}

	c = linkedClient(t, "https://cloud.example", time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { c.Keep(ctx, nil); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Keep did not stop when its context ended")
	}
}

// A control plane that answers with a token issued long ago (a cache, or a
// clock far behind this one) does not make Keep spin: the next refresh is
// timed from when the answer arrived here, and never sooner than the floor.
func TestKeepDoesNotSpinOnAnOldToken(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var c *Client
	var old string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := json.Marshal(map[string]string{"entitlement": old})
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	defer srv.Close()
	c = linkedClient(t, srv.URL, now.Add(-48*time.Hour))
	old, _ = token(t, c.dataDir, now.Add(-30*time.Hour), 1) // 30 h behind this clock
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	after := func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		if len(waits) == 4 {
			cancel()
			return nil
		}
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	done := make(chan struct{})
	go func() {
		c.keep(ctx, nil, after, func(time.Duration) time.Duration { return 0 })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Keep did not stop")
	}
	if calls != 3 {
		t.Fatalf("%d refreshes, want 3", calls)
	}
	if waits[0] != 0 {
		t.Errorf("first wait %v, want 0 (overdue)", waits[0])
	}
	for i, d := range waits[1:] {
		if d < refreshFloor || d < refreshEvery-time.Minute {
			t.Errorf("wait %d after an answer: %v, want about %v and never under %v", i+1, d, refreshEvery, refreshFloor)
		}
	}
	if info, _, _ := c.state.Info(); time.Since(info.CheckedAt) > time.Minute {
		t.Errorf("the answer's arrival was not recorded: checked_at %v", info.CheckedAt)
	}
}

// Whatever the clocks say, the wait after an answer is at least the floor.
func TestKeepFloorsTheWaitAfterAnAnswer(t *testing.T) {
	var c *Client
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := token(t, c.dataDir, time.Now(), 1)
		b, _ := json.Marshal(map[string]string{"entitlement": tok})
		w.Write(b)
	}))
	defer srv.Close()
	c = linkedClient(t, srv.URL, time.Now().Add(-48*time.Hour))
	// This machine's clock leaps 30 hours every time it is read, so each
	// answer is overdue by the time the next wait is worked out.
	base, reads := time.Now(), 0
	c.Now = func() time.Time { reads++; return base.Add(time.Duration(reads) * 30 * time.Hour) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	after := func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		if len(waits) == 3 {
			cancel()
			return nil
		}
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	c.keep(ctx, nil, after, func(time.Duration) time.Duration { return 0 })
	if len(waits) != 3 {
		t.Fatalf("waits %v: want 3", waits)
	}
	for i, d := range waits[1:] {
		if d != refreshFloor {
			t.Errorf("wait %d after an answer: %v, want the %v floor", i+1, d, refreshFloor)
		}
	}
}
