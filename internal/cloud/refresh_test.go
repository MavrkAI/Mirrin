package cloud

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scripted is a control plane that gives whatever answer the test sets.
type scripted struct {
	*httptest.Server
	status int
	ctype  string
	body   string
}

func newScripted(t *testing.T) *scripted {
	s := &scripted{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.ctype != "" {
			w.Header().Set("Content-Type", s.ctype)
		}
		w.WriteHeader(s.status)
		w.Write([]byte(s.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *scripted) answer(status int, ctype, body string) {
	s.status, s.ctype, s.body = status, ctype, body
}

// Answers that say nothing about this machine change nothing: a proxy's
// HTML 403, a 403 or 402 without its code, a 409 superseded naming a
// generation this machine already holds. They are retried later, and the
// entitlement stays in use.
func TestRefreshAnswersThatSayNothingAboutThisMachine(t *testing.T) {
	const htmlType = "text/html; charset=utf-8"
	for name, a := range map[string]struct {
		status      int
		ctype, body string
	}{
		"html 403":                {403, htmlType, "<html><body><h1>403 Forbidden</h1>cloudfront</body></html>"},
		"403 with no body":        {403, "", ""},
		"403 with another code":   {403, "application/json", `{"error":"forbidden","message":"No."}`},
		"html 402":                {402, htmlType, "<html>Payment Required</html>"},
		"superseded at own gen":   {409, "application/json", `{"error":"superseded","gen":1,"at":"2026-10-01T09:00:00Z"}`},
		"superseded below":        {409, "application/json", `{"error":"superseded","gen":0,"at":"2026-10-01T09:00:00Z"}`},
		"409 without a code":      {409, "application/json", `{"gen":5}`},
		"superseded, not json":    {409, htmlType, "superseded"},
		"server error":            {500, htmlType, "<html>oops</html>"},
		"superseded, bad gen":     {409, "application/json", `{"error":"superseded","gen":"5"}`},
		"superseded, no gen":      {409, "application/json", `{"error":"superseded"}`},
		"lapsed, but it is a 403": {403, "application/json", `{"error":"lapsed"}`},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newScripted(t)
			c := linkedClient(t, srv.URL, time.Now().Add(-48*time.Hour))
			before, _, _ := c.state.Info()
			srv.answer(a.status, a.ctype, a.body)
			_, err := c.Refresh(context.Background())
			var sup *SupersededError
			if err == nil || errors.Is(err, ErrRevoked) || errors.Is(err, ErrLapsed) || errors.As(err, &sup) {
				t.Fatalf("refresh: %v, want a plain error", err)
			}
			after, _, _ := c.state.Info()
			if after != before {
				t.Errorf("link state changed: %+v, was %+v", after, before)
			}
			if _, kind := c.state.Current(time.Now()); kind != Active {
				t.Errorf("state %v, want active", kind)
			}
			if _, err := c.untilRefresh(1, func(d time.Duration) time.Duration { return d }); err != nil {
				t.Errorf("Keep would stop: %v", err)
			}
		})
	}
}

// The answers that do speak about this machine are recorded.
func TestRefreshAnswersThatEndTheLink(t *testing.T) {
	for name, a := range map[string]struct {
		body string
		want StateKind
	}{
		"revoked": {`{"error":"revoked","message":"This machine's link was revoked."}`, Expired},
		"deleted": {`{"error":"deleted","message":"This account was deleted."}`, Expired},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newScripted(t)
			c := linkedClient(t, srv.URL, time.Now().Add(-48*time.Hour))
			srv.answer(403, "application/json", a.body)
			if _, err := c.Refresh(context.Background()); !errors.Is(err, ErrRevoked) {
				t.Fatalf("refresh: %v, want ErrRevoked", err)
			}
			if _, kind := c.state.Current(time.Now()); kind != a.want {
				t.Errorf("state %v, want %v", kind, a.want)
			}
		})
	}
	srv := newScripted(t)
	c := linkedClient(t, srv.URL, time.Now().Add(-48*time.Hour))
	srv.answer(409, "application/json", `{"error":"superseded","gen":2,"at":"2026-10-01T09:00:00Z"}`)
	var sup *SupersededError
	if _, err := c.Refresh(context.Background()); !errors.As(err, &sup) || sup.Gen != 2 {
		t.Fatalf("refresh: %v, want superseded at generation 2", err)
	}
	if _, kind := c.state.Current(time.Now()); kind != Superseded {
		t.Errorf("state %v, want superseded", kind)
	}
}

// A refresh never moves this machine back: not to a token issued before
// the one it holds, and not to a lower generation.
func TestRefreshRefusesAnOlderToken(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for name, tc := range map[string]struct {
		holdGen  int64
		offerIat time.Time
		offerGen int64
	}{
		"issued before the one held": {1, now.Add(-2 * time.Hour), 1},
		"a lower generation":         {3, now, 2},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newScripted(t)
			c := linkedClient(t, srv.URL, now.Add(-time.Hour))
			if tc.holdGen != 1 {
				tok, cl := token(t, c.dataDir, now.Add(-time.Hour), tc.holdGen)
				c.state.update(func(l *linkRecord) { l.Gen = 0 }) // let the save through
				if err := c.state.saveEntitlement(c.api, tok, cl, now, false); err != nil {
					t.Fatal(err)
				}
			}
			held, _ := c.state.Entitlement()
			offer, _ := token(t, c.dataDir, tc.offerIat, tc.offerGen)
			b, _ := json.Marshal(map[string]string{"entitlement": offer})
			srv.answer(200, "application/json", string(b))
			if _, err := c.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "this machine holds") {
				t.Fatalf("refresh: %v, want a refusal", err)
			}
			if tok, _ := c.state.Entitlement(); tok != held {
				t.Fatal("the older token was stored")
			}
		})
	}
}

// Unlink forgets the link only once the control plane has let go of the
// key: a 2xx, or a 403 saying it is already revoked or the account gone. A
// 401 (a clock more than five minutes out, as often as not) or a proxy's
// 403 leaves the link in place and says why.
func TestUnlinkNeedsTheControlPlaneToLetGo(t *testing.T) {
	for name, a := range map[string]struct {
		status      int
		ctype, body string
		unlinked    bool
		say         string
	}{
		"204":         {204, "", "", true, ""},
		"403 revoked": {403, "application/json", `{"error":"revoked"}`, true, ""},
		"403 deleted": {403, "application/json", `{"error":"deleted"}`, true, ""},
		"401":         {401, "application/json", `{"error":"unauthorized","message":"The request signature was not accepted."}`, false, "clock"},
		"html 403":    {403, "text/html", "<html>Forbidden</html>", false, "HTTP 403"},
		"503":         {503, "application/json", `{"error":"busy"}`, false, "HTTP 503"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newScripted(t)
			c := linkedClient(t, srv.URL, time.Now())
			srv.answer(a.status, a.ctype, a.body)
			err := c.Unlink(context.Background())
			if a.unlinked != (err == nil) || err != nil && !strings.Contains(err.Error(), a.say) {
				t.Fatalf("unlink: %v", err)
			}
			if c.state.Linked() == a.unlinked {
				t.Errorf("linked %v after unlink answered %d", c.state.Linked(), a.status)
			}
		})
	}
}

// A link.json whose device key has gone (an unlink in another process that
// raced a refresh) is no link at all, and a late save does not bring it
// back.
func TestALinkWithoutItsKeyIsNoLink(t *testing.T) {
	now := time.Now()
	c := linkedClient(t, "https://cloud.example", now)
	tok, cl := token(t, c.dataDir, now, 1)
	rec, _ := c.state.record()
	if rec == nil {
		t.Fatal("not linked")
	}
	// The other process forgets the link between this one's read and write.
	if err := c.state.forget(); err != nil {
		t.Fatal(err)
	}
	if err := c.state.saveEntitlement(c.api, tok, cl, now, false); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("save after unlink: %v, want ErrNotLinked", err)
	}
	// Even written back by hand, a link.json without a key is no link.
	b, _ := json.Marshal(rec)
	writeFileAtomic(filepath.Join(c.state.dir, linkFile), b)
	if c.state.Linked() {
		t.Error("linked without a device key")
	}
	if _, kind := c.state.Current(now); kind != None {
		t.Errorf("state %v without a device key, want none", kind)
	}
}
