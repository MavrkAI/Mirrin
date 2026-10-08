package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// signinSite is a local stand-in for a two-step sign-in (an email, Next, a
// password, then a redirect), like Google's.
func signinSite(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/signin":
			fmt.Fprint(w, `<html><body style="margin:0;font:16px sans-serif">
<form id="f" onsubmit="event.preventDefault(); goNext()">
<input id="email" style="position:absolute;left:40px;top:40px;width:300px;height:40px" placeholder="Email or phone">
<button type="button" id="next" style="position:absolute;left:40px;top:120px;width:120px;height:40px" onclick="goNext()">Next</button>
</form>
<script>function goNext(){ const v = document.getElementById('email').value; if (v) location.href = '/password?email=' + encodeURIComponent(v); }</script>
</body></html>`)
		case "/password":
			fmt.Fprintf(w, `<html><body style="margin:0;font:16px sans-serif">
<form method="post" action="/session">
<input type="hidden" name="email" value=%q>
<input type="password" name="pw" id="pw" style="position:absolute;left:40px;top:40px;width:300px;height:40px">
<button id="go" style="position:absolute;left:40px;top:120px;width:120px;height:40px">Sign in</button>
</form></body></html>`, r.URL.Query().Get("email"))
		case "/session":
			_ = r.ParseForm()
			if r.PostForm.Get("pw") != "hunter2" {
				http.Redirect(w, r, "/password?wrong=1&email="+url.QueryEscape(r.PostForm.Get("email")), http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/inbox?as="+url.QueryEscape(r.PostForm.Get("email")), http.StatusSeeOther)
		case "/inbox":
			fmt.Fprintf(w, `<html><body><h1 id="who">Signed in as %s</h1></body></html>`, r.URL.Query().Get("as"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// screenFor serves the real presence screen (internal/api/ui.html) with the
// live-view routes wired to s, as the daemon does.
func screenFor(t *testing.T, s *Session) *httptest.Server {
	t.Helper()
	ui, err := os.ReadFile("../../api/ui.html")
	if err != nil {
		t.Fatal(err)
	}
	chars, _ := os.ReadFile("../../api/characters.js")
	js := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(ui)
	})
	mux.HandleFunc("GET /characters.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(chars)
	})
	mux.HandleFunc("GET /screen", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{
			"name": "Mirrin", "state": "idle", "time": time.Now().Format(time.RFC3339), "health": "all good",
			"events": []any{}, "reminders": []any{}, "notices": []any{}, "approvals": []any{}, "tasks": []any{},
			"recent": []any{}, "ambient_after_seconds": 3600, "persona": "mirrin",
		})
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"kind\":\"state\",\"text\":\"idle\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /browser/state", func(w http.ResponseWriter, r *http.Request) { js(w, s.Live(r.Context())) })
	mux.HandleFunc("GET /browser/stream", func(w http.ResponseWriter, r *http.Request) {
		frames, err := s.Watch(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for f := range frames {
			b, _ := json.Marshal(f)
			fmt.Fprintf(w, "event: frame\ndata: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("POST /browser/input", func(w http.ResponseWriter, r *http.Request) {
		var in InputEvent
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		// A real network is uneven: a little delay on the first of a burst
		// shows up anything sent out of order.
		if in.Type == "text" && strings.ContainsAny(in.Text, "o") {
			time.Sleep(60 * time.Millisecond)
		}
		if err := s.Input(r.Context(), in); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /browser/control", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Hold bool }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Hold {
			s.TakeOver(true)
		} else {
			s.HandBack()
		}
		js(w, s.Live(r.Context()))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return srv
}

// viewer is a second throwaway Chrome showing the presence screen.
func viewer(t *testing.T) context.Context {
	t.Helper()
	dir := t.TempDir()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(findChrome()),
		chromedp.UserDataDir(dir),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("mute-audio", true),
	)
	actx, acancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(actx)
	ctx, tcancel := context.WithTimeout(ctx, 90*time.Second)
	t.Cleanup(func() { tcancel(); cancel(); acancel() })
	if err := chromedp.Run(ctx, chromedp.EmulateViewport(1440, 900)); err != nil {
		t.Skipf("viewer Chrome did not start: %v", err)
	}
	return ctx
}

func waitJS(t *testing.T, ctx context.Context, expr, what string) {
	t.Helper()
	var ok bool
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := chromedp.Run(pctx, chromedp.Poll(expr, &ok, chromedp.WithPollingInterval(50*time.Millisecond))); err != nil || !ok {
		t.Fatalf("%s: never happened (%v)", what, err)
	}
}

// waitTwin polls the twin's own page until expr holds.
func waitTwin(t *testing.T, s *Session, expr, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		s.mu.Lock()
		tab := s.ctx
		s.mu.Unlock()
		var ok bool
		cctx, cancel := context.WithTimeout(tab, 2*time.Second)
		err := chromedp.Run(cctx, chromedp.Evaluate(expr, &ok), chromedp.Evaluate(`location.href + ' | ' + (document.activeElement && document.activeElement.id) + ' | ' + ((document.getElementById('email')||document.getElementById('pw')||{}).value||'')`, &last))
		cancel()
		if err == nil && ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: never happened on the twin's page (at %s)", what, last)
}

// clickOnLive clicks the live view where the twin's page has element id.
func clickOnLive(t *testing.T, ui context.Context, s *Session, id string) {
	t.Helper()
	s.mu.Lock()
	tab := s.ctx
	s.mu.Unlock()
	var at []float64
	if err := chromedp.Run(tab, chromedp.Evaluate(`(() => { const r = document.getElementById(`+jsQuote(id)+`).getBoundingClientRect(); return [r.left + r.width/2, r.top + r.height/2]; })()`, &at)); err != nil {
		t.Fatal(err)
	}
	var pt []float64
	if err := chromedp.Run(ui, chromedp.Evaluate(fmt.Sprintf(`(() => { const r = document.querySelector('#bImg').getBoundingClientRect(); return [r.left + %f / bFrame.w * r.width, r.top + %f / bFrame.h * r.height]; })()`, at[0], at[1]), &pt)); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(ui, chromedp.MouseClickXY(pt[0], pt[1])); err != nil {
		t.Fatal(err)
	}
}

func jsQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// The owner can sign in on a page handed to them on the presence screen:
// a click on the live view focuses the field, typing, Backspace, Tab and
// Enter arrive in order, a pasted password lands, and the page goes on
// through its steps to the signed-in page.
func TestHandedOverSignInWorksOnTheScreen(t *testing.T) {
	needChrome(t)
	site := signinSite(t)
	s := newTestSession(t, "127.0.0.1")
	if _, err := s.signinScreen(context.Background(), "voice:local", site.URL+"/signin", "Sign in"); err != nil {
		t.Fatal(err)
	}
	screen := screenFor(t, s)
	ui := viewer(t)
	if err := chromedp.Run(ui, chromedp.Navigate(screen.URL+"/ui#browser"), page.BringToFront()); err != nil {
		t.Fatal(err)
	}
	waitJS(t, ui, `document.querySelector('#bSheet') && document.querySelector('#bSheet').dataset.mode === 'handover' && !document.querySelector('#bLayer').hidden && bFrame.w > 0 && document.querySelector('#bImg').src.startsWith('data:image/jpeg')`, "the hand-over on the screen")

	// A click on the field in the picture focuses it on the twin's page.
	clickOnLive(t, ui, s, "email")
	waitTwin(t, s, `document.activeElement && document.activeElement.id === 'email'`, "the click focusing the email field")
	// Typed fast, with a slip mended by Backspace: it arrives in order.
	if err := chromedp.Run(ui, chromedp.KeyEvent("owner@example.comm"), chromedp.KeyEvent(kb.Backspace)); err != nil {
		t.Fatal(err)
	}
	waitTwin(t, s, `document.getElementById('email').value === 'owner@example.com'`, "the typed email")
	// Tab to Next, Enter presses it, and the page goes on.
	if err := chromedp.Run(ui, chromedp.KeyEvent(kb.Tab)); err != nil {
		t.Fatal(err)
	}
	waitTwin(t, s, `document.activeElement && document.activeElement.id === 'next'`, "Tab reaching Next")
	if err := chromedp.Run(ui, chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	waitTwin(t, s, `location.pathname === '/password' && !!document.getElementById('pw')`, "the password step")

	// The password, pasted from a password manager, then Enter.
	clickOnLive(t, ui, s, "pw")
	waitTwin(t, s, `document.activeElement && document.activeElement.id === 'pw'`, "the click focusing the password field")
	// A real Cmd+V (Chrome's paste command, as the keyboard sends it), from
	// headless Chrome's own clipboard: this computer's clipboard is left alone.
	var wrote string
	if err := chromedp.Run(ui,
		cdpbrowser.SetPermission(&cdpbrowser.PermissionDescriptor{Name: "clipboard-write"}, cdpbrowser.PermissionSettingGranted).WithOrigin(screen.URL),
		chromedp.Evaluate(`navigator.clipboard.writeText('hunter2').then(() => 'ok', e => String(e))`, &wrote, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }),
		chromedp.Evaluate(`document.activeElement === document.querySelector('#bView')`, nil),
		chromedp.ActionFunc(func(ctx context.Context) error {
			v := func(t input.KeyType) *input.DispatchKeyEventParams {
				return input.DispatchKeyEvent(t).WithKey("v").WithCode("KeyV").WithWindowsVirtualKeyCode(86).WithModifiers(input.ModifierMeta)
			}
			if err := v(input.KeyRawDown).WithCommands([]string{"paste"}).Do(ctx); err != nil {
				return err
			}
			return v(input.KeyUp).Do(ctx)
		})); err != nil || wrote != "ok" {
		t.Fatalf("paste: %v %s", err, wrote)
	}
	waitTwin(t, s, `document.getElementById('pw').value === 'hunter2'`, "the pasted password")
	if err := chromedp.Run(ui, chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	waitTwin(t, s, `location.pathname === '/inbox' && document.getElementById('who').textContent === 'Signed in as owner@example.com'`, "signed in")
}
