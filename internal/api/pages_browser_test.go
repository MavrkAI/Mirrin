package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The page tests (pagetest) run the pages against a stand-in daemon; this
// one runs the reworked settings pages in a real browser against the real
// server's checks (devices' auth: the menu link swapping the master key for
// the browser's own cookie, the page CSP, and Sec-Fetch-Site/Origin on every
// change), so a page change that trips them fails here, not for the owner.
func TestSettingsPagesWorkThroughTheRealChecks(t *testing.T) {
	if os.Getenv("CI") != "" && os.Getenv("MIRRIN_BROWSER_TESTS") == "" {
		t.Skip("browser test; set MIRRIN_BROWSER_TESTS=1 to run it in CI")
	}
	chrome := ""
	for _, p := range []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "google-chrome", "chromium"} {
		if path, err := exec.LookPath(p); err == nil {
			chrome = path
			break
		}
	}
	if chrome == "" {
		t.Skip("no Chrome available")
	}
	e := newEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := listener{kind: kindLoopback, via: "loopback"}
		e.s.Handler().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), listenerKey, l)))
	}))
	defer ts.Close()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome),
		chromedp.UserDataDir(chromeProfile(t)),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
	)
	actx, cancelA := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelA()
	ctx, cancel := chromedp.NewContext(actx)
	defer cancel()
	ctx, cancelT := context.WithTimeout(ctx, 60*time.Second)
	defer cancelT()

	// The menu opens a page with the master key; the page carries on with
	// the browser's own key and never shows the master one.
	var path string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(ts.URL+"/memory?token="+master),
		chromedp.WaitVisible("#addContent", chromedp.ByID),
		chromedp.Evaluate(`location.pathname + location.search`, &path),
	); err != nil {
		t.Fatal(err)
	}
	if path != "/memory" {
		t.Fatalf("the master key stayed in the address: %q", path)
	}
	// A change from the page passes the same-origin checks.
	var msg string
	if err := chromedp.Run(ctx,
		chromedp.SetValue("#addContent", "Likes flat whites.", chromedp.ByID),
		chromedp.Evaluate(`document.getElementById('add').requestSubmit(); true`, nil),
		chromedp.Poll(`document.getElementById('addMsg').textContent.length > 0`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Text("#addMsg", &msg, chromedp.ByID),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "Remembered") || !e.f.saw("addfact") {
		t.Fatalf("adding a fact from the page: %q (reached the twin: %v)", msg, e.f.saw("addfact"))
	}
	// Every settings page opens with that cookie, and its script runs
	// under the page's policy (it fills the page in).
	for _, c := range []struct{ path, ready string }{
		{"/health", `document.querySelector('#run') && !document.querySelector('#gate:not([hidden])')`},
		{"/channels", `document.readyState === 'complete' && !document.body.innerText.includes("isn't paired")`},
		{"/accounts", `document.readyState === 'complete' && !document.body.innerText.includes("isn't paired")`},
		{"/protocols", `document.readyState === 'complete' && !document.body.innerText.includes("isn't paired")`},
	} {
		if err := chromedp.Run(ctx,
			chromedp.Navigate(ts.URL+c.path),
			chromedp.Poll(c.ready, nil, chromedp.WithPollingTimeout(10*time.Second)),
		); err != nil {
			var body string
			_ = chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerText`, &body))
			t.Fatalf("%s: %v\n%s", c.path, err, body)
		}
	}
	// Health's "Check again now" posts through the same checks.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(ts.URL+"/health"),
		chromedp.WaitVisible("#run", chromedp.ByID),
		chromedp.Click("#run", chromedp.ByID),
		chromedp.Poll(`document.getElementById('runMsg').textContent.includes('Checked just now')`, nil, chromedp.WithPollingTimeout(10*time.Second)),
	); err != nil {
		var m string
		_ = chromedp.Run(ctx, chromedp.Text("#runMsg", &m, chromedp.ByID))
		t.Fatalf("Check again now: %v (%q)", err, m)
	}
}
