package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// The real browser, driven the way the model drives it, stays off this
// computer and the local network: the address itself, a redirect, an https
// redirect (a tunnel) and an image on a public page are all refused, and the
// model is told why in words.
func TestBrowserStaysOffTheLocalNetwork(t *testing.T) {
	needChrome(t)
	var hits counter
	var tlsPort string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.hit(r.URL.Path)
		port := r.Host[strings.LastIndex(r.Host, ":")+1:]
		switch r.URL.Path {
		case "/hop":
			http.Redirect(w, r, "http://127.0.0.1:"+port+"/admin", http.StatusFound)
		case "/tlshop":
			http.Redirect(w, r, "https://127.0.0.1:"+tlsPort+"/admin", http.StatusFound)
		case "/page":
			_, _ = io.WriteString(w, `<html><body><h1>A public page</h1><img src="http://127.0.0.1:`+port+`/pixel.png"></body></html>`)
		default:
			_, _ = io.WriteString(w, "<html><body>admin panel</body></html>")
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	secure := httptest.NewTLSServer(h)
	defer secure.Close()
	tlsPort = secure.URL[strings.LastIndex(secure.URL, ":")+1:]
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	local := "http://localhost:" + port

	s := newTestSession(t) // nothing on this machine allowed
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	refused := func(name string, in any, says ...string) {
		t.Helper()
		out, err := run(t, reg, name, in)
		if err == nil {
			t.Fatalf("%s %v went through:\n%s", name, in, out)
		}
		for _, want := range append(says, "allow_hosts") {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s %v: %q should say %q", name, in, err, want)
			}
		}
	}
	refused("browse_page", map[string]string{"url": srv.URL + "/admin"}, "on this computer or a private network")
	refused("browse_page", map[string]string{"url": local + "/admin"}, "localhost")
	refused("browse_page", map[string]string{"url": "http://169.254.169.254/latest/meta-data/"})
	refused("screenshot_page", map[string]string{"url": "http://[::1]:" + port + "/admin"})
	refused("browser_inspect", map[string]string{"url": "http://192.168.1.1/"})
	refused("browser_act", map[string]string{"url": "http://10.0.0.1/", "steps": `[]`})
	refused("browser_signin", map[string]string{"url": srv.URL + "/admin"})
	if _, err := run(t, reg, "browse_page", map[string]string{"url": "file:///etc/hosts"}); err == nil || !strings.Contains(err.Error(), "only open web pages") {
		t.Fatalf("file address: %v", err)
	}
	if err := s.StartTeaching(srv.URL+"/admin", "x"); err == nil {
		t.Fatal("teach opened a local address")
	}

	// The owner allows the name "localhost"; where it sends the browser next
	// is checked again.
	s.AllowHosts("localhost")
	if out, err := run(t, reg, "browse_page", map[string]string{"url": local + "/admin"}); err != nil || !strings.Contains(out, "admin panel") {
		t.Fatalf("allowed host: %v\n%s", err, out)
	}
	refused("browse_page", map[string]string{"url": local + "/hop"}, "localhost sent the browser on to 127.0.0.1")
	refused("browse_page", map[string]string{"url": local + "/tlshop"}, "127.0.0.1")
	out, err := run(t, reg, "browse_page", map[string]string{"url": local + "/page"})
	if err != nil || !strings.Contains(out, "A public page") || !strings.Contains(out, "blocked: 127.0.0.1") {
		t.Fatalf("page with a private image: %v\n%s", err, out)
	}
	if n := hits.n("/admin") + hits.n("/pixel.png"); n != 1 { // the one allowed visit
		t.Fatalf("private pages were reached %d times", n)
	}
}

// Headless Chrome presents its own user agent without "HeadlessChrome": the
// real version and platform, matching the client hints it sends. (It used to
// claim to be Chrome 129 on a Mac, everywhere.)
func TestHeadlessBrowserIntroducesItselfHonestly(t *testing.T) {
	needChrome(t)
	var mu sync.Mutex
	var ua, hints string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ua, hints = r.Header.Get("User-Agent"), r.Header.Get("Sec-CH-UA")
		mu.Unlock()
		_, _ = io.WriteString(w, "<html><body>hello</body></html>")
	}))
	defer srv.Close()
	s := newTestSession(t, "127.0.0.1")
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	if _, err := run(t, reg, "browse_page", map[string]string{"url": srv.URL}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	product := s.ua.product // "Chrome/153.0.8010.53", from the browser itself
	s.mu.Unlock()
	major := strings.SplitN(strings.TrimPrefix(product, "Chrome/"), ".", 2)[0]
	mu.Lock()
	defer mu.Unlock()
	if major == "" || strings.Contains(ua, "Headless") || !strings.Contains(ua, "Chrome/"+major+".") {
		t.Fatalf("user agent %q for %s", ua, product)
	}
	platform := map[string]string{"darwin": "Macintosh", "linux": "Linux", "windows": "Windows"}[runtime.GOOS]
	if platform != "" && !strings.Contains(ua, platform) {
		t.Fatalf("user agent %q doesn't name this platform (%s)", ua, platform)
	}
	if hints == "" || strings.Contains(hints, "Headless") || !strings.Contains(hints, major) {
		t.Fatalf("client hints %q don't match %s", hints, product)
	}
	var page string
	ctx, _ := s.tab(false)
	if err := chromedp.Run(ctx, chromedp.Evaluate(`navigator.userAgent`, &page)); err != nil || page != ua {
		t.Fatalf("page sees %q, server saw %q (%v)", page, ua, err)
	}
	// What the server saw is what was learned from Chrome itself.
	if got := s.knownUA(); got != ua {
		t.Fatalf("learned %q, sent %q", got, ua)
	}
}

// An approved click runs only on the page the owner was asked about: if the
// element it aims at now says something else, has gone, or the browser has
// moved on or closed, the yes is refused before anything runs (and again just
// before the click), and the model is told why. A call nobody said yes to is
// never held to an old question.
func TestApprovedActionChecksThePageFirst(t *testing.T) {
	needChrome(t)
	var hits counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.hit(r.URL.Path)
		if r.URL.Path == "/other" {
			_, _ = io.WriteString(w, "<html><body><button>Somewhere else</button></body></html>")
			return
		}
		_, _ = io.WriteString(w, `<html><body><button id="a" onclick="fetch('/clicked')">Save draft</button><button id="b">Cancel</button></body></html>`)
	}))
	defer srv.Close()
	s := newTestSession(t, "127.0.0.1")
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	tool, _ := reg.Get("browser_act")
	act := tool.(interface {
		tools.Tool
		tools.Checker
		tools.CallRisker
	})
	ctx := context.Background()
	call := tools.Call{ChatKey: "whatsapp:owner", Input: json.RawMessage(`{"steps":"[{\"type\":\"click\",\"ref\":1}]"}`)}
	approve := func() error { return s.CheckApproved(ctx, call.ChatKey, call.Input) }

	// Nothing open yet: nobody is asked about a click that can't happen.
	if err := act.Check(ctx, call); err == nil || !strings.Contains(err.Error(), "browse_page first") {
		t.Fatalf("no page: %v", err)
	}
	inspect := func() {
		t.Helper()
		if _, err := run(t, reg, "browser_inspect", map[string]string{"url": srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	tab := func() context.Context { c, _ := s.tab(false); return c }
	eval := func(js string) {
		t.Helper()
		if err := chromedp.Run(tab(), chromedp.Evaluate(js, nil)); err != nil {
			t.Fatal(err)
		}
	}
	clicks := 0
	refused := func(err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "nothing was clicked") {
			t.Fatalf("want %q, got %v", want, err)
		}
		if n := hits.n("/clicked"); n != clicks {
			t.Fatalf("clicked %d times, want %d", n, clicks)
		}
	}
	asked := func() {
		t.Helper()
		inspect()
		if err := act.Check(ctx, call); err != nil {
			t.Fatal(err)
		}
	}

	asked()
	eval(`document.querySelector('[data-oh-ref="1"]').innerText = 'Delete account'`)
	refused(approve(), `now reads "Delete account"`)

	asked()
	eval(`document.querySelector('#a').remove()`)
	refused(approve(), `"Save draft" (element 1) is no longer there`)

	asked()
	if _, err := run(t, reg, "browse_page", map[string]string{"url": srv.URL + "/other"}); err != nil {
		t.Fatal(err)
	}
	refused(approve(), "moved on")

	// Unchanged, the approved click goes ahead, once.
	asked()
	if err := approve(); err != nil {
		t.Fatal(err)
	}
	if _, err := act.Run(ctx, call); err != nil {
		t.Fatal(err)
	}
	clicks++
	if hits.n("/clicked") != clicks {
		t.Fatal("the approved click didn't happen")
	}

	// Changed between the yes and the click: the tool looks again.
	asked()
	if err := approve(); err != nil {
		t.Fatal(err)
	}
	eval(`document.querySelector('[data-oh-ref="1"]').innerText = 'Delete account'`)
	_, err := act.Run(ctx, call)
	refused(err, `now reads "Delete account"`)

	// The owner said no (the tool never hears of it); the same call later
	// allowed without asking isn't held to that question.
	asked()
	eval(`document.querySelector('[data-oh-ref="1"]').innerText = 'Save as draft'`)
	if _, err := act.Run(ctx, call); err != nil {
		t.Fatalf("an unapproved question held back a later call: %v", err)
	}
	clicks++

	// Closed in between: nothing starts a fresh browser to click in.
	asked()
	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()
	refused(approve(), "closed")
	s.mu.Lock()
	started := s.ctx != nil
	s.mu.Unlock()
	if started {
		t.Fatal("an approved click started a new browser")
	}
}

// A Chrome that uses some other proxy (a company policy or an extension
// outranks the flag Mirrin starts it with) is stopped, with the reason on the
// Health page, rather than left free to reach the local network.
func TestBrowserRefusesAChromeOnAnotherProxy(t *testing.T) {
	needChrome(t)
	var hits counter
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.hit(r.Host)
		_, _ = io.WriteString(w, "the company proxy")
	}))
	defer other.Close()
	oldAdjust, oldWait := adjustFlags, canaryWait
	t.Cleanup(func() { adjustFlags, canaryWait = oldAdjust, oldWait; noteLaunch(nil) })
	adjustFlags = func(f map[string]any) { f["proxy-server"] = other.URL }
	canaryWait = time.Second

	s := newTestSession(t)
	_, err := s.tab(false)
	if err == nil || !strings.Contains(err.Error(), "another proxy") || !strings.Contains(err.Error(), "chrome-profile") {
		t.Fatalf("Chrome on another proxy: %v", err)
	}
	s.mu.Lock()
	running := s.ctx != nil
	s.mu.Unlock()
	if running {
		t.Fatal("that Chrome was kept")
	}
	if r := Health().Run(context.Background()); r.State != health.Fail || !strings.Contains(r.Detail, "another proxy") {
		t.Fatalf("health: %+v", r)
	}
	seen := false
	hits.mu.Lock()
	for host := range hits.hits {
		seen = seen || strings.HasSuffix(strings.Split(host, ":")[0], canaryDomain)
	}
	hits.mu.Unlock()
	if !seen {
		t.Fatal("the stand-in proxy never saw the check, so the test proves nothing")
	}

	// Back on the guard, it starts.
	adjustFlags, canaryWait = oldAdjust, oldWait
	if _, err := s.tab(false); err != nil {
		t.Fatal(err)
	}
}

// A start hands out the tab only once Chrome has taken the guard's answer to
// the check. An answer that arrived while the first page was starting to load
// used to cancel that page (net::ERR_ABORTED), now and then, on a busy
// machine.
func TestStartWaitsForTheCheckToBeAnswered(t *testing.T) {
	needChrome(t)
	var heard, answered atomic.Int32
	t.Cleanup(func() { canaryAnswering = nil })
	canaryAnswering = func() {
		heard.Add(1)
		time.Sleep(300 * time.Millisecond) // the answer is slow to arrive
		answered.Add(1)
	}
	s := newTestSession(t)
	if _, err := s.tab(false); err != nil {
		t.Fatal(err)
	}
	if h, a := heard.Load(), answered.Load(); h == 0 || a != h {
		t.Fatalf("the tab was handed out with %d of %d checks answered", a, h)
	}
}

// A page that won't load says why in words, not in Go's.
func TestPagesThatWontLoadSayWhy(t *testing.T) {
	needChrome(t)
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	port := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	s := newTestSession(t, "127.0.0.1")
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	for _, u := range []string{fmt.Sprintf("http://127.0.0.1:%d/", port), fmt.Sprintf("https://127.0.0.1:%d/", port)} {
		out, err := run(t, reg, "browse_page", map[string]string{"url": u})
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1 refused the connection. The site may be down") {
			t.Errorf("%s: %v\n%s", u, err, out)
		}
	}
	if _, err := run(t, reg, "browse_page", map[string]string{"url": "https://nosuch.test/"}); err == nil || err.Error() != "I couldn't find nosuch.test. Check the address" {
		t.Errorf("a name that doesn't exist: %v", err)
	}
}
