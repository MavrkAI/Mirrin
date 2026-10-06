package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

type route struct {
	method, path, body string
	scope              devices.Scope
	remote             bool // answers on listeners other devices reach
}

var routeTable = []route{
	{"GET", "/status", "", devices.View, true},
	{"POST", "/message", `{"text":"hi"}`, devices.Chat, true},
	{"POST", "/message/stream", `{"text":"hi"}`, devices.Chat, true},
	{"GET", "/screen", "", devices.View, true},
	{"GET", "/screen/shot?path=x", "", devices.Chat, true}, // or approve (orScope)
	{"GET", "/browser/state", "", devices.Chat, true},      // or approve (orScope)
	{"GET", "/events", "", devices.View, true},
	{"GET", "/usage", "", devices.View, true},
	{"POST", "/approvals/1/approve", "", devices.Approve, true},
	{"POST", "/approvals/1/deny", "", devices.Approve, true},
	{"POST", "/screen/tips/off", "", devices.Chat, true},
	{"POST", "/screen/facts/1/undo", "", devices.Chat, true},
	{"POST", "/screen/reminders/1/done", "", devices.Chat, true},
	{"POST", "/screen/portrait/ack", "", devices.Chat, true},
	{"POST", "/pause", `{"paused":false}`, devices.Admin, false},
	{"POST", "/protocols/run", `{"name":"morning"}`, devices.Admin, false},
	{"POST", "/jobs/run", `{"name":"patterns"}`, devices.Admin, false},
	{"GET", "/health", "", devices.View, false},
	{"POST", "/health/run", "", devices.Admin, false},
	{"GET", "/memory/facts", "", devices.Admin, false},
	{"POST", "/memory/facts", `{"content":"likes tea"}`, devices.Admin, false},
	{"DELETE", "/memory/facts/1", "", devices.Admin, false},
	{"GET", "/memory/audit", "", devices.Admin, false},
	{"GET", "/channels/list", "", devices.Admin, false},
	{"POST", "/channels/connect", `{"name":"telegram"}`, devices.Admin, false},
	{"POST", "/channels/whatsapp/pair", `{}`, devices.Admin, false},
	{"GET", "/accounts/list", "", devices.Admin, false},
	{"POST", "/accounts/google/disconnect", "", devices.Admin, false},
	{"POST", "/accounts/google/client/replace", "", devices.Admin, false},
	{"GET", "/accounts/model", "", devices.Admin, false},
	{"POST", "/accounts/model", `{"provider":"openai","key":"sk-good-0123456789"}`, devices.Admin, false},
	{"POST", "/accounts/model/check", `{"provider":"openai","key":"sk-good-0123456789"}`, devices.Admin, false},
	{"GET", "/protocols/installed", "", devices.Admin, false},
	{"POST", "/protocols/install", `{"name":"https://example.com/pack.git"}`, devices.Admin, false},
	{"POST", "/protocols/morning/enabled", `{"enabled":false}`, devices.Admin, false},
	{"POST", "/protocols/morning/skip", "", devices.Admin, false},
	{"POST", "/protocols/morning/schedule", `{"schedule":"30 8 * * 1-5"}`, devices.Admin, false},
	{"GET", "/devices", "", devices.Admin, false},
	{"POST", "/devices/offers", `{"kind":"pwa"}`, devices.Admin, false},
}

// orScope is a second scope that also opens a route in routeTable.
var orScope = map[string]devices.Scope{"/screen/shot?path=x": devices.Approve, "/browser/state": devices.Approve}

// TestRouteMatrix: every route, from every listener, with every credential.
// The master key works only on loopback (and, for what an old-style code
// gave, on the old plain-HTTP listener); each device only within its scopes;
// settings only on this computer.
func TestRouteMatrix(t *testing.T) {
	e := newEnv(t)
	type cred struct {
		name  string
		tok   string
		scope devices.Scope // "" for none, "*" for the master key
	}
	creds := []cred{{name: "none"}, {name: "master", tok: master, scope: "*"}}
	for _, sc := range devices.AllScopes {
		_, tok, err := e.store.Add("only "+string(sc), devices.KindPWA, []devices.Scope{sc}, "tailscale", "")
		if err != nil {
			t.Fatal(err)
		}
		creds = append(creds, cred{name: "device:" + string(sc), tok: tok, scope: sc})
	}
	for _, rt := range routeTable {
		for _, at := range []where{onLoopback, onRemote, onLegacy} {
			for _, c := range creds {
				want := http.StatusOK
				switch {
				case !rt.remote && at != onLoopback:
					want = http.StatusNotFound // settings answer only on this computer
				case c.scope == "":
					want = http.StatusUnauthorized
				case c.scope == "*" && at == onRemote:
					want = http.StatusUnauthorized // the master key never works over the remote listener
				case c.scope == "*":
					want = http.StatusOK
				case c.scope != rt.scope && c.scope != orScope[rt.path]:
					want = http.StatusForbidden
				}
				q := req{method: rt.method, path: rt.path, body: rt.body}
				if c.tok != "" {
					q.header = bearer(c.tok)
				}
				w := e.do(at, q)
				if w.Code != want {
					t.Errorf("%s %s on %s with %s: %d, want %d (%s)", rt.method, rt.path, at, c.name, w.Code, want, strings.TrimSpace(w.Body.String()))
				}
			}
		}
	}
}

func TestLoopbackRefusesOtherHostNames(t *testing.T) {
	e := newEnv(t)
	for host, want := range map[string]int{
		"evil.example":          http.StatusMisdirectedRequest,
		"evil.example:7742":     http.StatusMisdirectedRequest,
		"100.64.0.1:7742":       http.StatusMisdirectedRequest,
		"127.0.0.1:7742":        http.StatusOK,
		"localhost:7742":        http.StatusOK,
		"[::1]:7742":            http.StatusOK,
		"localhost:9000":        http.StatusOK, // an SSH tunnel's port
		"LOCALHOST:7742":        http.StatusOK,
		"127.0.0.1.nip.io:7742": http.StatusMisdirectedRequest,
	} {
		r := req{path: "/status", header: bearer(master)}
		w := e.doHost(onLoopback, host, r)
		if w.Code != want {
			t.Errorf("Host %s: %d, want %d", host, w.Code, want)
		}
	}
}

func (e *env) doHost(at where, host string, q req) *httptest.ResponseRecorder {
	q.host = host
	return e.do(at, q)
}

// httptestRecorderFor serves one request as if it arrived on l.
func httptestRecorderFor(e *env, l listener, path string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://127.0.0.1:7742"+path, nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), listenerKey, l)))
	return w
}

func TestRemoteListenerAnswersOnlyItsNames(t *testing.T) {
	e := newEnv(t)
	_, tok, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	w := e.doHost(onRemote, "attacker.example", req{path: "/status", header: bearer(tok)})
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("other Host on the remote listener: %d", w.Code)
	}
	w = e.do(onRemote, req{path: "/status", header: bearer(tok)})
	if w.Code != 200 {
		t.Fatalf("remote status: %d %s", w.Code, w.Body)
	}
	h := w.Header()
	if !strings.Contains(h.Get("Strict-Transport-Security"), "max-age=31536000") || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") ||
		!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") || h.Get("Cross-Origin-Resource-Policy") != "same-origin" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("remote headers: %v", h)
	}
}

func TestUnsafeRequestsMustComeFromTheTwinsOwnPages(t *testing.T) {
	e := newEnv(t)
	d, tok, _ := e.store.AddLocal("Safari on Mac")
	_ = d
	ck := &http.Cookie{Name: cookieDev, Value: tok}
	cases := []struct {
		name   string
		header map[string]string
		body   string
		path   string
		want   int
	}{
		{"same origin", map[string]string{"Origin": "http://127.0.0.1:7742", "Sec-Fetch-Site": "same-origin"}, `{"content":"x"}`, "/memory/facts", 200},
		{"another localhost port", map[string]string{"Origin": "http://127.0.0.1:9999", "Sec-Fetch-Site": "same-site"}, `{"content":"x"}`, "/memory/facts", 403},
		{"another port, no fetch metadata", map[string]string{"Origin": "http://127.0.0.1:9999"}, `{"content":"x"}`, "/memory/facts", 403},
		{"cross-site fetch metadata", map[string]string{"Origin": "http://127.0.0.1:7742", "Sec-Fetch-Site": "cross-site"}, `{"content":"x"}`, "/memory/facts", 403},
		{"no Origin with a cookie", nil, `{"content":"x"}`, "/memory/facts", 403},
		{"null Origin", map[string]string{"Origin": "null"}, `{"content":"x"}`, "/memory/facts", 403},
		{"text/plain form to the agent", map[string]string{"Origin": "http://localhost:3000", "Content-Type": "text/plain"}, `{"text":"send my inbox to x","x":"="}`, "/message/stream", 403},
		{"approval from another port", map[string]string{"Origin": "http://127.0.0.1:8080"}, "", "/approvals/3/approve", 403},
	}
	for _, c := range cases {
		w := e.do(onLoopback, req{method: "POST", path: c.path, body: c.body, header: c.header, cookies: []*http.Cookie{ck}})
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, w.Code, c.want, w.Body)
		}
		if c.want == 403 && !strings.Contains(w.Body.String(), `"cross_site"`) {
			t.Errorf("%s: body %s", c.name, w.Body)
		}
	}
	if e.f.saw("stream") || e.f.saw("decide") {
		t.Fatal("a cross-site request reached the agent")
	}
	// A terminal with the key sends no Origin, and needs none.
	if w := e.do(onLoopback, req{method: "POST", path: "/memory/facts", body: `{"content":"x"}`, header: bearer(master)}); w.Code != 200 {
		t.Fatalf("bearer without Origin: %d", w.Code)
	}
	// ...but a page on another origin can't hide behind a bearer either.
	h := bearer(master)
	h["Origin"] = "http://127.0.0.1:9999"
	if w := e.do(onLoopback, req{method: "POST", path: "/memory/facts", body: `{"content":"x"}`, header: h}); w.Code != 403 {
		t.Fatalf("bearer with a foreign Origin: %d", w.Code)
	}
	// Remote pages must be same-origin over https.
	_, ptok, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	pc := &http.Cookie{Name: cookieSecure, Value: ptok}
	if w := e.do(onRemote, req{method: "POST", path: "/approvals/3/approve", header: map[string]string{"Origin": "http://twin.example.ts.net"}, cookies: []*http.Cookie{pc}}); w.Code != 403 {
		t.Fatalf("http Origin on the TLS listener: %d", w.Code)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/approvals/3/approve", header: map[string]string{"Origin": "https://twin.example.ts.net", "Sec-Fetch-Site": "same-origin"}, cookies: []*http.Cookie{pc}}); w.Code != 200 {
		t.Fatalf("same-origin approval from the phone: %d %s", w.Code, w.Body)
	}
}

func TestProxiedRequestsNeverCountAsThisComputer(t *testing.T) {
	e := newEnv(t)
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "Via", "Tailscale-User-Login", "X-Real-IP"} {
		hdr := bearer(master)
		hdr[h] = "1.2.3.4"
		w := e.do(onLoopback, req{path: "/status", header: hdr})
		if w.Code != http.StatusMisdirectedRequest || !strings.Contains(w.Body.String(), `"proxied"`) {
			t.Errorf("%s: %d %s", h, w.Code, w.Body)
		}
	}
	// Regression: a signed webhook through a tunnel (Twilio via Tailscale
	// Funnel or ngrok) and the liveness check still work. A tunnel keeps the
	// public name in Host, which is never a loopback name.
	for _, host := range []string{"mavrk.tail1234.ts.net", "abcd.ngrok-free.app", "127.0.0.1:7742"} {
		fw := map[string]string{"X-Forwarded-For": "1.2.3.4", "X-Forwarded-Proto": "https", "Tailscale-Funnel-Request": "?1"}
		if w := e.doHost(onLoopback, host, req{method: "POST", path: "/phone/turn", header: fw}); w.Code != 200 || w.Body.String() != "twilio ok" {
			t.Errorf("webhook through a tunnel as %s: %d %s", host, w.Code, w.Body)
		}
		if w := e.doHost(onLoopback, host, req{path: "/healthz", header: fw}); w.Code != 200 {
			t.Errorf("healthz through a tunnel as %s: %d", host, w.Code)
		}
		// Nothing else comes through, even with the master key.
		hdr := bearer(master)
		hdr["X-Forwarded-For"] = "1.2.3.4"
		if w := e.doHost(onLoopback, host, req{path: "/status", header: hdr}); w.Code != http.StatusMisdirectedRequest {
			t.Errorf("/status through a tunnel as %s: %d", host, w.Code)
		}
	}
	// ...and a public name without proxy headers is still the wrong address.
	if w := e.doHost(onLoopback, "mavrk.tail1234.ts.net", req{path: "/status", header: bearer(master)}); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("/status as a public name: %d", w.Code)
	}
	// A webhook prefix covers what's under it, and nothing that merely
	// starts the same way.
	if !e.s.proxyExempt("/phone/turn") || e.s.proxyExempt("/phonebook") || e.s.proxyExempt("/status") {
		t.Error("proxyExempt matches differently from the routes")
	}
}

func TestHealthzIsPublicEverywhere(t *testing.T) {
	e := newEnv(t)
	for _, at := range []where{onLoopback, onRemote, onLegacy} {
		w := e.do(at, req{path: "/healthz"})
		if w.Code != 200 || w.Body.String() != "ok\n" {
			t.Errorf("%s: %d %q", at, w.Code, w.Body)
		}
	}
}

func TestUnpairedGetsAClearAnswer(t *testing.T) {
	e := newEnv(t)
	w := e.do(onLegacy, req{path: "/screen"})
	var body apiError
	if w.Code != 401 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if body.Error != "not_paired" || !strings.Contains(body.Message, "isn't paired with Mirrin") || !strings.Contains(body.Fix, "mirrin pair --screen") {
		t.Fatalf("401 body %+v", body)
	}
	// A browser opening the page gets the same as a page, not JSON.
	w = e.do(onLegacy, req{path: "/ui", header: map[string]string{"Accept": "text/html"}})
	if w.Code != 401 || !strings.Contains(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), "mirrin pair --screen") {
		t.Fatalf("page 401: %d %s", w.Code, w.Body)
	}
	// On this computer it points at the menu.
	w = e.do(onLoopback, req{path: "/screen"})
	if !strings.Contains(w.Body.String(), "menu") {
		t.Fatalf("loopback 401: %s", w.Body)
	}
	// A revoked device hears the same.
	d, tok, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	_, _ = e.store.Revoke(d.ID)
	if w := e.do(onLegacy, req{path: "/screen", header: bearer(tok)}); w.Code != 401 || !strings.Contains(w.Body.String(), "not_paired") {
		t.Fatalf("revoked: %d %s", w.Code, w.Body)
	}
}

// The menu's links carry the master key; the browser must end up with a
// key of its own, and the address bar without either.
func TestTokenLinkBecomesTheBrowsersOwnKey(t *testing.T) {
	e := newEnv(t)
	w := e.do(onLoopback, req{path: "/ui?token=" + master + "&mode=orb", header: map[string]string{"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Safari/605.1.15"}})
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/ui?mode=orb" {
		t.Fatalf("redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	c := cookieFrom(w, cookieDev)
	if c == nil || c.Value == master || !devices.LooksLikeToken(c.Value) {
		t.Fatalf("cookie %+v", c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge != cookieMaxAge || c.Path != "/" {
		t.Fatalf("cookie attributes %+v", c)
	}
	for _, sc := range w.Result().Cookies() {
		if strings.Contains(sc.Value, master) {
			t.Fatalf("a cookie holds the master key: %+v", sc)
		}
	}
	d, ok := e.store.Authenticate(c.Value)
	if !ok || !d.Local() || d.Name != "Safari on Mac" {
		t.Fatalf("browser's device %+v %v", d, ok)
	}
	if w := e.do(onLoopback, req{path: "/ui", cookies: []*http.Cookie{c}}); w.Code != 200 || !strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("page with its cookie: %d", w.Code)
	}
	// That browser key is this computer's only.
	if w := e.do(onLegacy, req{path: "/screen", cookies: []*http.Cookie{c}}); w.Code != 401 {
		t.Fatalf("a local browser's key on the LAN listener: %d", w.Code)
	}
	// Elsewhere ?token= does nothing but leave the address bar.
	w = e.do(onLegacy, req{path: "/ui?token=" + master})
	if w.Code != http.StatusFound || cookieFrom(w, cookieDev) != nil {
		t.Fatalf("?token= on the LAN listener: %d %v", w.Code, w.Result().Cookies())
	}
}

// Regression (memory.go:65): opening Memory or Health from the menu replaced
// the screen's year-long cookie with a Strict session cookie, so the screen
// signed out when the browser restarted.
func TestOpeningMemoryKeepsTheScreensCookie(t *testing.T) {
	e := newEnv(t)
	w := e.do(onLoopback, req{path: "/ui?token=" + master})
	screen := cookieFrom(w, cookieDev)
	if screen == nil {
		t.Fatal("no cookie from the screen link")
	}
	for _, page := range []string{"/memory", "/health", "/channels", "/accounts", "/protocols"} {
		w = e.do(onLoopback, req{path: page + "?token=" + master, cookies: []*http.Cookie{screen}})
		if w.Code != http.StatusFound {
			t.Fatalf("%s link: %d", page, w.Code)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == cookieDev || c.Name == "antbot_token" {
				t.Fatalf("%s replaced the screen's cookie: %+v", page, c)
			}
		}
		w = e.do(onLoopback, req{path: page, header: map[string]string{"Accept": "text/html"}, cookies: []*http.Cookie{screen}})
		if w.Code != 200 {
			t.Fatalf("%s with the screen's cookie: %d", page, w.Code)
		}
		// A page load keeps the cookie alive, with the same attributes.
		if c := cookieFrom(w, cookieDev); c == nil || c.Value != screen.Value || c.MaxAge != cookieMaxAge || c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("%s refreshed the cookie as %+v", page, c)
		}
	}
	if w := e.do(onLoopback, req{path: "/screen", cookies: []*http.Cookie{screen}}); w.Code != 200 {
		t.Fatalf("screen after the other pages: %d", w.Code)
	}
}

// The cookies' names: Mirrin's, then the ones browsers signed in before the
// rename still carry. The tests below loop over the old names, so a rename
// of one would leave them passing while that browser is signed out.
func TestCookieNames(t *testing.T) {
	if cookieDev != "mirrin_dev" || cookieSecure != "__Host-mirrin" {
		t.Errorf("device cookies %q and %q", cookieDev, cookieSecure)
	}
	if !slices.Equal(legacyDeviceCookies, []string{"antbot_dev", "__Host-antbot"}) {
		t.Errorf("AntBot's device cookies %v", legacyDeviceCookies)
	}
	if !slices.Equal(legacyCookies, []string{"antbot_token", "openhuman_token"}) {
		t.Errorf("the master-key cookies %v", legacyCookies)
	}
}

// Screens paired the old way carry the master key in antbot_token (or, from
// before the rename, openhuman_token). They keep working, on a key of their
// own from their next request.
func TestOldCookieMovesOntoItsOwnKey(t *testing.T) {
	e := newEnv(t)
	for _, name := range legacyCookies {
		w := e.do(onLoopback, req{path: "/screen", cookies: []*http.Cookie{{Name: name, Value: master}}})
		if w.Code != 200 {
			t.Fatalf("%s on loopback: %d", name, w.Code)
		}
		c := cookieFrom(w, cookieDev)
		if c == nil || !devices.LooksLikeToken(c.Value) {
			t.Fatalf("%s: no key of its own: %v", name, w.Result().Cookies())
		}
		if old := cookieFrom(w, name); old == nil || old.MaxAge >= 0 {
			t.Fatalf("%s wasn't cleared: %+v", name, old)
		}
		if w := e.do(onLoopback, req{path: "/screen", cookies: []*http.Cookie{c}}); w.Code != 200 {
			t.Fatalf("with the new cookie: %d", w.Code)
		}
	}
	// A wall screen on the LAN listener becomes a device the owner is told about.
	ua := map[string]string{"User-Agent": "Mozilla/5.0 (X11; Linux aarch64) Chrome/120.0"}
	w := e.do(onLegacy, req{path: "/screen", header: ua, cookies: []*http.Cookie{{Name: "antbot_token", Value: master}}})
	if w.Code != 200 {
		t.Fatalf("old screen on the LAN listener: %d %s", w.Code, w.Body)
	}
	c := cookieFrom(w, cookieDev)
	d, ok := e.store.Authenticate(c.Value)
	if !ok || d.Local() || d.Has(devices.Admin) || !d.Has(devices.Approve) || d.Name != "Chrome on Linux" {
		t.Fatalf("migrated screen %+v", d)
	}
	ev := e.pairedEvents(1)
	if len(ev) != 1 || ev[0].How != HowLegacyScreen || ev[0].Device.ID != d.ID || ev[0].IP != "100.64.0.9" {
		t.Fatalf("announcements %+v", ev)
	}
	// The page's other requests racing in with the old cookie share that key.
	w2 := e.do(onLegacy, req{path: "/status", header: ua, cookies: []*http.Cookie{{Name: "antbot_token", Value: master}}})
	if c2 := cookieFrom(w2, cookieDev); c2 == nil || c2.Value != c.Value {
		t.Fatalf("a second device for the same screen: %v", c2)
	}
	// Never over the TLS listener.
	if w := e.do(onRemote, req{path: "/screen", cookies: []*http.Cookie{{Name: "antbot_token", Value: master}}}); w.Code != 401 {
		t.Fatalf("master-key cookie on the remote listener: %d", w.Code)
	}
}

// A browser signed in before the rename carries its key in AntBot's cookie
// (antbot_dev over plain HTTP, __Host-antbot over TLS). It stays signed in,
// moves to the new cookie on the spot, and the old one is expired the way
// browsers accept.
func TestAntBotDeviceCookieStillSignsIn(t *testing.T) {
	e := newEnv(t)
	_, local, _ := e.store.AddLocal("Safari on Mac")
	w := e.do(onLoopback, req{path: "/screen", cookies: []*http.Cookie{{Name: "antbot_dev", Value: local}}})
	if w.Code != 200 {
		t.Fatalf("antbot_dev on loopback: %d", w.Code)
	}
	if c := cookieFrom(w, cookieDev); c == nil || c.Value != local || c.MaxAge != cookieMaxAge {
		t.Fatalf("no %s for the same key: %v", cookieDev, w.Result().Cookies())
	}
	if old := cookieFrom(w, "antbot_dev"); old == nil || old.MaxAge >= 0 {
		t.Fatalf("antbot_dev wasn't expired: %+v", old)
	}

	_, phone, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	same := map[string]string{"Origin": "https://twin.example.ts.net", "Sec-Fetch-Site": "same-origin"}
	w = e.do(onRemote, req{method: "POST", path: "/approvals/3/approve", header: same, cookies: []*http.Cookie{{Name: "__Host-antbot", Value: phone}}})
	if w.Code != 200 {
		t.Fatalf("__Host-antbot on the TLS listener: %d %s", w.Code, w.Body)
	}
	if c := cookieFrom(w, cookieSecure); c == nil || c.Value != phone || !c.Secure {
		t.Fatalf("no %s for the same key: %v", cookieSecure, w.Result().Cookies())
	}
	if old := cookieFrom(w, "__Host-antbot"); old == nil || old.MaxAge >= 0 || !old.Secure || old.Path != "/" || old.Domain != "" {
		t.Fatalf("__Host-antbot wasn't expired so a browser accepts it: %+v", old)
	}

	// The old cookie is a credential like the new one: a change it carries
	// must come from the twin's own page.
	if w := e.do(onLoopback, req{method: "POST", path: "/memory/facts", body: `{"content":"x"}`, cookies: []*http.Cookie{{Name: "antbot_dev", Value: local}}}); w.Code != 403 {
		t.Fatalf("a cross-site change with only antbot_dev: %d", w.Code)
	}
	// Only the cookie for its own transport counts, and only with a live key.
	if w := e.do(onRemote, req{path: "/status", cookies: []*http.Cookie{{Name: "antbot_dev", Value: phone}}}); w.Code != 401 {
		t.Fatalf("antbot_dev over TLS: %d", w.Code)
	}
	if w := e.do(onLoopback, req{path: "/status", cookies: []*http.Cookie{{Name: "antbot_dev", Value: "abt1_not-a-key"}}}); w.Code != 401 || cookieFrom(w, cookieDev) != nil {
		t.Fatalf("a made-up key in antbot_dev: %d %v", w.Code, w.Result().Cookies())
	}
}

// Regression (api.go:167): "Run now" on the Protocols page got 401 because
// the route took only a bearer token, and the page sends its cookie.
func TestRunNowWorksFromTheProtocolsPage(t *testing.T) {
	e := newEnv(t)
	c := cookieFrom(e.do(onLoopback, req{path: "/protocols?token=" + master}), cookieDev)
	w := e.do(onLoopback, req{method: "POST", path: "/protocols/run", body: `{"name":"morning"}`, cookies: []*http.Cookie{c},
		header: map[string]string{"Origin": "http://127.0.0.1:7742", "Content-Type": "application/json"}})
	if w.Code != 200 || !e.f.saw("run:morning") {
		t.Fatalf("run now: %d %s", w.Code, w.Body)
	}
}

// Regression (accounts.go:93): the Google redirect was built from api.listen,
// so with 0.0.0.0 or a Tailscale address Google refused it.
func TestGoogleRedirectIsAlwaysLoopback(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:7742", "100.101.102.103:7742", "127.0.0.1:7742"} {
		e := newEnv(t)
		e.s.addr = listen
		e.s.remote = true
		if got := e.s.UIURL(); !strings.HasPrefix(got, "http://127.0.0.1:7742/ui?token=") {
			t.Errorf("%s: screen link %s", listen, got)
		}
		for host, want := range map[string]string{
			"127.0.0.1:7742": "http://127.0.0.1:7742/oauth/google",
			"localhost:7742": "http://localhost:7742/oauth/google",
		} {
			w := e.doHost(onLoopback, host, req{method: "POST", path: "/accounts/google/connect", header: bearer(master)})
			if w.Code != 200 || e.f.redirect != want {
				t.Errorf("%s via %s: %d redirect %q", listen, host, w.Code, e.f.redirect)
			}
		}
	}
}

func TestGoogleCallbackNeedsTheBrowserThatStartedIt(t *testing.T) {
	e := newEnv(t)
	c := cookieFrom(e.do(onLoopback, req{path: "/accounts?token=" + master}), cookieDev)
	w := e.do(onLoopback, req{method: "POST", path: "/accounts/google/connect", cookies: []*http.Cookie{c}, header: map[string]string{"Origin": "http://127.0.0.1:7742"}})
	flow := cookieFrom(w, oauthCookie)
	if w.Code != 200 || flow == nil || flow.Value == "st4te" || !flow.HttpOnly {
		t.Fatalf("connect: %d %+v", w.Code, flow)
	}
	// A link someone else sends (no flow cookie) does nothing.
	w = e.do(onLoopback, req{path: "/oauth/google?state=st4te&code=abc", header: map[string]string{"Accept": "text/html"}})
	if w.Code != 400 || e.f.finished || !strings.Contains(w.Body.String(), "different browser") {
		t.Fatalf("callback without the flow cookie: %d finished=%v", w.Code, e.f.finished)
	}
	w = e.do(onLoopback, req{path: "/oauth/google?state=st4te&code=abc", cookies: []*http.Cookie{flow}})
	if w.Code != http.StatusFound || !e.f.finished || w.Header().Get("Location") != "/accounts" {
		t.Fatalf("callback: %d finished=%v", w.Code, e.f.finished)
	}
	// It no longer hands out a session cookie of its own.
	if cookieFrom(w, cookieDev) != nil || cookieFrom(w, "antbot_token") != nil {
		t.Fatalf("callback minted a session: %v", w.Result().Cookies())
	}
	// And only on this computer.
	if w := e.do(onLegacy, req{path: "/oauth/google?state=st4te&code=abc"}); w.Code != 404 {
		t.Fatalf("callback on the LAN listener: %d", w.Code)
	}
}

// Google's answer is put in plain words; whatever else a link puts in the
// address never shows on the twin's own page.
func TestGoogleCallbackErrorIsInPlainWords(t *testing.T) {
	e := newEnv(t)
	html := map[string]string{"Accept": "text/html"}
	w := e.do(onLoopback, req{path: "/oauth/google?error=access_denied", header: html})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "didn&#39;t go through") {
		t.Fatalf("access_denied: %d %s", w.Code, w.Body)
	}
	w = e.do(onLoopback, req{path: "/oauth/google?error=%3Cscript%3Ealert(1)%3C/script%3E+call+0800+123", header: html})
	if body := w.Body.String(); w.Code != 400 || strings.Contains(body, "<script") || strings.Contains(body, "0800") {
		t.Fatalf("a link's own text reached the page: %d %s", w.Code, body)
	}
	if e.f.finished {
		t.Fatal("an error finished a sign-in")
	}
}

func TestRemoteRateLimit(t *testing.T) {
	e := newEnv(t)
	e.s.limiter = newIPLimiter(1, 3)
	var codes []int
	for i := 0; i < 5; i++ {
		codes = append(codes, e.do(onLegacy, req{path: "/healthz"}).Code)
	}
	if codes[2] != 200 || codes[3] != 429 {
		t.Fatalf("codes %v", codes)
	}
	w := e.do(onLegacy, req{path: "/healthz"})
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// Another address has its own bucket, and this computer has none.
	if w := e.do(onLegacy, req{path: "/healthz", ip: "100.64.0.10"}); w.Code != 200 {
		t.Fatalf("other address: %d", w.Code)
	}
	for i := 0; i < 10; i++ {
		if w := e.do(onLoopback, req{path: "/healthz"}); w.Code != 200 {
			t.Fatalf("loopback limited: %d", w.Code)
		}
	}
}

type fakeConn struct {
	net.Conn
	local net.Addr
}

func (c fakeConn) LocalAddr() net.Addr { return c.local }

func TestConnectionsAreClassifiedByTheirListener(t *testing.T) {
	tcp := func(s string) net.Addr { a, _ := net.ResolveTCPAddr("tcp", s); return a }
	cases := []struct {
		l        listener
		classify bool
		local    string
		want     listenerKind
		via      string
	}{
		{listener{kind: kindLegacy}, true, "127.0.0.1:7742", kindLoopback, "loopback"},
		{listener{kind: kindLegacy}, true, "[::1]:7742", kindLoopback, "loopback"},
		{listener{kind: kindLegacy}, true, "[::ffff:127.0.0.1]:7742", kindLoopback, "loopback"},
		{listener{kind: kindLegacy}, true, "100.101.1.2:7742", kindLegacy, "tailscale"},
		{listener{kind: kindLegacy}, true, "192.168.1.20:7742", kindLegacy, "lan"},
		{listener{kind: kindLoopback, via: "loopback"}, false, "127.0.0.1:7742", kindLoopback, "loopback"},
		{listener{kind: kindLoopback, via: "loopback"}, false, "192.168.1.20:7742", kindRefused, ""},
		{listener{kind: kindRemote, via: "relay r1"}, false, "127.0.0.1:443", kindRemote, "relay r1"},
	}
	for _, c := range cases {
		ctx := connContext(c.l, c.classify)(context.Background(), fakeConn{local: tcp(c.local)})
		got := listenerFrom(ctx)
		if got.kind != c.want || got.via != c.via {
			t.Errorf("%v classify=%v local %s: %+v", c.l.kind, c.classify, c.local, got)
		}
	}
	if listenerFrom(context.Background()).kind != kindRefused {
		t.Fatal("a request from nowhere is trusted")
	}
}

func TestLoopbackAddr(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:7742":       "127.0.0.1:7742",
		"localhost:7742":       "localhost:7742",
		"[::1]:7742":           "[::1]:7742",
		"0.0.0.0:7742":         "127.0.0.1:7742",
		":7742":                "127.0.0.1:7742",
		"100.101.102.103:7742": "127.0.0.1:7742",
		"[::]:7742":            "127.0.0.1:7742",
	} {
		if got := LoopbackAddr(in); got != want {
			t.Errorf("LoopbackAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// Revoking a device ends what it has open, not only its next request.
func TestRevokeCutsAnOpenStream(t *testing.T) {
	e := newEnv(t)
	d, tok, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.s.Serve(ctx, ln, LoopbackOnly, "loopback") }()
	r, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/events", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("events: %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first frame %q %v", line, err)
	}
	if _, err := e.store.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		for {
			if _, err := br.ReadString('\n'); err != nil {
				close(ended)
				return
			}
		}
	}()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the revoked device's event stream stayed open")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func TestLoopbackListenerRefusesOtherConnections(t *testing.T) {
	e := newEnv(t)
	w := httptestRecorderFor(e, listener{kind: kindRefused}, "/status", bearer(master))
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("refused connection: %d", w.Code)
	}
}

func TestMountedRoutesFollowTheirExposure(t *testing.T) {
	e := newEnv(t)
	e.s.Mount("hello", Remote, func(mux *http.ServeMux, a Authz) {
		mux.HandleFunc("GET /hello", a.Require(devices.View, func(w http.ResponseWriter, r *http.Request) {
			p := PeerFrom(r.Context())
			_, _ = w.Write([]byte("hello " + p.Device.Name))
		}))
	})
	e.s.Mount("secret", LoopbackOnly, func(mux *http.ServeMux, a Authz) {
		mux.HandleFunc("GET /secret", a.Local(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("s")) }))
	})
	_, tok, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	if w := e.do(onRemote, req{path: "/hello", header: bearer(tok)}); w.Code != 200 || w.Body.String() != "hello Phone" {
		t.Fatalf("mounted remote route: %d %s", w.Code, w.Body)
	}
	if w := e.do(onRemote, req{path: "/secret", header: bearer(tok)}); w.Code != 404 {
		t.Fatalf("mounted local route on remote: %d", w.Code)
	}
	if w := e.do(onLoopback, req{path: "/secret", header: bearer(master)}); w.Code != 200 {
		t.Fatalf("mounted local route on loopback: %d", w.Code)
	}
}

func TestAdminRemoteIsOptIn(t *testing.T) {
	e := newEnv(t)
	e.s.AllowAdminRemote()
	_, admin, _ := e.store.Add("Laptop", devices.KindCLI, []devices.Scope{devices.View, devices.Admin}, "", "")
	_, phone, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	if w := e.do(onRemote, req{path: "/channels/list", header: bearer(admin)}); w.Code != 200 {
		t.Fatalf("admin device with admin_remote: %d", w.Code)
	}
	if w := e.do(onRemote, req{path: "/channels/list", header: bearer(phone)}); w.Code != 404 {
		t.Fatalf("non-admin device with admin_remote: %d", w.Code)
	}
	if w := e.do(onRemote, req{path: "/devices", header: bearer(admin)}); w.Code != 404 {
		t.Fatalf("device management is this computer's only: %d", w.Code)
	}
}

// With remote admin on, a settings page on another listener could start a
// Google sign-in that can never finish there (Google answers on 127.0.0.1,
// where the flow's cookie isn't): it is refused in words, before anyone is
// sent to Google.
func TestGoogleConnectOnlyOnThisComputer(t *testing.T) {
	e := newEnv(t)
	e.s.AllowAdminRemote()
	_, admin, _ := e.store.Add("Laptop", devices.KindCLI, []devices.Scope{devices.View, devices.Admin}, "", "")
	w := e.do(onRemote, req{method: "POST", path: "/accounts/google/connect", header: bearer(admin)})
	if w.Code != http.StatusConflict || e.f.redirect != "" || !strings.Contains(w.Body.String(), "computer") || cookieFrom(w, oauthCookie) != nil {
		t.Fatalf("connect from a remote page: %d %q redirect %q", w.Code, w.Body.String(), e.f.redirect)
	}
	if w := e.do(onLoopback, req{method: "POST", path: "/accounts/google/connect", header: bearer(master)}); w.Code != 200 || e.f.redirect == "" {
		t.Fatalf("connect on this computer: %d", w.Code)
	}
}

func TestMasterKeyComparisonIgnoresLookalikes(t *testing.T) {
	e := newEnv(t)
	lo := listener{kind: kindLoopback}
	for _, tok := range []string{"", master[:47], master + "0", strings.ToUpper(master), " " + master + "x"} {
		if e.s.isMaster(tok, lo) {
			t.Errorf("isMaster(%q)", tok)
		}
	}
	if !e.s.isMaster(master, lo) || !e.s.isMaster(master, listener{kind: kindLegacy}) {
		t.Fatal("the master key itself")
	}
}

// A refused device on this computer is told where to pair (the "Add your
// phone" page); one elsewhere gets the command in the fix instead.
func TestNotPairedSaysWhereToPair(t *testing.T) {
	e, _ := pagesEnv(t)
	w := e.do(onLoopback, req{path: "/screen"})
	var body apiError
	if w.Code != 401 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.PairURL != "/devices/add" || !strings.Contains(w.Body.String(), `"pair_url":"/devices/add"`) {
		t.Fatalf("loopback: %d %s", w.Code, w.Body)
	}
	page := e.do(onLoopback, req{path: "/memory", header: map[string]string{"Accept": "text/html"}})
	if page.Code != 401 || !strings.Contains(page.Body.String(), `<a href="/devices/add"`) {
		t.Fatalf("loopback page: %d %s", page.Code, page.Body)
	}
	body = apiError{}
	w = e.do(onRemote, req{path: "/screen"})
	if w.Code != 401 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.PairURL != "" || !strings.Contains(body.Fix, "`mirrin pair --screen`") {
		t.Fatalf("remote: %d %s", w.Code, w.Body)
	}
}

// The settings pages carry the twin's name, so a page that can't load its
// data yet (not paired) never shows another name.
func TestSettingsPagesCarryTheTwinsName(t *testing.T) {
	e := newEnv(t)
	e.s.WithName("Ada")
	for _, path := range []string{"/memory", "/channels", "/accounts", "/protocols", "/health", "/ui"} {
		w := e.do(onLoopback, req{path: path, header: map[string]string{"Accept": "text/html", "Authorization": "Bearer " + master}})
		if w.Code != 200 || strings.Count(w.Body.String(), `<meta name="mirrin-name" content="Ada">`) != 1 {
			t.Errorf("%s: %d, name meta tags: %d", path, w.Code, strings.Count(w.Body.String(), `<meta name="mirrin-name"`))
		}
	}
}
