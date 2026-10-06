package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestPWAInjection(t *testing.T) {
	e := newEnv(t)
	name := `Ava <&"` + "'"
	e.s.WithName(name)
	w := e.do(onLoopback, req{path: "/ui", header: bearer(master)})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	got := w.Body.Bytes()
	if bytes.Count(got, []byte(`rel="manifest"`)) != 1 {
		t.Fatal("manifest count")
	}
	pos := bytes.Index(uiHTML, []byte("</head>"))
	added := len(got) - len(uiHTML)
	if !bytes.Equal(got[:pos], uiHTML[:pos]) || !bytes.Equal(got[pos+added:], uiHTML[pos:]) {
		t.Fatal("screen bytes outside injection changed")
	}
	if bytes.Contains(got[pos:pos+added], []byte(name)) {
		t.Fatal("unescaped name")
	}
}
func TestPWAPublicAssetsAndManifest(t *testing.T) {
	e := newEnv(t)
	e.s.WithName("Ava & Me")
	for _, path := range []string{"/sw.js", "/pwa/pwa.js", "/start", "/offline", "/icons/icon-192.png", "/icons/icon-512.png", "/icons/maskable-512.png", "/icons/apple-touch-icon-180.png", "/favicon.svg"} {
		w := e.do(onRemote, req{path: path})
		if w.Code != 200 {
			t.Fatal(path, w.Code)
		}
		if strings.HasPrefix(path, "/icons/") {
			cfg, err := png.DecodeConfig(w.Body)
			if err != nil || cfg.Width != cfg.Height {
				t.Fatal(path, err, cfg)
			}
		}
	}
	for _, ticket := range []string{"", "it_abcdefghijklmnopqrstuvwx", "it_bad<script>"} {
		w := e.do(onRemote, req{path: "/manifest.webmanifest?t=" + ticket})
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		want := "/ui"
		if validTicket(ticket) {
			want = "/start#t=" + ticket
		}
		if m["name"] != "Ava & Me" || m["start_url"] != want {
			t.Fatal(m)
		}
	}
	sw := e.do(onRemote, req{path: "/sw.js"})
	if sw.Header().Get("Service-Worker-Allowed") != "/" || sw.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal(sw.Header())
	}
	if e.do(onRemote, req{path: "/pwa/config.json"}).Code != 401 {
		t.Fatal("config public")
	}
	_, tok, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	if w := e.do(onRemote, req{path: "/pwa/config.json", header: bearer(tok)}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestPWAShellHashesAndVersion(t *testing.T) {
	e := newEnv(t)
	v, b := e.s.pwaVersion()
	var hashes map[string]string
	json.Unmarshal(b, &hashes)
	for path, hash := range hashes {
		w := e.do(onRemote, req{path: path})
		sum := sha256.Sum256(w.Body.Bytes())
		if hex.EncodeToString(sum[:]) != hash {
			t.Fatal(path, "hash differs")
		}
		for _, prefix := range []string{"/events", "/screen", "/message", "/approvals", "/push", "/pair"} {
			if strings.HasPrefix(path, prefix) {
				t.Fatal("private cache entry", path)
			}
		}
	}
	other := New("127.0.0.1:0", master, e.f).WithName("Another twin")
	next, _ := other.pwaVersion()
	if next == v {
		t.Fatal("changed shell kept old version")
	}
}
func TestPWATicketPrefetchCookieAndExpiry(t *testing.T) {
	e := newEnv(t)
	dev, tok, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	ticket := e.store.NewTicket(dev.ID, tok)
	for _, path := range []string{"/start", "/manifest.webmanifest?t=" + ticket, "/pair/ticket?t=" + ticket} {
		w := e.do(onRemote, req{path: path, header: map[string]string{"Purpose": "prefetch"}})
		wantStatus := 200
		if strings.HasPrefix(path, "/pair/ticket") {
			wantStatus = 405
		}
		if w.Code != wantStatus {
			t.Fatal("prefetch status", path, w.Code)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("GET set cookie", path)
		}
	}
	body := `{"t":"` + ticket + `"}`
	origin := map[string]string{"Origin": "https://twin.example.ts.net", "Sec-Fetch-Site": "same-origin"}
	if w := e.do(onRemote, req{method: "POST", path: "/pair/ticket", body: body, header: origin, cookies: []*http.Cookie{{Name: cookieSecure, Value: tok}}}); w.Code != 409 {
		t.Fatal("cookie spent ticket", w.Code)
	}
	if w := e.do(onRemote, req{method: "POST", path: "/pair/ticket", body: body, header: origin}); w.Code != 200 || len(w.Result().Cookies()) == 0 {
		t.Fatal("prefetch consumed ticket", w.Code, w.Body.String())
	}
	if w := e.do(onRemote, req{method: "POST", path: "/pair/ticket", body: body, header: origin}); w.Code != 410 {
		t.Fatal("ticket reused", w.Code)
	}
	ticket = e.store.NewTicket(dev.ID, tok)
	e.store.SetClock(func() time.Time { return time.Now().Add(31 * time.Minute) })
	if _, _, err := e.store.RedeemTicket(ticket); err == nil {
		t.Fatal("ticket never expired")
	}
}

func TestPWABrowserLifecycle(t *testing.T) {
	if os.Getenv("CI") != "" && os.Getenv("MIRRIN_BROWSER_TESTS") == "" {
		t.Skip("set MIRRIN_BROWSER_TESTS=1 where Chrome and local sockets are available")
	}
	chrome := ""
	for _, p := range []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "google-chrome", "chromium"} {
		if path, e := exec.LookPath(p); e == nil {
			chrome = path
			break
		}
	}
	if chrome == "" {
		t.Skip("Chrome unavailable")
	}
	e := newEnv(t)
	e.s.WithName("Test Twin")
	var bump atomic.Bool
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), listenerKey, listener{kind: kindLoopback, via: "loopback"}))
		if bump.Load() && r.URL.Path == "/sw.js" {
			rr := httptest.NewRecorder()
			e.s.Handler().ServeHTTP(rr, r)
			for k, v := range rr.Header() {
				w.Header()[k] = v
			}
			version, _ := e.s.pwaVersion()
			w.Write(bytes.ReplaceAll(rr.Body.Bytes(), []byte(version), []byte(version+"-next")))
			return
		}
		e.s.Handler().ServeHTTP(w, r)
	}))
	ts.Listener.Close()
	var listenErr error
	ts.Listener, listenErr = net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatal(listenErr)
	}
	ts.Start()
	defer ts.Close()
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.UserDataDir(chromeProfile(t)), chromedp.Flag("use-mock-keychain", true), chromedp.Flag("password-store", "basic"))
	alloc, ca := chromedp.NewExecAllocator(t.Context(), opts...)
	defer ca()
	ctx, cc := chromedp.NewContext(alloc)
	defer cc()
	ctx, ct := context.WithTimeout(ctx, 60*time.Second)
	defer ct()
	var name string
	if err := chromedp.Run(ctx, chromedp.Navigate(ts.URL+"/ui?token="+master), chromedp.Poll(`!!navigator.serviceWorker.controller`, nil), chromedp.Evaluate(`navigator.serviceWorker.ready.then(()=>fetch('/manifest.webmanifest')).then(r=>r.json()).then(m=>m.name)`, &name, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		t.Fatal(err)
	}
	if name != "Test Twin" {
		t.Fatal(name)
	}
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		errs, err := page.GetInstallabilityErrors().Do(ctx)
		if len(errs) > 0 {
			t.Errorf("installability: %+v", errs)
		}
		return err
	})); err != nil {
		t.Fatal(err)
	}
	bump.Store(true)
	if err := chromedp.Run(ctx, chromedp.Evaluate(`navigator.serviceWorker.getRegistration().then(r=>r.update())`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }), chromedp.Poll(`caches.keys().then(k=>k.filter(n=>n.startsWith('mirrin-shell-')).length===1&&k.some(n=>n.endsWith('-next')))`, nil)); err != nil {
		t.Fatal(err)
	}
	var offline, keys string
	if err := chromedp.Run(ctx, network.OverrideNetworkState(true, 0, -1, -1), chromedp.ActionFunc(func(context.Context) error { ts.CloseClientConnections(); ts.Close(); return nil }), chromedp.Reload(), chromedp.Poll(`document.body.innerText.includes('last seen')`, nil), chromedp.Text("body", &offline), chromedp.Evaluate(`caches.keys().then(async names=>JSON.stringify((await Promise.all(names.map(async n=>(await (await caches.open(n)).keys()).map(r=>new URL(r.url).pathname)))).flat()))`, &keys, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(offline, "Test Twin") {
		t.Fatal(offline)
	}
	for _, path := range []string{"/events", "/screen", "/message/stream", "/approvals"} {
		if strings.Contains(keys, path) {
			t.Fatal("private data cached", keys)
		}
	}
}

func TestPWAWorkerWithoutSockets(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable; browser lifecycle test covers this on reviewer machines")
	}
	e := newEnv(t)
	sw := e.do(onRemote, req{path: "/sw.js"}).Body.Bytes()
	dir := t.TempDir()
	worker := dir + "/sw.js"
	assets := dir + "/assets.json"
	if err = os.WriteFile(worker, sw, 0600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e.s.pwaShell())
	if err = os.WriteFile(assets, b, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), node, "pwa/sw_test.js", worker, assets)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worker: %v\n%s", err, b)
	}
}

func TestPWAManifestTemplateNameIsData(t *testing.T) {
	e := newEnv(t)
	e.s.WithName(`{{START}} "Ava"`)
	w := e.do(onRemote, req{path: "/manifest.webmanifest"})
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != `{{START}} "Ava"` || m["start_url"] != "/ui" {
		t.Fatal(m)
	}
}

func TestPWAClientWithoutSockets(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	cmd := exec.CommandContext(t.Context(), node, "pwa/client_test.js", "pwa/pwa.js")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("client: %v\n%s", err, b)
	}
}

func TestPWAApprovalFallbackDefersToFocusedPage(t *testing.T) {
	e := newEnv(t)
	w := e.do(onRemote, req{path: "/approve/12"})
	if w.Code != 302 || w.Header().Get("Location") != "/ui#approval=12" {
		t.Fatal(w.Code, w.Header())
	}
	if w := e.do(onRemote, req{path: "/ui"}); w.Code != 401 {
		t.Fatal("fallback bypassed authentication")
	}
	other := newEnv(t)
	other.s.Mount("focused", Remote, func(m *http.ServeMux, a Authz) {
		m.HandleFunc("GET /approve/{id}", a.Require(devices.Approve, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("focused")) }))
	})
	_, tok, _ := other.store.Add("Phone", devices.KindPWA, nil, "", "")
	w = other.do(onRemote, req{path: "/approve/12", header: bearer(tok)})
	if w.Code != 200 || w.Body.String() != "focused" {
		t.Fatal("WP-06 route shadowed", w.Code, w.Body.String())
	}
}
