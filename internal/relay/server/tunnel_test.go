package server

import (
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// Expired: relays refuse. That holds for a tunnel that is already up, not
// only for new hellos: once its entitlement lapses the tunnel is ended with
// a notice and its names stop routing. The daemon redials with the token
// it holds: a lapsed one is refused, a refreshed one welcomed.
func TestEntitlementExpiryEndsTunnel(t *testing.T) {
	h := startHosted(t, func(_ *Config, o *Options) { o.sweepEvery = 20 * time.Millisecond })
	d := newDevice(t)
	ent := h.is.entitlement(t, d, "ember", 3, h.clock.now()) // exp in 35 days
	tt := mustTunnel(t, h.testRelay, signedHello(t, d, ent))
	done := tunnelDone(t, h.Server, "ember.mirrin.test")

	h.clock.advance(34 * 24 * time.Hour)
	time.Sleep(100 * time.Millisecond) // several sweeps
	if _, ok := h.reg.Lookup("ember.mirrin.test"); !ok || tt.sess.IsClosed() {
		t.Fatal("tunnel ended before its entitlement lapsed")
	}

	h.clock.advance(2 * 24 * time.Hour)
	c := tt.nextControl(t, 5*time.Second)
	if c.T != wire.ControlNotice || !strings.Contains(c.Message, "expired") {
		t.Fatalf("got %+v", c)
	}
	waitSignal(t, 5*time.Second, "the expired session to close", tt.sess.CloseChan())
	waitSignal(t, 5*time.Second, "the expired tunnel to detach", done)
	if _, ok := h.reg.Lookup("ember.mirrin.test"); ok {
		t.Fatal("the expired name still routes")
	}
	closedSilently(t, dialName(t, h.testRelay, "ember.mirrin.test"))

	if e := refusal(t, h.testRelay, signedHello(t, d, ent)); e.Code != wire.CodeEntitlementExpired {
		t.Fatalf("redial with the lapsed token: %s", e.Code)
	}
	again := mustTunnel(t, h.testRelay, signedHello(t, d, h.is.entitlement(t, d, "ember", 3, h.clock.now())))
	time.Sleep(100 * time.Millisecond)
	if again.sess.IsClosed() {
		t.Fatal("a refreshed entitlement was ended too")
	}
}

// last_seen is when the relay last heard from the daemon. A laptop that
// falls asleep closes nothing; the relay notices only when a keepalive
// goes unanswered, and that later time must not be reported.
func TestLastSeenAfterSilentDrop(t *testing.T) {
	clk := &clock{}
	var logs logBuffer
	d := newDevice(t)
	cfg := selfHostConfig(t, Allow{Hostname: "h.test", Key: d.enc()})
	const keepaliveEvery = 250 * time.Millisecond
	r := startRelay(t, cfg, Options{Now: clk.now, Log: logs.logger(), keepalive: keepaliveEvery})
	p := newStallProxy(t, r.addr)
	tt, err := dialTunnelVia(t, r, p.addr(), signedHello(t, d, ""))
	if err != nil {
		t.Fatal(err)
	}

	done := tunnelDone(t, r.Server, "h.test")
	clk.advance(10 * time.Minute)
	time.Sleep(time.Second) // keepalives answered at the new time
	if tt.sess.IsClosed() {
		t.Fatal("a healthy tunnel was ended")
	}
	p.stall()
	asleep := clk.now()
	// An answer already past the proxy is heard before the jump below; on a
	// slow machine that takes a moment (less than the 250ms ping timeout).
	time.Sleep(100 * time.Millisecond)
	clk.advance(30 * time.Minute) // by the relay's clock, it notices much later

	client := r.https()
	k := base64.RawURLEncoding.EncodeToString(d.statusKey)
	var s statusJSON
	waitSignal(t, 10*time.Second, "the silent tunnel to detach", done)
	resp, err := client.Get("https://" + controlName + "/v1/status/h.test?k=" + k)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Online {
		t.Fatal("a lost tunnel is still online")
	}
	last, err := time.Parse(time.RFC3339, s.LastSeen)
	if err != nil {
		t.Fatal(err)
	}
	// The last answer came at most one keepalive before the daemon went
	// quiet, and last_seen is in whole seconds.
	if diff := last.Sub(asleep); diff < -(keepaliveEvery+time.Second) || diff > time.Second {
		t.Fatalf("last_seen %v is %v from when the daemon went quiet", last, diff)
	}
	if _, ok := r.reg.Lookup("h.test"); ok {
		t.Fatal("a lost tunnel still routes")
	}
	// Even at debug level the log names the daemon only by its /24.
	out := logs.String()
	if logs.lines("relay: tunnel down") == 0 || strings.Contains(out, "127.0.0.1") {
		t.Fatalf("log:\n%s", out)
	}
}

// A deny list reason the wire cannot carry verbatim is dropped from the
// message, so the daemon still gets "denied" and retry_after 3600 rather
// than a bare close.
func TestDeniedMessage(t *testing.T) {
	const base = "This twin's address is suspended."
	for _, why := range []string{
		"",
		"under review",           // no-break space
		"zero\u200bwidth",        // zero-width space
		"line separator",         // line separator
		"right\u202eto left",     // bidi override
		"tab\there",              // control
		"del\x7f",                // control
		"bad \xff utf-8",         // not UTF-8
		strings.Repeat("x", 600), // too long to show
	} {
		if m := deniedMessage(why); m != base {
			t.Errorf("why %q: message %q", why, m)
		}
	}
	why := "Recovered on 2026-09-01 — see the email we sent."
	if m := deniedMessage(why); m != base+" "+why {
		t.Errorf("printable why dropped: %q", m)
	}

	h := startHosted(t, nil)
	d := newDevice(t)
	ent := h.is.entitlement(t, d, "ember", 3, time.Now())
	tt := mustTunnel(t, h.testRelay, signedHello(t, d, ent))
	tok, err := entitle.SignDenyList(entitle.DenyList{Seq: 1, Iat: time.Now(),
		Keys: []entitle.Entry{{Value: d.enc(), Why: "abuse report"}}}, "dl-test-a", h.is.dl)
	if err != nil {
		t.Fatal(err)
	}
	h.deny.set(tok)
	h.polls.next(t)
	if c := tt.nextControl(t, 5*time.Second); c.T != wire.ControlNotice || c.Message != base {
		t.Fatalf("live tunnel got %+v", c)
	}
	waitSignal(t, 5*time.Second, "the denied tunnel to close", tt.sess.CloseChan())
	e := refusal(t, h.testRelay, signedHello(t, d, ent))
	if e.Code != wire.CodeDenied || e.RetryAfter != deniedRetry || e.Message != base {
		t.Fatalf("hello: %+v", e)
	}
}
