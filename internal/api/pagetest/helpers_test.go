package pagetest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// One headless Chrome serves every browser test; each test gets its own tab.
var (
	chromeOnce sync.Once
	chromeCtx  context.Context
	chromeErr  error
	chromeStop func()
)

func TestMain(m *testing.M) {
	code := m.Run()
	if chromeStop != nil {
		chromeStop()
	}
	os.Exit(code)
}

func chromePath() string {
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// tab opens a fresh tab, skipping the test when no browser can run here.
func tab(t *testing.T) context.Context {
	t.Helper()
	if os.Getenv("CI") != "" && os.Getenv("MIRRIN_BROWSER_TESTS") == "" {
		t.Skip("browser test; set MIRRIN_BROWSER_TESTS=1 to run it in CI")
	}
	path := chromePath()
	if path == "" {
		t.Skip("no Chrome available")
	}
	chromeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mirrin-pagetest-")
		if err != nil {
			chromeErr = err
			return
		}
		// chromedp's defaults keep Chrome from calling Google in the
		// background (sync, updates, safe browsing, metrics); tests talk only
		// to the loopback fake daemon.
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.ExecPath(path),
			chromedp.DisableGPU,
			chromedp.UserDataDir(dir),
			chromedp.Flag("use-mock-keychain", true),
			chromedp.Flag("password-store", "basic"),
			chromedp.Flag("disable-component-update", true),
			chromedp.Flag("disable-domain-reliability", true),
			chromedp.Flag("mute-audio", true),
			chromedp.Flag("lang", "en-US"),
			// another device: screen.test is the fake daemon too, by a
			// name that isn't this computer's (remoteURL)
			chromedp.Flag("host-resolver-rules", "MAP *.test 127.0.0.1"),
		)
		actx, acancel := chromedp.NewExecAllocator(context.Background(), opts...)
		bctx, bcancel := chromedp.NewContext(actx)
		if err := chromedp.Run(bctx); err != nil {
			chromeErr = err
			bcancel()
			acancel()
			_ = os.RemoveAll(dir)
			return
		}
		chromeCtx = bctx
		chromeStop = func() { bcancel(); acancel(); _ = os.RemoveAll(dir) }
	})
	if chromeErr != nil {
		t.Skipf("Chrome did not start: %v", chromeErr)
	}
	ctx, cancel := chromedp.NewContext(chromeCtx)
	ctx, tcancel := context.WithTimeout(ctx, 60*time.Second)
	t.Cleanup(func() { tcancel(); cancel() })
	// In front, so the page is visible and its animations and timers run as they would for a person.
	if err := chromedp.Run(ctx, page.BringToFront()); err != nil {
		t.Fatal(err)
	}
	return ctx
}

// pageFiles maps each page's path to the file the API embeds for it.
var pageFiles = map[string]string{
	"/ui":        "../ui.html",
	"/health":    "../health.html",
	"/memory":    "../memory.html",
	"/channels":  "../channels.html",
	"/accounts":  "../accounts.html",
	"/protocols": "../protocols.html",
}

// localPageFiles are the device and safety pages (local_pages_test.go):
// settings pages too, with the same helper, served the same way.
var localPageFiles = map[string]string{
	"/devices/add":    "../devices_add.html",
	"/devices/page":   "../devices.html",
	"/backup":         "../backup.html",
	"/trust":          "../trust.html",
	"/spending":       "../spending.html",
	"/restore/review": "../restore_review.html",
	"/reach":          "../reach.html",
	"/voice/setup":    "../voice_setup.html",
}

func readPage(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// daemon is a stand-in for the API: it serves the real pages and whatever
// JSON a test sets, and records every request.
type daemon struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	calls    []string
	feed     chan string
	done     chan struct{}
}

func newDaemon(t *testing.T) *daemon {
	t.Helper()
	d := &daemon{t: t, handlers: map[string]http.HandlerFunc{}, feed: make(chan string, 16), done: make(chan struct{})}
	all := map[string]string{}
	for path, file := range pageFiles {
		all[path] = file
	}
	for path, file := range localPageFiles {
		all[path] = file
	}
	for path, file := range all {
		html := []byte(readPage(t, file))
		d.handlers["GET "+path] = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(html)
		}
	}
	// the drawings the screen and the welcome page load (internal/api/characters.js)
	characters := []byte(readPage(t, "../characters.js"))
	d.handlers["GET /characters.js"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(characters)
	}
	d.handlers["GET /events"] = d.events
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(func() {
		close(d.done)
		d.srv.CloseClientConnections()
		d.srv.Close()
	})
	return d
}

func (d *daemon) url(path string) string { return d.srv.URL + path }

// remoteURL is the page as another device sees it: the same fake daemon, by
// a name that isn't this computer's, so the page knows it isn't on the
// computer the twin runs on (ON_THIS_COMPUTER is false).
func (d *daemon) remoteURL(path string) string {
	return "http://screen.test:" + strconv.Itoa(d.srv.Listener.Addr().(*net.TCPAddr).Port) + path
}

func (d *daemon) serve(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	// /health is both the page and its JSON, told apart by Accept, as in api.go.
	if key == "GET /health" && !strings.Contains(r.Header.Get("Accept"), "text/html") {
		key = "GET /health.json"
	}
	d.mu.Lock()
	d.calls = append(d.calls, key)
	h := d.handlers[key]
	d.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

func (d *daemon) handle(key string, h http.HandlerFunc) {
	d.mu.Lock()
	d.handlers[key] = h
	d.mu.Unlock()
}

func (d *daemon) count(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if c == key {
			n++
		}
	}
	return n
}

// waitCalled waits for the page to have made a request.
func (d *daemon) waitCalled(key string, n int) {
	d.t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if d.count(key) >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Fatalf("the page never sent %s (%d times wanted, %d seen)", key, n, d.count(key))
}

// push sends one event down every open /events stream.
func (d *daemon) push(ev map[string]any) {
	b, _ := json.Marshal(ev)
	d.feed <- string(b)
}

func (d *daemon) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fmt.Fprint(w, "data: {\"kind\":\"state\",\"text\":\"idle\"}\n\n")
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-d.done:
			return
		case ev := <-d.feed:
			fmt.Fprintf(w, "data: %s\n\n", ev)
			fl.Flush()
		}
	}
}

func jsonH(v any) http.HandlerFunc {
	b, _ := json.Marshal(v)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
}

func textH(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { http.Error(w, body, code) }
}

// notPairedH is the answer the API gives a device it doesn't know
// (internal/api/auth.go notPaired): a code, a sentence, and the next step.
func notPairedH(fix string) http.HandlerFunc {
	return refusedH(http.StatusUnauthorized, "not_paired", "This device isn't paired with Mirrin.", fix)
}

// refusedH is any refusal from the API (internal/api/auth.go apiError).
func refusedH(status int, code, message, fix string) http.HandlerFunc {
	b, _ := json.Marshal(map[string]string{"error": code, "message": message, "fix": fix})
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	}
}

// screen is a /screen answer; mut changes it for one test.
func screen(mut func(m map[string]any)) map[string]any {
	m := map[string]any{
		"name": "Mirrin", "state": "idle", "time": time.Now().Format(time.RFC3339), "health": "all good",
		"events": []any{}, "reminders": []any{}, "notices": []any{}, "approvals": []any{}, "tasks": []any{},
		"recent": []any{}, "ambient_after_seconds": 3600, "persona": "mirrin",
	}
	if mut != nil {
		mut(m)
	}
	return m
}

func run(t *testing.T, ctx context.Context, actions ...chromedp.Action) {
	t.Helper()
	if err := chromedp.Run(ctx, actions...); err != nil {
		t.Fatal(err)
	}
}

func phone() chromedp.Action {
	return chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(2), chromedp.EmulateMobile, chromedp.EmulateTouch)
}

func desktop() chromedp.Action { return chromedp.EmulateViewport(1280, 800) }

func utc() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if err := emulation.SetTimezoneOverride("UTC").Do(ctx); err != nil {
			return err
		}
		return emulation.SetLocaleOverride().WithLocale("en-US").Do(ctx)
	})
}

func reducedMotion() chromedp.Action {
	return emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-reduced-motion", Value: "reduce"}})
}

// eval runs an expression in the page and decodes its value.
func eval[T any](t *testing.T, ctx context.Context, expr string) T {
	t.Helper()
	var v T
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &v)); err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return v
}

// waitFor polls a page expression until it is truthy.
func waitFor(t *testing.T, ctx context.Context, expr, what string) {
	t.Helper()
	var ok bool
	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := chromedp.Run(pctx, chromedp.Poll(expr, &ok, chromedp.WithPollingTimeout(7*time.Second), chromedp.WithPollingInterval(50*time.Millisecond))); err != nil || !ok {
		var body string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`document.body ? document.body.innerText : ''`, &body))
		t.Fatalf("%s: never happened (%v)\npage text:\n%s", what, err, body)
	}
}

// visible is a page expression that is true when a selector is on screen.
func visible(sel string) string {
	return fmt.Sprintf(`(() => { const e = document.querySelector(%q); if (!e) return false; const r = e.getBoundingClientRect(); const cs = getComputedStyle(e); return r.width > 0 && r.height > 0 && cs.visibility !== 'hidden' && !e.closest('[hidden]'); })()`, sel)
}

// textOf is an element's text as written, before CSS upper-cases any of it.
func textOf(t *testing.T, ctx context.Context, sel string) string {
	t.Helper()
	return eval[string](t, ctx, fmt.Sprintf(`(() => { const e = document.querySelector(%q); return e ? e.textContent : ''; })()`, sel))
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
