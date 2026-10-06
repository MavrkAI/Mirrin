package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

var (
	_ tools.Checker    = checked{}
	_ tools.CallRisker = checked{}
)

func TestSecretFields(t *testing.T) {
	for _, c := range []struct {
		f      field
		value  string
		secret bool
	}{
		{field{Type: "password"}, "hunter2", true},
		{field{Autocomplete: "cc-number"}, "4111", true},
		{field{Autocomplete: "section-pay billing cc-csc"}, "123", true},
		{field{Autocomplete: "one-time-code"}, "918273", true},
		{field{Autocomplete: "new-password"}, "x", true},
		{field{Name: "cardNumber"}, "1234", true},
		{field{Name: "card_number"}, "1234", true},
		{field{ID: "ccNum"}, "1234", true},
		{field{Name: "cvc"}, "123", true},
		{field{Placeholder: "Security code"}, "123", true},
		{field{Label: "Verification code"}, "123456", true},
		{field{Label: "Passport number"}, "X1234567", true},
		{field{Label: "Social Security Number"}, "1", true},
		{field{Aria: "IBAN"}, "x", true},
		{field{Name: "otp"}, "1", true},
		{field{Label: "Driver's licence number"}, "x", true},
		{field{Label: "Expiry date"}, "07/29", true},
		// A number that is a card, an SSN or an IBAN, wherever it is typed.
		{field{Name: "note"}, "4242 4242 4242 4242", true},
		{field{Name: "q"}, "078-05-1120", true},
		{field{Name: "memo"}, "GB82 WEST 1234 5698 7654 32", true},
		{field{Type: "text", InputMode: "numeric", Name: "ref"}, "123456789012345", true},
		// One-time codes, however the field is named.
		{field{Name: "code", Label: "Enter the 6-digit code we sent to your phone"}, "123456", true},
		{field{Name: "code", Placeholder: "123456"}, "123456", true},
		{field{ID: "verify", Label: "Code"}, "123456", true},
		{field{Name: "code"}, "123 456", true},
		{field{Label: "We texted you a code"}, "ABCD", true},
		{field{Label: "Code from your authenticator app"}, "x", true},
		{field{Label: "Two-factor code"}, "x", true},
		{field{Label: "2-step verification"}, "x", true},
		{field{Name: "smscode"}, "x", true},
		{field{Label: "Sign-in code"}, "x", true},
		{field{Label: "Confirmation code"}, "x", true},
		{field{Name: "token"}, "x", true},
		// Card expiry and security numbers by other names.
		{field{Name: "exp_month"}, "07", true},
		{field{Name: "expMonth"}, "07", true},
		{field{Name: "card_exp_year"}, "2029", true},
		{field{ID: "expirationYear"}, "2029", true},
		{field{Name: "sortcode"}, "12-34-56", true},
		{field{Label: "Security number"}, "123", true},
		// Everyday fields stay readable.
		{field{Name: "q", Placeholder: "Search"}, "hello", false},
		{field{Name: "shipping", Label: "Shipping address"}, "1 Main St", false},
		{field{Name: "zip", Label: "Postcode"}, "SW1A 1AA", false},
		{field{Name: "promo", Label: "Promo code"}, "SUMMER", false},
		{field{Name: "promo", Label: "Promo code"}, "2024", false},
		{field{Name: "postcode", Label: "Postcode"}, "2000", false},
		{field{Name: "zip", Label: "ZIP code"}, "90210", false},
		{field{Label: "Postal code"}, "12345", false},
		{field{Label: "Area code"}, "4155", false},
		{field{Label: "Gift card code"}, "ABCD-EFGH", false},
		{field{Name: "code", Label: "Discount code"}, "5000", false},
		{field{Label: "Tracking code"}, "12345678", false},
		{field{Name: "code", Label: "Code"}, "SUMMER24", false}, // not only digits
		{field{Type: "tel", Name: "phone"}, "+44 20 7946 0958", false},
		{field{Type: "tel", Name: "phone"}, "004420794609581", false},
		{field{Name: "company", Label: "Company"}, "Spinning Pandas", false},
		{field{Name: "order"}, "4242 4242 4242 4241", false}, // fails the card checksum
		{field{InputMode: "numeric", Name: "qty"}, "12", false},
	} {
		if got := secretField(c.f, c.value); got != c.secret {
			t.Errorf("secretField(%+v, %q) = %v, want %v", c.f, c.value, got, c.secret)
		}
	}
}

func TestScrubbing(t *testing.T) {
	for in, want := range map[string]string{
		"https://bank.example/cb?code=abc123&state=xyz&lang=en":         "https://bank.example/cb?code=[secret]&state=[secret]&lang=en",
		"https://app.example/#access_token=eyJ.a.b&token_type=bearer":   "https://app.example/#access_token=[secret]&token_type=bearer",
		"https://shop.example/pay?card=4111111111111111":                "https://shop.example/pay?card=[card number]",
		"https://example.com/a?page=2#top":                              "https://example.com/a?page=2#top",
		"https://s3.example/x?X-Amz-Signature=deadbeef&X-Amz-Expires=9": "https://s3.example/x?X-Amz-Signature=[secret]&X-Amz-Expires=9",
		// Sign-in parameters by any name, and codes by the family name.
		"https://x.example/cb?oauth_token=abc&oauth_verifier=def&page=2":                               "https://x.example/cb?oauth_token=[secret]&oauth_verifier=[secret]&page=2",
		"https://x.example/l?login_token=abc&access-token=def":                                         "https://x.example/l?login_token=[secret]&access-token=[secret]",
		"https://x.example/v?auth_code=123&otpCode=456&promo_code=SUMMER&zipcode=90210&countryCode=GB": "https://x.example/v?auth_code=[secret]&otpCode=[secret]&promo_code=SUMMER&zipcode=90210&countryCode=GB",
		"https://x.example/a?PHPSESSID=abc&client_secret=def":                                          "https://x.example/a?PHPSESSID=[secret]&client_secret=[secret]",
		// A user name and password in the address.
		"https://me:hunter2@router.example/admin": "https://router.example/admin",
		// Tokens in the path of a sign-in link; a document's ID stays.
		"https://x.example/reset-password/Xk3j9Qp2Lm7Rt5Vw8Yz1Ab4Cd":                                             "https://x.example/reset-password/[secret]",
		"https://x.example/auth/magic/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08?next=%2F": "https://x.example/auth/magic/[secret]?next=%2F",
		"https://x.example/s/eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig":                                           "https://x.example/s/[secret]",
		"https://docs.example/document/d/1a2B3c4D5e6F7g8H9i0JkLmNoPqRsTuVwXyZ/edit":                              "https://docs.example/document/d/1a2B3c4D5e6F7g8H9i0JkLmNoPqRsTuVwXyZ/edit",
		"https://shop.example/p/iphone-15-pro-max-256gb-blue-titanium":                                           "https://shop.example/p/iphone-15-pro-max-256gb-blue-titanium",
		"https://x.example/login/how-to-reset-your-password-in-three-steps":                                      "https://x.example/login/how-to-reset-your-password-in-three-steps",
	} {
		if got := scrubURL(in); got != want {
			t.Errorf("scrubURL(%s) = %s, want %s", in, got, want)
		}
	}
	if got := scrubCards("Card 4111-1111-1111-1111, order 1234567890123"); got != "Card [card number], order 1234567890123" {
		t.Errorf("scrubCards: %s", got)
	}
	st := teachStep{Type: "type", Selector: "#x", Label: "Card", Value: "5555555555554444"}
	st.redact()
	if st.Value != secretValue {
		t.Errorf("a card typed into a plain field was kept: %q", st.Value)
	}
	st = teachStep{Type: "type", Selector: "#pin", Label: "PIN", Value: "1234", Field: &field{Name: "pin"}}
	st.redact()
	if st.Value != secretValue || st.Field != nil {
		t.Errorf("PIN kept: %+v", st)
	}
}

func TestHeadlessUserAgentIsLearned(t *testing.T) {
	own := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/153.0.0.0 Safari/537.36"
	if got := headlessUA(own); got != "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36" {
		t.Fatalf("headlessUA = %q", got)
	}
	if headlessUA("Mozilla/5.0 … Chrome/153.0.0.0 Safari/537.36") != "" {
		t.Fatal("nothing to change should mean no flag")
	}
	s := &Session{}
	v153 := chromeVersion{product: "Chrome/153.0.8010.53", userAgent: own}
	// First start, as itself: learn, then start again with the flag.
	flag := s.nextUA("", v153)
	if !strings.Contains(flag, "Chrome/153.0.0.0") || strings.Contains(flag, "Headless") {
		t.Fatalf("learned %q", flag)
	}
	// Started with the flag, same Chrome: keep it.
	if got := s.nextUA(flag, chromeVersion{product: "Chrome/153.0.8010.53", userAgent: flag}); got != flag {
		t.Fatalf("same Chrome restarted: %q", got)
	}
	// Chrome was updated: learn again rather than claim the old version.
	if got := s.nextUA(flag, chromeVersion{product: "Chrome/154.0.8100.1", userAgent: flag}); got != "" {
		t.Fatalf("after an update: %q", got)
	}
	if s.knownUA() != "" {
		t.Fatal("stale user agent kept")
	}
}

func TestWebURL(t *testing.T) {
	s := &Session{guard: newGuard()}
	s.guard.resolve = func(context.Context, string) ([]netip.Addr, error) { // no lookups leave the machine
		return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
	}
	ctx := context.Background()
	for in, want := range map[string]string{
		"https://example.com/a":   "https://example.com/a",
		"8.8.8.8/path?q=1":        "https://8.8.8.8/path?q=1",
		"about:blank":             "about:blank",
		"http://93.184.215.14:80": "http://93.184.215.14:80",
	} {
		if got, err := s.webURL(ctx, in); err != nil || got != want {
			t.Errorf("webURL(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, says := range map[string]string{
		"file:///Users/me/.ssh/id_rsa":        "only open web pages",
		"javascript:alert(1)":                 "only open web pages",
		"view-source:https://example.com":     "only open web pages",
		"chrome://settings":                   "only open web pages",
		"data:text/html,<b>hi</b>":            "only open web pages",
		"http://169.254.169.254/latest/":      "allow_hosts",
		"http://[fd00:ec2::254]/latest/":      "allow_hosts",
		"127.0.0.1:8080/admin":                "allow_hosts",
		"http://0.0.0.0:631/":                 "allow_hosts",
		"https://":                            "isn't a web address",
		"http://[::ffff:192.168.1.1]/router/": "allow_hosts",
	} {
		if _, err := s.webURL(ctx, in); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("webURL(%s): %v, want it to say %q", in, err, says)
		}
	}
	s.AllowHosts("192.168.1.0/24")
	if _, err := s.webURL(ctx, "http://192.168.1.20:8123/"); err != nil {
		t.Errorf("allowed range refused: %v", err)
	}

	// A name that doesn't exist is said plainly, before the browser tries;
	// behind the owner's proxy (which may know intranet names) it is left
	// to the proxy.
	s.guard.resolve = fakeResolve
	s.guard.upstream = func(*url.URL) (*url.URL, error) { return nil, nil }
	for _, in := range []string{"https://nosuch.test/login", "http://nosuch.test/"} {
		if _, err := s.webURL(ctx, in); err == nil || err.Error() != "I couldn't find nosuch.test. Check the address" {
			t.Errorf("webURL(%s): %v", in, err)
		}
	}
	s.guard.upstream = func(*url.URL) (*url.URL, error) { return &url.URL{Scheme: "http", Host: "proxy.corp:3128"}, nil }
	if _, err := s.webURL(ctx, "https://nosuch.test/"); err != nil {
		t.Errorf("behind a proxy: %v", err)
	}
}

// A visible window is Chrome as installed; only headless presents the
// learned user agent. Every start goes through the guard.
func TestChromeFlags(t *testing.T) {
	s := &Session{dataDir: t.TempDir()}
	const proxy, ua = "http://127.0.0.1:4321", "Mozilla/5.0 (Macintosh) Chrome/153.0.0.0"
	headed := s.chromeFlags(true, proxy, ua)
	if _, ok := headed["user-agent"]; ok {
		t.Fatalf("a visible window got a user agent: %v", headed)
	}
	if headed["headless"] != false {
		t.Fatalf("visible window: %v", headed)
	}
	hidden := s.chromeFlags(false, proxy, ua)
	if hidden["user-agent"] != ua || hidden["headless"] != true {
		t.Fatalf("headless: %v", hidden)
	}
	if _, ok := s.chromeFlags(false, proxy, "")["user-agent"]; ok {
		t.Fatal("nothing learned yet should mean Chrome's own")
	}
	for _, f := range []map[string]any{headed, hidden} {
		if f["proxy-server"] != proxy || f["proxy-bypass-list"] != "<-loopback>" || f["force-webrtc-ip-handling-policy"] != "disable_non_proxied_udp" {
			t.Fatalf("not behind the guard: %v", f)
		}
	}
}

// The pages approvals were asked on are kept for a day; after that only the
// time, so a late yes is refused, and never more than maxAsked.
func TestAskedPagesAreKeptBriefly(t *testing.T) {
	s := &Session{}
	ctx := context.Background()
	in := json.RawMessage(`{"steps":"[{\"type\":\"click\",\"ref\":1}]"}`)
	key := askKey("chat", in)
	s.remember(key, askedPage{url: "https://x.example/", targets: []askedTarget{{sel: "#a", print: "x"}}, at: time.Now().Add(-25 * time.Hour)})
	s.remember(askKey("chat", json.RawMessage(`{"steps":"[]"}`)), askedPage{url: "https://x.example/", targets: []askedTarget{}, at: time.Now()})
	if a := s.asked[key]; a.targets != nil || a.url != "" {
		t.Fatalf("an old page wasn't reduced to its time: %+v", a)
	}
	if err := s.CheckApproved(ctx, "chat", in); err == nil || !strings.Contains(err.Error(), "asked too long ago, so nothing was clicked") {
		t.Fatalf("late yes: %v", err)
	}
	if _, ok := s.asked[key]; ok {
		t.Fatal("the tombstone outlived its answer")
	}
	// Nobody asked here (a daemon restart, a policy that allows it): the
	// address check alone applies.
	if err := s.CheckApproved(ctx, "other chat", in); err != nil {
		t.Fatalf("unasked: %v", err)
	}
	// A question the owner never said yes to doesn't hold back a later call
	// that needs no approval.
	s.remember(key, askedPage{url: "https://x.example/", targets: []askedTarget{{sel: "#a", print: "x"}}, at: time.Now()})
	if err := s.checkStillAsked(tools.Call{ChatKey: "chat", Input: in}); err != nil {
		t.Fatalf("unapproved question held a call back: %v", err)
	}
	// The cap: the oldest goes.
	s.asked = nil
	for i := 0; i < maxAsked+3; i++ {
		s.remember(fmt.Sprint(i), askedPage{at: time.Now().Add(time.Duration(i-1000) * time.Minute), targets: []askedTarget{}})
	}
	if len(s.asked) != maxAsked {
		t.Fatalf("%d kept", len(s.asked))
	}
	if _, ok := s.asked["0"]; ok {
		t.Fatal("the oldest was kept")
	}
}

func TestLoadErrorsInWords(t *testing.T) {
	oldLocked := keychainLocked
	t.Cleanup(func() { keychainLocked = oldLocked })
	keychainLocked = func(context.Context) bool { return false }
	for msg, want := range map[string]string{
		"page load error net::ERR_NAME_NOT_RESOLVED":        "I couldn't find example.com",
		"page load error net::ERR_INTERNET_DISCONNECTED":    "offline",
		"page load error net::ERR_CERT_AUTHORITY_INVALID":   "certificate",
		"page load error net::ERR_CONNECTION_TIMED_OUT":     "didn't answer in time",
		"page load error net::ERR_TUNNEL_CONNECTION_FAILED": "couldn't connect to example.com",
	} {
		if got := loadError("https://example.com/x", errors.New(msg)).Error(); !strings.Contains(got, want) {
			t.Errorf("%s -> %q, want %q", msg, got, want)
		}
	}
	if err := loadError("https://example.com/x", context.DeadlineExceeded); !strings.Contains(err.Error(), "took too long") {
		t.Errorf("deadline: %v", err)
	}
	// Chrome can stop at a locked keychain's prompt when a page first needs
	// its saved logins, not only as it starts.
	keychainLocked = func(context.Context) bool { return true }
	if err := loadError("https://example.com/x", context.DeadlineExceeded); !strings.Contains(err.Error(), "keychain is locked") || !strings.Contains(err.Error(), "unlock-keychain") {
		t.Errorf("deadline, keychain locked: %v", err)
	}
}

// Screenshots of signed-in pages don't pile up: past their keeping time, or
// over the size cap (oldest first), they go; nothing else in the data
// directory is touched.
func TestScreenshotsAreKeptForAWhileOnly(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.png")
	write := func(name string, age time.Duration, size int) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
		return p
	}
	day := 24 * time.Hour
	oldShot := write("browser-1-1.png", 20*day, 10)
	oldPage := write("screenshot-2.png", 15*day, 10)
	fresh := write("browser-3-3.png", time.Hour, 10)
	notes := write("notes.png", 40*day, 10)
	db := write("memory.db", 40*day, 10)
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "browser-4-4.png")
	if err := os.Symlink(outside, link); err != nil {
		link = outside // no symlinks here (Windows without the privilege)
	}
	s := NewSession(config.Browser{}, dir, nil) // defaults: 14 days, 200 MB
	defer s.Close()
	for p, want := range map[string]bool{oldShot: false, oldPage: false, fresh: true, notes: true, db: true, outside: true, link: true} {
		if _, err := os.Lstat(p); (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", filepath.Base(p), err == nil, want)
		}
	}

	// A longer keeping time is honoured.
	kept := write("browser-5-5.png", 20*day, 10)
	s2 := NewSession(config.Browser{KeepScreenshotsDays: 30}, dir, nil)
	s2.Close()
	if _, err := os.Stat(kept); err != nil {
		t.Fatal("30-day setting ignored")
	}

	// The size cap removes the oldest, but never the newest few.
	capDir := t.TempDir()
	var names []string
	for i := 0; i < keepNewest+5; i++ {
		name := fmt.Sprintf("browser-%d-%d.png", 1000+i, i)
		p := filepath.Join(capDir, name)
		if err := os.WriteFile(p, make([]byte, 300<<10), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-time.Duration(100-i) * time.Minute)
		_ = os.Chtimes(p, at, at)
		names = append(names, p)
	}
	if n := pruneShots(capDir, 0, 1<<20, time.Now()); n != 5 {
		t.Fatalf("size cap removed %d, want the 5 oldest", n)
	}
	for i, p := range names {
		if _, err := os.Stat(p); (err == nil) != (i >= 5) {
			t.Errorf("%s kept = %v", filepath.Base(p), err == nil)
		}
	}
}

type fakeHistory []llm.Message

func (f fakeHistory) History(context.Context, string, int) ([]llm.Message, error) { return f, nil }

// Clearing a conversation removes the screenshots it showed, and only the
// browser's own files.
func TestForgettingAConversationRemovesItsScreenshots(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	mk := func(d, name string) string {
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, []byte("png"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	shown := mk(dir, "browser-1727400000-3.png")
	named := mk(dir, "screenshot-1727400001-4.png")
	kept := mk(dir, "browser-1727400002-5.png") // another conversation's
	foreign := mk(other, "browser-1-1.png")
	db := mk(dir, "memory.db")
	h := fakeHistory{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, Text: "page: Bank\n[[image:" + shown + "]]"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "Here it is, screenshot: " + named + " and " + foreign + " and " + dir + "/../" + filepath.Base(other) + "/browser-1-1.png and " + db}}},
	}
	if n := ForgetConversationScreenshots(context.Background(), h, "whatsapp:me", dir); n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	for p, want := range map[string]bool{shown: false, named: false, kept: true, foreign: true, db: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", p, err == nil, want)
		}
	}
}

// A Chrome that never answers ends in a plain error within the timeout, not
// a hang; on a Mac with a locked keychain it says so and how to fix it, and
// the health check shows the same.
func TestChromeThatHangsIsAPlainError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a stand-in Chrome")
	}
	fake := filepath.Join(t.TempDir(), "chrome")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldFind, oldTimeout, oldLocked := findChrome, launchTimeout, keychainLocked
	t.Cleanup(func() { findChrome, launchTimeout, keychainLocked = oldFind, oldTimeout, oldLocked; noteLaunch(nil) })
	findChrome = func() string { return fake }
	launchTimeout = 2 * time.Second
	locked := true
	keychainLocked = func(context.Context) bool { return locked }

	s := newTestSession(t)
	start := time.Now()
	_, err := s.tab(false)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s", took)
	}
	if err == nil || !strings.Contains(err.Error(), "login keychain is locked") || !strings.Contains(err.Error(), "security unlock-keychain") {
		t.Fatalf("locked keychain: %v", err)
	}
	r := Health().Run(context.Background())
	if r.State != health.Fail || !strings.Contains(r.Detail, "keychain") || !strings.Contains(r.Fix, "unlock-keychain") {
		t.Fatalf("health after a failed start: %+v", r)
	}
	// Still locked: asking again says so at once instead of hanging again.
	start = time.Now()
	if _, err := s.tab(false); err == nil || !strings.Contains(err.Error(), "keychain") || time.Since(start) > time.Second {
		t.Fatalf("second try with the keychain still locked: %v after %s", err, time.Since(start))
	}

	locked = false
	_, err = s.tab(false)
	if err == nil || !strings.Contains(err.Error(), "didn't start within 2 seconds") {
		t.Fatalf("hung start: %v", err)
	}

	// Chrome not installed at all.
	findChrome = func() string { return filepath.Join(t.TempDir(), "no-such-chrome") }
	_, err = s.tab(false)
	if err == nil || !strings.Contains(err.Error(), "couldn't find Google Chrome") {
		t.Fatalf("missing Chrome: %v", err)
	}
}

func TestLaunchErrorsInWords(t *testing.T) {
	for err, want := range map[error]string{
		errors.New("chrome failed to start:\nOpening in existing browser session.\n"): "another copy of my browser is still open",
		errors.New(`exec: "google-chrome": executable file not found in $PATH`):       "couldn't find Google Chrome",
		errors.New("websocket url timeout reached"):                                   "Chrome didn't start. Restart Mirrin",
	} {
		if got := explainLaunch(context.Background(), err).Error(); !strings.Contains(got, want) || strings.Contains(got, "$PATH") {
			t.Errorf("%v -> %q, want %q", err, got, want)
		}
	}
}

func TestBrowserHealth(t *testing.T) {
	oldFind, oldLocked, oldProxy := findChrome, keychainLocked, readSystemProxy
	t.Cleanup(func() { findChrome, keychainLocked, readSystemProxy = oldFind, oldLocked, oldProxy; noteLaunch(nil) })
	for _, name := range envProxies {
		t.Setenv(name, "")
	}
	noteLaunch(nil)
	findChrome = func() string { return "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" }
	keychainLocked = func(context.Context) bool { return false }
	readSystemProxy = func() *sysProxy { return nil }
	if r := Health().Run(context.Background()); r.State != health.OK {
		t.Fatalf("all well: %+v", r)
	}

	// A proxy the browser can't follow is a warning with a way round it,
	// until the owner sets one it can.
	for _, what := range []string{pacSetting, wpadSetting, socks4Setting} {
		readSystemProxy = func() *sysProxy { return &sysProxy{unsupported: what} }
		if r := Health().Run(context.Background()); r.State != health.Warn || !strings.Contains(r.Detail, what) || !strings.Contains(r.Fix, "HTTPS_PROXY") {
			t.Fatalf("%s: %+v", what, r)
		}
	}
	t.Setenv("HTTPS_PROXY", "http://proxy.corp:8080")
	if r := Health().Run(context.Background()); r.State != health.OK {
		t.Fatalf("with HTTPS_PROXY set: %+v", r)
	}
	t.Setenv("HTTPS_PROXY", "socks4://proxy.corp:1080")
	if r := Health().Run(context.Background()); r.State != health.Warn || !strings.Contains(r.Detail, "HTTPS_PROXY") || !strings.Contains(r.Fix, "socks5://") {
		t.Fatalf("unusable HTTPS_PROXY: %+v", r)
	}
	t.Setenv("HTTPS_PROXY", "")
	readSystemProxy = func() *sysProxy { return nil }

	// A start that hung on a locked keychain stops failing once it's unlocked.
	keychainLocked = func(context.Context) bool { return true }
	noteLaunch(explainLaunch(context.Background(), errLaunchTimeout))
	if r := Health().Run(context.Background()); r.State != health.Fail || !strings.Contains(r.Detail, "keychain") {
		t.Fatalf("still locked: %+v", r)
	}
	keychainLocked = func(context.Context) bool { return false }
	if r := Health().Run(context.Background()); r.State != health.OK {
		t.Fatalf("unlocked since: %+v", r)
	}
	noteLaunch(nil)
	keychainLocked = func(context.Context) bool { return true }
	if r := Health().Run(context.Background()); r.State != health.Warn || !strings.Contains(r.Detail, "keychain is locked") || !strings.Contains(r.Fix, "Screen Sharing") {
		t.Fatalf("locked keychain: %+v", r)
	}
	findChrome = func() string { return "" }
	if r := Health().Run(context.Background()); r.State != health.Warn || !strings.Contains(r.Fix, "google.com/chrome") {
		t.Fatalf("no Chrome: %+v", r)
	}
	for out, want := range map[string]bool{
		"security: SecKeychainCopySettings /Users/x/Library/Keychains/login.keychain-db: User interaction is not allowed.": true,
		"security: SecKeychainCopySettings: The specified keychain could not be found.":                                    false,
		`Keychain "/Users/x/Library/Keychains/login.keychain-db" lock-on-sleep timeout=21600s`:                             false,
	} {
		if lockedText(out) != want {
			t.Errorf("lockedText(%q) != %v", out, want)
		}
	}
}

// Handing back from the screen names the chat to carry on in: the one that
// last used the browser, lately, and only when the owner had it.
func TestHandBackNamesTheChatToCarryOn(t *testing.T) {
	s := &Session{}
	if got := s.HandBack(); got != "" {
		t.Fatalf("not held: %q", got)
	}
	done, _ := s.live.use("voice:local")
	done()
	s.TakeOver(true)
	if got := s.HandBack(); got != "voice:local" {
		t.Fatalf("held: %q", got)
	}
	if s.live.isHeld() {
		t.Fatal("still held after the hand back")
	}
	if got := s.HandBack(); got != "" {
		t.Fatalf("a second hand back: %q", got)
	}
	s.handTo("Solve the check")
	s.live.mu.Lock()
	s.live.chatAt = time.Now().Add(-handBackTo - time.Minute)
	s.live.mu.Unlock()
	if got := s.HandBack(); got != "" {
		t.Fatalf("a chat from long ago: %q", got)
	}
}

// A hand-over from a chat at this computer gives the screen's address; from
// WhatsApp it gives no 127.0.0.1 link, which would be dead on the phone, but
// says where the page is and that the phone was told, when it was.
func TestHandOverSaysWhereForTheChat(t *testing.T) {
	phoned := true
	var asked []string
	s := &Session{
		OnHandOver: func(string, string) bool { return phoned },
		ScreenURL: func(chatKey string) string {
			asked = append(asked, chatKey)
			if strings.HasPrefix(chatKey, "whatsapp:") {
				return ""
			}
			return "http://127.0.0.1:1/ui"
		},
	}
	here := "https://www.opentable.example/signin"
	local := s.handedOver("screen:local", here, "Sign in")
	if !strings.Contains(local, "is on the presence screen (http://127.0.0.1:1/ui)") {
		t.Fatalf("screen chat: %q", local)
	}
	away := s.handedOver("whatsapp:447700900000", here, "Sign in")
	if strings.Contains(away, "127.0.0.1") || !strings.Contains(away, "It's on the presence screen on "+yourComputer()+", and I've sent a notification to your phone.") {
		t.Fatalf("whatsapp chat: %q", away)
	}
	phoned = false
	if quiet := s.handedOver("whatsapp:447700900000", here, "Sign in"); strings.Contains(quiet, "phone") || !strings.Contains(quiet, "It's on the presence screen on "+yourComputer()+".") {
		t.Fatalf("whatsapp chat, no phone told: %q", quiet)
	}
	if len(asked) != 3 || asked[0] != "screen:local" || asked[1] != "whatsapp:447700900000" {
		t.Fatalf("asked for %q", asked)
	}
}

// A step whose element never appears fails in seconds, not after the whole
// call's time: that silence is what the owner hears on voice.
func TestAMissingElementFailsQuickly(t *testing.T) {
	needChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body><p>nothing to click</p></body></html>`))
	}))
	defer srv.Close()
	old := stepWait
	stepWait = 500 * time.Millisecond
	defer func() { stepWait = old }()
	s := newTestSession(t, "127.0.0.1")
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	start := time.Now()
	_, err := reg.Run(context.Background(), "browser_act", tools.Call{Input: json.RawMessage(`{"url":"` + srv.URL + `","steps":"[{\"type\":\"click\",\"selector\":\"#missing\"}]"}`)})
	if err == nil || !strings.Contains(err.Error(), "nothing matching was visible") || time.Since(start) > 20*time.Second {
		t.Fatalf("after %s: %v", time.Since(start), err)
	}
}
