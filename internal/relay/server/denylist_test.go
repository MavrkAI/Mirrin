package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// A poll completion covers verification and the cache rename, including an
// ignored stale list. No sockets or wall-clock polling are needed here.
func TestDenyPollCompletion(t *testing.T) {
	is := newIssuer(t)
	cfg := DefaultConfig()
	cfg.ID = "r1"
	cfg.ControlHostname = controlName
	cfg.StateDir = t.TempDir()
	cfg.Zones = []string{"mirrin.test"}
	cfg.IssuerKeys = is.keys()
	cfg.DenylistURL = "https://deny.test/list"
	tok := is.denyList(t, 5, []string{"ember"})
	opts := Options{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tok))}, nil
	})}}
	polls := newDenyPolls(&opts)
	s, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.wg.Add(1)
	go s.pollDenyList()
	waitSignal(t, 5*time.Second, "initial refresh", polls.done)
	check := func(seq int64, denied bool) {
		t.Helper()
		if got := s.m.denySeq.Load(); got != seq {
			t.Fatalf("seq %d, want %d", got, seq)
		}
		if _, got := s.deny.denies("ember", nil); got != denied {
			t.Fatalf("denied = %v, want %v", got, denied)
		}
		b, err := os.ReadFile(filepath.Join(cfg.StateDir, denyCacheFile))
		if err != nil {
			t.Fatal(err)
		}
		l, err := entitle.VerifyDenyList(strings.TrimSpace(string(b)), s.pol.dlKeys, time.Now())
		if err != nil || l.Seq != seq {
			t.Fatalf("cached seq %d, want %d: %v", l.Seq, seq, err)
		}
	}
	check(5, true)
	for _, seq := range []int64{4, 5, 6} {
		tok = is.denyList(t, seq, nil)
		polls.next(t)
		check(max(5, seq), seq < 6)
	}
}
