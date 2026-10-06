package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

func watchCfg(regions ...string) WatchConfig {
	c := &Config{Watch: WatchConfig{Relays: []string{"r1", "r2"}}}
	for _, r := range regions {
		c.Watch.Regions = append(c.Watch.Regions, Region{Name: r, URL: "http://127.0.0.1/" + r})
	}
	c.defaults()
	return c.Watch
}

func round(ok1, ok2 bool) Round {
	return Round{At: time.Now(), Relays: []RelayResult{{ID: "r1", OK: ok1}, {ID: "r2", OK: ok2}}}
}

// counts steps the evaluator through rounds and counts page triggers and
// resolves.
func counts(e *evaluator, views ...map[string]Round) (pages, resolves int, events []Event) {
	for _, v := range views {
		for _, ev := range e.step(view{now: time.Now(), rounds: v}) {
			events = append(events, ev)
			if ev.Page && ev.Raised {
				pages++
			}
			if ev.Page && !ev.Raised {
				resolves++
			}
		}
	}
	return
}

func repeat(n int, v map[string]Round) []map[string]Round {
	out := make([]map[string]Round, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestPagingRule(t *testing.T) {
	good := map[string]Round{"a": round(true, true), "b": round(true, true), "c": round(true, true)}
	bothDown := map[string]Round{"a": round(false, false), "b": round(false, false), "c": round(false, false)}

	t.Run("one relay down never pages", func(t *testing.T) {
		e := newEvaluator(watchCfg("a", "b", "c"))
		r1Down := map[string]Round{"a": round(false, true), "b": round(false, true), "c": round(false, true)}
		pages, _, evs := counts(e, repeat(60, r1Down)...)
		if pages != 0 {
			t.Fatalf("%d pages", pages)
		}
		if len(evs) != 1 || evs[0].Key != "relay-r1" || !evs[0].Raised {
			t.Fatalf("events %+v, want one relay-r1 email", evs)
		}
	})
	t.Run("one region's own network never pages", func(t *testing.T) {
		e := newEvaluator(watchCfg("a", "b", "c"))
		v := map[string]Round{"a": round(false, false), "b": round(true, true), "c": round(true, true)}
		if pages, _, _ := counts(e, repeat(60, v)...); pages != 0 {
			t.Fatalf("%d pages", pages)
		}
	})
	t.Run("two bad rounds do not page", func(t *testing.T) {
		e := newEvaluator(watchCfg("a", "b"))
		views := append(repeat(2, bothDown), good)
		views = append(views, repeat(2, bothDown)...)
		if pages, _, _ := counts(e, views...); pages != 0 {
			t.Fatalf("%d pages", pages)
		}
	})
	t.Run("game day: both relays down six minutes pages exactly once", func(t *testing.T) {
		e := newEvaluator(watchCfg("a", "b", "c"))
		views := append(repeat(5, good), repeat(6, bothDown)...)
		views = append(views, repeat(10, good)...)
		pages, resolves, evs := counts(e, views...)
		if pages != 1 || resolves != 1 {
			t.Fatalf("%d pages, %d resolves: %+v", pages, resolves, evs)
		}
		// Paged on the third bad round.
		pages, _, _ = counts(newEvaluator(watchCfg("a", "b")), repeat(2, bothDown)...)
		if pages != 0 {
			t.Fatal("paged before three rounds")
		}
	})
	t.Run("from two regions when the third is silent", func(t *testing.T) {
		e := newEvaluator(watchCfg("a", "b", "c"))
		v := map[string]Round{"a": round(false, false), "b": round(false, false)}
		if pages, _, _ := counts(e, repeat(3, v)...); pages != 1 {
			t.Fatalf("%d pages", pages)
		}
	})
	t.Run("a flapping outage stays one page", func(t *testing.T) {
		e := newEvaluator(watchCfg("a", "b"))
		views := repeat(3, bothDown)
		for range 5 { // a good round now and then doesn't resolve and re-page
			views = append(views, good, bothDown, bothDown)
		}
		if pages, resolves, _ := counts(e, views...); pages != 1 || resolves != 0 {
			t.Fatalf("%d pages, %d resolves", pages, resolves)
		}
	})
	t.Run("unknown is not failing", func(t *testing.T) {
		e := newEvaluator(watchCfg("a", "b"))
		v := map[string]Round{"a": round(false, false)} // b unreadable
		if pages, _, _ := counts(e, repeat(30, v)...); pages != 0 {
			t.Fatalf("%d pages", pages)
		}
	})
}

// A week of rounds with everything that should only ever email: relays
// failing one at a time, a region's own network failing, probes going
// quiet, and short blips of both relays. Zero pages.
func TestSoakWeekNoPages(t *testing.T) {
	e := newEvaluator(watchCfg("a", "b", "c"))
	rng := rand.New(rand.NewPCG(7, 20))
	pages := 0
	emails := 0
	blip := 0
	for range 7 * 24 * 60 {
		v := map[string]Round{}
		if blip == 0 && rng.IntN(2000) == 0 {
			blip = 2 // both relays down for two rounds: under the rule
		}
		for _, r := range []string{"a", "b", "c"} {
			if rng.IntN(100) == 0 {
				continue // the probe is silent this round
			}
			ok1, ok2 := rng.IntN(50) != 0, rng.IntN(50) != 0
			if rng.IntN(200) == 0 {
				ok1, ok2 = false, false // this region's own network
			}
			if blip > 0 {
				ok1, ok2 = false, false
			}
			v[r] = round(ok1, ok2)
		}
		if blip > 0 {
			blip--
		}
		for _, ev := range e.step(view{now: time.Now(), rounds: v}) {
			if ev.Page {
				pages++
			} else {
				emails++
			}
		}
	}
	if pages != 0 {
		t.Fatalf("%d pages in a week of noise", pages)
	}
	t.Logf("%d emails in the week", emails)
}

func TestCertAlarm(t *testing.T) {
	cfg := watchCfg("a", "b")
	e := newEvaluator(cfg)
	soon := round(true, true)
	soon.Relays[0].NotAfter = time.Now().Add(10 * 24 * time.Hour)
	_, _, evs := counts(e, map[string]Round{"a": soon})
	if len(evs) != 1 || evs[0].Key != "cert" || !evs[0].Raised || evs[0].Page {
		t.Fatalf("%+v", evs)
	}
	later := round(true, true)
	later.Relays[0].NotAfter = time.Now().Add(80 * 24 * time.Hour)
	if _, _, evs = counts(e, map[string]Round{"a": later}); len(evs) != 1 || evs[0].Raised {
		t.Fatalf("%+v", evs)
	}
}

// fakeProbe serves Results for one region, with a round the test sets.
type fakeProbe struct {
	*httptest.Server
	mu     sync.Mutex
	region string
	round  Round
	token  string
}

func newFakeProbe(t *testing.T, region, token string) *fakeProbe {
	f := &fakeProbe{region: region, token: token}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "no", 401)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(Results{Region: f.region, Rounds: []Round{f.round}})
	}))
	t.Cleanup(f.Close)
	return f
}

// set serves r as the latest round, stamped with the probe's region.
func (f *fakeProbe) set(r Round) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.Region = f.region
	f.round = r
}

// fakePager counts what it is sent; it fails the first fail requests.
type fakePager struct {
	*httptest.Server
	mu       sync.Mutex
	fail     int
	triggers int
	resolves int
	bodies   []map[string]any
}

func newFakePager(t *testing.T) *fakePager {
	p := &fakePager{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.fail > 0 {
			p.fail--
			http.Error(w, "busy", 503)
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		p.bodies = append(p.bodies, b)
		switch {
		case b["event"] == "trigger" || b["event_action"] == "trigger":
			p.triggers++
		default:
			p.resolves++
		}
		w.WriteHeader(202)
	}))
	t.Cleanup(p.Close)
	return p
}

// fakeSMTP is the smallest SMTP server net/smtp talks to: no STARTTLS, no
// auth. It keeps each message's body.
type fakeSMTP struct {
	addr string
	mu   sync.Mutex
	msgs []string
	fail bool
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeSMTP{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(l string) { io.WriteString(c, l+"\r\n") }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 fake")
		case strings.HasPrefix(cmd, "MAIL"):
			s.mu.Lock()
			fail := s.fail
			s.mu.Unlock()
			if fail {
				say("451 try later")
				continue
			}
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT"):
			say("250 ok")
		case cmd == "DATA":
			say("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.msgs = append(s.msgs, b.String())
			s.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (s *fakeSMTP) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

func (s *fakeSMTP) setFail(f bool) { s.mu.Lock(); s.fail = f; s.mu.Unlock() }

// The watcher over HTTP: probes in two regions, a pager that is busy at
// first, and mail. A game day pages exactly once even though the pager's
// first answer failed, and resolves once.
func TestWatcherGameDay(t *testing.T) {
	t.Setenv("CANARY_TEST_TOKEN", "sekrit")
	a, b := newFakeProbe(t, "a", "sekrit"), newFakeProbe(t, "b", "sekrit")
	pager := newFakePager(t)
	pager.fail = 1
	mail := newFakeSMTP(t)
	cfg := &Config{
		Probe: ProbeConfig{Relays: []ProbeRelay{{ID: "r1", Addrs: []string{"192.0.2.1:443"}}, {ID: "r2", Addrs: []string{"198.51.100.1:443"}}}},
		Watch: WatchConfig{
			Regions:  []Region{{Name: "a", URL: a.URL}, {Name: "b", URL: b.URL}},
			TokenEnv: "CANARY_TEST_TOKEN",
			Page:     PageConfig{Format: "webhook", URL: pager.URL},
			Email:    EmailConfig{SMTP: mail.addr, From: "canary@example.org", To: []string{"ops@example.org"}},
		},
	}
	cfg.defaults()
	if err := cfg.check(roleWatch); err != nil {
		t.Fatal(err)
	}
	w, err := newWatcher(cfg, newNotifier(cfg.Watch), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w.now = func() time.Time { return now }
	step := func(ok1, ok2 bool) {
		r := round(ok1, ok2)
		r.At = now
		a.set(r)
		b.set(r)
		w.tick(context.Background())
		now = now.Add(time.Minute)
	}
	for range 3 {
		step(true, true)
	}
	for range 6 {
		step(false, false)
	}
	for range 4 {
		step(true, true)
	}
	pager.mu.Lock()
	triggers, resolves := pager.triggers, pager.resolves
	pager.mu.Unlock()
	if triggers != 1 || resolves != 1 {
		t.Fatalf("pager saw %d triggers and %d resolves", triggers, resolves)
	}
	var paged, relayMail int
	for _, m := range mail.messages() {
		if strings.Contains(m, "Subject: [mirrin ops] PAGE:") {
			paged++
		}
		if strings.Contains(m, "Subject: [mirrin ops] alert: Relay r1") {
			relayMail++
		}
	}
	if paged != 1 || relayMail != 1 {
		t.Fatalf("%d page emails, %d relay-r1 emails:\n%s", paged, relayMail, strings.Join(mail.messages(), "\n---\n"))
	}

	// /healthz says the watcher is running rounds.
	rec := httptest.NewRecorder()
	w.handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz %d", rec.Code)
	}
	now = now.Add(time.Hour)
	rec = httptest.NewRecorder()
	w.handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 {
		t.Fatalf("healthz with no recent round: %d", rec.Code)
	}
}

// A page whose email copy fails is not sent to the pager again while the
// email is retried.
func TestPageNotResentWhenOnlyEmailFails(t *testing.T) {
	pager := newFakePager(t)
	mail := newFakeSMTP(t)
	mail.setFail(true)
	cfg := &Config{Watch: WatchConfig{Relays: []string{"r1", "r2"}, Regions: []Region{{Name: "a", URL: "http://127.0.0.1:1"}, {Name: "b", URL: "http://127.0.0.1:1"}},
		Page:  PageConfig{Format: "webhook", URL: pager.URL},
		Email: EmailConfig{SMTP: mail.addr, From: "c@example.org", To: []string{"o@example.org"}}}}
	cfg.defaults()
	w, _ := newWatcher(cfg, newNotifier(cfg.Watch), slog.New(slog.DiscardHandler))
	w.pending = []Event{{Key: pageKey, Page: true, Raised: true, Summary: "down"}}
	for range 3 {
		w.tick(context.Background())
	}
	mail.setFail(false)
	w.tick(context.Background())
	if pager.triggers != 1 {
		t.Fatalf("%d triggers", pager.triggers)
	}
	if n := len(mail.messages()); n < 1 || len(w.pending) != 0 {
		t.Fatalf("%d emails, %d pending", n, len(w.pending))
	}
}

func TestProbeResultsNeedTheToken(t *testing.T) {
	p := &prober{cfg: ProbeConfig{Region: "a"}, token: "sekrit"}
	for _, c := range []struct {
		auth string
		code int
	}{{"", 401}, {"Bearer nope", 401}, {"Bearer sekrit", 200}} {
		req := httptest.NewRequest("GET", "/v1/results", nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		p.handler().ServeHTTP(rec, req)
		if rec.Code != c.code {
			t.Errorf("%q: %d, want %d", c.auth, rec.Code, c.code)
		}
	}
}

// A stale probe round is unknown, not failing.
func TestStaleRoundIsUnknown(t *testing.T) {
	pr := newFakeProbe(t, "a", "")
	cfg := &Config{}
	cfg.defaults()
	w := &watcher{cfg: cfg, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	now := time.Now()
	r := round(false, false)
	r.At = now.Add(-10 * time.Minute)
	pr.set(r)
	if _, ok := w.fetch(context.Background(), Region{Name: "a", URL: pr.URL}, now); ok {
		t.Fatal("a ten-minute-old round counted")
	}
	r.At = now.Add(-30 * time.Second)
	pr.set(r)
	if _, ok := w.fetch(context.Background(), Region{Name: "a", URL: pr.URL}, now); !ok {
		t.Fatal("a fresh round didn't count")
	}
}

func TestChecks(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	now := time.Now()
	var tok string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			io.WriteString(w, "{}")
		case "/down":
			http.Error(w, "no", 502)
		case "/denylist":
			io.WriteString(w, tok+"\n")
		}
	}))
	defer srv.Close()
	cfg := &Config{DenyListKeys: map[string]string{"dl-2026a": entitle.EncodeKey(pub)}}
	cfg.defaults()
	w := &watcher{cfg: cfg, http: srv.Client()}
	sign := func(iat time.Time) string {
		s, err := entitle.SignDenyList(entitle.DenyList{Seq: 4, Iat: iat}, "dl-2026a", priv)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, c := range []struct {
		check Check
		tok   string
		want  string
	}{
		{Check{Name: "ok", Kind: "http", URL: srv.URL + "/ok"}, "", ""},
		{Check{Name: "down", Kind: "http", URL: srv.URL + "/down"}, "", "status 502"},
		{Check{Name: "fresh", Kind: "denylist", URL: srv.URL + "/denylist", MaxAge: 15 * time.Minute}, sign(now.Add(-time.Minute)), ""},
		{Check{Name: "stale", Kind: "denylist", URL: srv.URL + "/denylist", MaxAge: 15 * time.Minute}, sign(now.Add(-time.Hour)), "1h0m0s old"},
		{Check{Name: "forged", Kind: "denylist", URL: srv.URL + "/denylist", MaxAge: 15 * time.Minute}, "v4.public.bm9wZQ", "doesn't verify"},
	} {
		tok = c.tok
		err := w.runCheck(context.Background(), c.check, now)
		if (err == nil) != (c.want == "") || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", c.check.Name, err, c.want)
		}
	}
}

func TestPagerDutyBody(t *testing.T) {
	pager := newFakePager(t)
	t.Setenv("PD_KEY", "routing-key-1")
	n := newNotifier(WatchConfig{Page: PageConfig{Format: "pagerduty", URL: pager.URL, RoutingKeyEnv: "PD_KEY"}})
	if err := n.page(context.Background(), Event{Key: pageKey, Page: true, Raised: true, Summary: "down"}); err != nil {
		t.Fatal(err)
	}
	if err := n.page(context.Background(), Event{Key: pageKey, Page: true, Summary: "up"}); err != nil {
		t.Fatal(err)
	}
	b := pager.bodies
	if len(b) != 2 || b[0]["routing_key"] != "routing-key-1" || b[0]["event_action"] != "trigger" || b[0]["dedup_key"] != pageKey ||
		b[0]["payload"].(map[string]any)["severity"] != "critical" || b[1]["event_action"] != "resolve" {
		t.Fatalf("%+v", b)
	}
	n2 := newNotifier(WatchConfig{Page: PageConfig{Format: "pagerduty", RoutingKeyEnv: "UNSET_KEY_FOR_TEST"}})
	if err := n2.page(context.Background(), Event{Raised: true}); err == nil {
		t.Fatal("paged with no routing key")
	}
}

func TestEmailHeadersAreOneLine(t *testing.T) {
	n := newNotifier(WatchConfig{Email: EmailConfig{From: "a@example.org", To: []string{"b@example.org"}}})
	m := string(n.message(Event{Key: "check-x", Raised: true, Summary: "bad\r\nBcc: evil@example.org"}))
	head, _, _ := strings.Cut(m, "\r\n\r\n")
	if strings.Contains(head, "\r\nBcc:") {
		t.Fatalf("header injection:\n%s", head)
	}
}

// A round from another region is unknown: two regions pointed at one
// probe must not count as two.
func TestRoundFromAnotherRegionIsUnknown(t *testing.T) {
	pr := newFakeProbe(t, "a", "")
	cfg := &Config{}
	cfg.defaults()
	w := &watcher{cfg: cfg, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	now := time.Now()
	r := round(false, false)
	r.At = now
	pr.set(r)
	if _, ok := w.fetch(context.Background(), Region{Name: "a", URL: pr.URL}, now); !ok {
		t.Fatal("region a's own round didn't count")
	}
	if _, ok := w.fetch(context.Background(), Region{Name: "b", URL: pr.URL}, now); ok {
		t.Fatal("region a's round counted for region b")
	}
	// A results document naming one region but carrying another's round.
	pr.mu.Lock()
	pr.round.Region = "b"
	pr.mu.Unlock()
	if _, ok := w.fetch(context.Background(), Region{Name: "a", URL: pr.URL}, now); ok {
		t.Fatal("a round from region b counted for region a")
	}
}

// A stalled probe's last failing round counts once, however many watcher
// rounds see it; a repeated round neither advances nor resets the alarm.
func TestRepeatedRoundCountsOnce(t *testing.T) {
	a, b := newFakeProbe(t, "a", ""), newFakeProbe(t, "b", "")
	pager := newFakePager(t)
	mail := newFakeSMTP(t)
	cfg := &Config{Watch: WatchConfig{Relays: []string{"r1", "r2"},
		Regions: []Region{{Name: "a", URL: a.URL}, {Name: "b", URL: b.URL}},
		Page:    PageConfig{Format: "webhook", URL: pager.URL},
		Email:   EmailConfig{SMTP: mail.addr, From: "c@example.org", To: []string{"o@example.org"}}}}
	cfg.defaults()
	if err := cfg.check(roleWatch); err != nil {
		t.Fatal(err)
	}
	w, err := newWatcher(cfg, newNotifier(cfg.Watch), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w.now = func() time.Time { return now }
	down := round(false, false)
	down.At = now
	a.set(down)
	b.set(down)
	// One failing probe round, seen by three watcher rounds 40 s apart
	// (all still fresh): not three bad rounds.
	for range 3 {
		w.tick(context.Background())
		now = now.Add(40 * time.Second)
	}
	pager.mu.Lock()
	n := pager.triggers
	pager.mu.Unlock()
	if n != 0 {
		t.Fatalf("one probe round paged (%d triggers)", n)
	}
	// Two more distinct failing rounds make three: that pages.
	for range 2 {
		down.At = now
		a.set(down)
		b.set(down)
		w.tick(context.Background())
		now = now.Add(time.Minute)
	}
	pager.mu.Lock()
	n = pager.triggers
	pager.mu.Unlock()
	if n != 1 {
		t.Fatalf("three distinct failing rounds: %d triggers, want 1", n)
	}
}

func TestTrimPendingKeepsEachPageOnce(t *testing.T) {
	var p []Event
	for i := range 120 {
		p = append(p, Event{Key: fmt.Sprint("e", i), Page: i == 10 || i == 110})
	}
	got := trimPending(p)
	pages := map[string]int{}
	for _, e := range got {
		if e.Page {
			pages[e.Key]++
		}
	}
	if len(got) != 51 || pages["e10"] != 1 || pages["e110"] != 1 {
		t.Fatalf("%d kept, pages %v", len(got), pages)
	}
	if short := p[:40]; len(trimPending(short)) != 40 {
		t.Fatal("a short queue was trimmed")
	}
}
