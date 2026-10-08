package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Google's sign-in turns away a browser something drives, so a hand-over of
// it from a chat at this computer goes to an ordinary Chrome window; other
// pages, and chats from elsewhere, stay on the screen.
func TestGoogleSignInGoesToAWindowNobodyDrives(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://accounts.google.com/v3/signin/identifier?continue=https://mail.google.com/mail/": true,
		"https://ACCOUNTS.google.com/":   true,
		"https://accounts.google.co.uk/": true,
		// Gmail asked for by name sends a visitor on to Google's sign-in
		// (the owner's first hand-over, on 2026-10-07, went to the screen).
		"https://mail.google.com/mail/u/0/#inbox":           true,
		"https://workspace.google.com/intl/en-US/gmail/":    true,
		"https://gmail.com/":                                true,
		"https://google.com/":                               true,
		"https://notgoogle.com/":                            false,
		"https://google.com.evil/":                          false,
		"https://accounts.google.com.evil/":                 true, // still Google's prefix: harmless, a window is only a window
		"https://example.com/accounts.google.com":           false,
		"https://example.com/?continue=https://google.com/": false,
		"https://www.booking.com/signin":                    false,
		"":                                                  false,
	} {
		if got := refusesAutomation(raw); got != want {
			t.Errorf("refusesAutomation(%q) = %v", raw, got)
		}
	}
	s := &Session{ScreenURL: func(chatKey string) string {
		if strings.HasPrefix(chatKey, "voice:") || strings.HasPrefix(chatKey, "cli:") {
			return "http://127.0.0.1:1/ui"
		}
		return ""
	}}
	google := "https://accounts.google.com/v3/signin/identifier"
	if got := s.plainURL("voice:local", google, false); got != google {
		t.Fatalf("by voice: %q", got)
	}
	if gmail := "https://mail.google.com/mail/u/0/#inbox"; s.plainURL("voice:local", gmail, false) != gmail {
		t.Fatalf("Gmail by voice went to the screen, where Google's sign-in refuses it")
	}
	if got := s.plainURL("whatsapp:447700900000", google, false); got != "" {
		t.Fatalf("from WhatsApp, the screen as before: %q", got)
	}
	if got := s.plainURL("whatsapp:447700900000", google, true); got != google {
		t.Fatalf("asked for a window: %q", got)
	}
	if got := s.plainURL("voice:local", "https://www.booking.com/signin", true); got != "" {
		t.Fatalf("another site keeps the window the twin can drive: %q", got)
	}

	// Nothing drives it, and nothing marks it driven; the twin's profile and
	// the guard are the same.
	s = NewSession(config.Browser{Enabled: true}, t.TempDir(), nil)
	defer s.Close()
	args := strings.Join(s.plainArgs("127.0.0.1:9", google), " ")
	for _, bad := range []string{"remote-debugging", "enable-automation", "AutomationControlled", "headless", "user-agent"} {
		if strings.Contains(args, bad) {
			t.Errorf("the sign-in window has %q: %s", bad, args)
		}
	}
	for _, want := range []string{"--user-data-dir=" + filepath.Join(s.dataDir, "chrome-profile"), "--proxy-server=127.0.0.1:9", "--proxy-bypass-list=<-loopback>"} {
		if !strings.Contains(args, want) {
			t.Errorf("the sign-in window lacks %q: %s", want, args)
		}
	}
	if !strings.HasSuffix(args, " "+google) {
		t.Errorf("not opened at the page: %s", args)
	}
}

// The tool and the persona say what to do when the owner can't use or see
// the screen: hand over again, in a window.
func TestSigninSaysToUseAWindowWhenTheScreenFails(t *testing.T) {
	s := NewSession(config.Browser{Enabled: true}, t.TempDir(), nil)
	defer s.Close()
	for _, tl := range s.Tools() {
		if tl.Spec().Name != "browser_signin" {
			continue
		}
		d := tl.Spec().Description
		if !strings.Contains(d, "screen isn't working") || !strings.Contains(d, "window set to true") {
			t.Fatalf("browser_signin doesn't say what to do when the screen fails: %s", d)
		}
		return
	}
	t.Fatal("no browser_signin")
}

// A sign-in in the window is the twin's afterwards: the window shares the
// twin's profile, and the twin's next browser tool closes it and reads the
// site signed in.
func TestSignInWindowLoginIsKeptForTheTwin(t *testing.T) {
	needChrome(t)
	var logins atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "owner-signed-in", Path: "/", MaxAge: 3600})
			fmt.Fprint(w, `<html><body>Signed in</body></html>`)
			logins.Add(1)
		case "/whoami":
			c, err := r.Cookie("sid")
			who := "nobody"
			if err == nil {
				who = c.Value
			}
			fmt.Fprintf(w, `<html><body><main>you are %s</main></body></html>`, who)
		default:
			fmt.Fprint(w, `<html><body>hello</body></html>`)
		}
	}))
	defer site.Close()
	old := refuseAutomation
	refuseAutomation = []string{"127.0.0.1"} // the local page stands in for Google's
	defer func() { refuseAutomation = old }()

	s := newTestSession(t, "127.0.0.1")
	s.ScreenURL = func(string) string { return "http://127.0.0.1:1/ui" }
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	call := func(name string, in any) string {
		t.Helper()
		raw, _ := json.Marshal(in)
		out, err := reg.Run(context.Background(), name, tools.Call{ChatKey: "voice:local", Input: raw})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	// The twin's own browser is running: the window takes the profile over.
	call("browse_page", map[string]string{"url": site.URL + "/"})
	got := call("browser_signin", map[string]string{"url": site.URL + "/login", "ask": "Sign in"})
	if !strings.Contains(got, "Chrome window is open on this computer") {
		t.Fatalf("hand-over: %q", got)
	}
	if !s.plainOpen() {
		t.Fatal("no sign-in window")
	}
	if b := s.Brief(context.Background()); !strings.Contains(b, "Wait until they say done") {
		t.Fatalf("the twin isn't told the window is the user's: %q", b)
	}
	for i := 0; logins.Load() == 0; i++ {
		if i > 150 {
			t.Fatal("the window never opened the sign-in page")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// "Done": the twin carries on, signed in.
	if got := call("browse_page", map[string]string{"url": site.URL + "/whoami"}); !strings.Contains(got, "you are owner-signed-in") {
		t.Fatalf("the twin's browser doesn't have the login:\n%s", got)
	}
	if s.plainOpen() {
		t.Fatal("the sign-in window is still open")
	}
}

// A window asked for from a chat away from this computer (WhatsApp) opens
// on this computer, and the twin is told so: it mustn't say the page is in
// front of an owner who may be out.
func TestSigninWindowFromAwaySaysWhereItIs(t *testing.T) {
	needChrome(t)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>Sign in</body></html>`)
	}))
	defer site.Close()
	s := newTestSession(t, "127.0.0.1")
	s.ScreenURL = func(chatKey string) string {
		if strings.HasPrefix(chatKey, "voice:") {
			return "http://127.0.0.1:1/ui"
		}
		return ""
	}
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	call := func(chatKey string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"url": site.URL + "/login", "window": true})
		out, err := reg.Run(context.Background(), "browser_signin", tools.Call{ChatKey: chatKey, Input: raw})
		if err != nil {
			t.Fatalf("browser_signin: %v", err)
		}
		return out
	}
	if got := call("whatsapp:447700900000"); !strings.Contains(got, "not where this chat is") || !strings.Contains(got, "never that it's in front of them") {
		t.Fatalf("from WhatsApp, the twin isn't told the window is on this computer: %q", got)
	}
	if got := call("voice:local"); strings.Contains(got, "not where this chat is") {
		t.Fatalf("by voice, at this computer, the window is in front of them: %q", got)
	}
}
