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

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// TestPairPageInABrowser opens a pairing link in a real (headless) Chrome:
// the secret leaves the address bar, pairing sets the browser's cookie and
// lands on the screen, and the spent link says so plainly.
func TestPairPageInABrowser(t *testing.T) {
	if os.Getenv("CI") != "" && os.Getenv("MIRRIN_BROWSER_TESTS") == "" {
		t.Skip("browser test needs a sandbox-capable Chrome; set MIRRIN_BROWSER_TESTS=1 to run in CI")
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
		l := listener{kind: kindLegacy, via: "lan"}
		e.s.Handler().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), listenerKey, l)))
	}))
	defer ts.Close()
	o := e.offer("pwa")
	link := PairLink(ts.URL, o.Offer, "Mirrin")

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
	ctx, cancelT := context.WithTimeout(ctx, 40*time.Second)
	defer cancelT()

	var hash, title, lead string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(link),
		chromedp.WaitVisible("#name", chromedp.ByID),
		chromedp.Evaluate(`location.hash`, &hash),
		chromedp.Title(&title),
		chromedp.Text("#lead", &lead, chromedp.ByID),
	); err != nil {
		t.Fatal(err)
	}
	if hash != "" || title != "Pair with Mirrin" || !strings.Contains(lead, "approve") {
		t.Fatalf("hash %q title %q lead %q", hash, title, lead)
	}
	if err := chromedp.Run(ctx,
		chromedp.SetValue("#name", "Test phone", chromedp.ByID),
		chromedp.Click("#go", chromedp.ByID),
	); err != nil {
		t.Fatalf("pairing in the browser: %v", err)
	}
	// The page moves on to the screen by itself; evaluating across that
	// navigation can fail, so ask until it has happened.
	for deadline := time.Now().Add(10 * time.Second); ; {
		var onScreen bool
		if chromedp.Run(ctx, chromedp.Evaluate(`location.pathname === '/ui' && document.readyState === 'complete'`, &onScreen)) == nil && onScreen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pairing never reached the screen")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var paired *devices.Device
	for _, d := range e.store.List() {
		if d.Name == "Test phone" {
			paired = &d
		}
	}
	if paired == nil || paired.Kind != devices.KindPWA {
		t.Fatalf("devices %+v", e.store.List())
	}
	// The screen loaded with the browser's new cookie (not the 401 page).
	var body string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerText.includes("isn't paired") ? "unpaired" : "ok"`, &body)); err != nil || body != "ok" {
		t.Fatalf("screen after pairing: %q %v", body, err)
	}
	// The same link again explains itself.
	var msg string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(link),
		chromedp.WaitVisible("#go", chromedp.ByID),
		chromedp.Click("#go", chromedp.ByID),
		chromedp.Poll(`document.getElementById('msg').className.includes('err')`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Text("#msg", &msg, chromedp.ByID),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "already used") || !strings.Contains(msg, "mirrin pair") {
		t.Fatalf("spent link: %q", msg)
	}
	// A link with no secret says what to do.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(ts.URL+"/pair"),
		chromedp.WaitVisible("#lead", chromedp.ByID),
		chromedp.Text("#lead", &msg, chromedp.ByID),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "incomplete") || !strings.Contains(msg, "mirrin pair --screen") {
		t.Fatalf("bare /pair: %q", msg)
	}
}
