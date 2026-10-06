package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

type fakeReach struct {
	mu       sync.Mutex
	info     ReachInfo
	checks   []ReachCheck
	verrs    error
	asked    []string
	useErr   error
	checkout string
}

func (f *fakeReach) ReachInfo(context.Context) ReachInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info
}
func (f *fakeReach) ReachVerify(context.Context) ([]ReachCheck, error) { return f.checks, f.verrs }
func (f *fakeReach) ReachUseCloud(_ context.Context, handle string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, handle)
	return f.checkout, f.useErr
}

func reachEnv(t *testing.T) (*env, *fakeReach) {
	t.Helper()
	e := newEnv(t)
	f := &fakeReach{info: ReachInfo{Mode: "tailscale", Cards: []ReachCard{
		{Kind: ReachCloud, Available: true},
		{Kind: ReachRelay, Detail: "No relay of your own is set up."},
		{Kind: ReachTailscale, Using: true, Ready: true, Address: "https://mac.tail1234.ts.net"},
	}}}
	e.s.WithReachPage(f)
	return e, f
}

// The page lists the paid handle last, whatever order the twin gave, and
// the page and its data answer on this computer only.
func TestReachPageRendersCloudLastAndOnlyHere(t *testing.T) {
	e, f := reachEnv(t)
	e.s.AllowAdminRemote()
	_, admin, _ := e.store.Add("Laptop", devices.KindCLI, devices.AllScopes, "", "")
	for _, at := range []where{onRemote, onLegacy} {
		for _, p := range []string{"/reach", "/reach/info"} {
			if w := e.do(at, req{path: p, header: map[string]string{"Authorization": "Bearer " + admin, "Accept": "text/html"}}); w.Code != http.StatusNotFound {
				t.Errorf("GET %s on %s: %d, want 404", p, at, w.Code)
			}
		}
		for _, p := range []string{"/reach/verify", "/reach/cloud"} {
			if w := e.do(at, req{method: "POST", path: p, header: bearer(admin), body: "{}"}); w.Code != http.StatusNotFound {
				t.Errorf("POST %s on %s: %d, want 404", p, at, w.Code)
			}
		}
	}
	if len(f.asked) != 0 {
		t.Fatal("a remote request reached the twin")
	}

	w := e.do(onLoopback, req{path: "/reach?token=" + master, header: map[string]string{"Accept": "text/html"}})
	c := cookieFrom(w, cookieDev)
	if w.Code != http.StatusFound || c == nil {
		t.Fatalf("from the menu: %d", w.Code)
	}
	w = e.do(onLoopback, req{path: "/reach", cookies: []*http.Cookie{c}, header: map[string]string{"Accept": "text/html"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Reach from anywhere") {
		t.Fatalf("page: %d", w.Code)
	}
	w = e.do(onLoopback, req{path: "/reach/info", header: bearer(master)})
	var info ReachInfo
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &info) != nil {
		t.Fatalf("info: %d %s", w.Code, w.Body)
	}
	var kinds []string
	for _, c := range info.Cards {
		kinds = append(kinds, c.Kind)
	}
	if strings.Join(kinds, ",") != "tailscale,relay,cloud" {
		t.Fatalf("cards %v", kinds)
	}
	// In the page itself, the card order is kept.
	html := string(reachHTML)
	if strings.Index(html, "tailscale:'Tailscale'") > strings.Index(html, "cloud:'Mirrin Cloud'") {
		t.Fatal("the page names Cloud before Tailscale")
	}
}

func TestReachPageVerifyAndUseCloud(t *testing.T) {
	e, f := reachEnv(t)
	f.checks = []ReachCheck{{Name: "certificate", OK: true, Detail: "the key is this computer's"}, {Name: "caa", Detail: "CAA names another account"}}
	w := e.do(onLoopback, req{method: "POST", path: "/reach/verify", header: bearer(master)})
	var v struct {
		OK     bool         `json:"ok"`
		Checks []ReachCheck `json:"checks"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.OK || len(v.Checks) != 2 {
		t.Fatalf("verify: %d %s", w.Code, w.Body)
	}
	f.verrs = errors.New("nothing to verify: reach is off")
	if w := e.do(onLoopback, req{method: "POST", path: "/reach/verify", header: bearer(master)}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Nothing to verify") {
		t.Fatalf("verify refused: %d %s", w.Code, w.Body)
	}

	f.checkout = "https://pay.example/checkout/1"
	w = e.do(onLoopback, req{method: "POST", path: "/reach/cloud", header: bearer(master), body: `{"handle":" Ember-Otter-42 "}`})
	if w.Code != 200 || !strings.Contains(w.Body.String(), f.checkout) || f.asked[0] != "ember-otter-42" {
		t.Fatalf("use cloud: %d %s %v", w.Code, w.Body, f.asked)
	}
	f.useErr = &HumanError{Sentence: "That name is taken.", Fix: "Try another, or leave it empty for a random one."}
	w = e.do(onLoopback, req{method: "POST", path: "/reach/cloud", header: bearer(master), body: `{}`})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "That name is taken.") || !strings.Contains(w.Body.String(), "Try another") {
		t.Fatalf("refused: %d %s", w.Code, w.Body)
	}
}

// A paired phone learns each relay's status address for this twin, so its
// offline page can say when the twin was last seen.
func TestPWAConfigCarriesStatusURLs(t *testing.T) {
	e := newEnv(t)
	urls := []string{"https://r1.relay.example/v1/status/ember-otter-42?k=abc", "https://r2.relay.example/v1/status/ember-otter-42?k=abc"}
	e.s.WithStatusURLs(func() []string { return urls })
	_, tok, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	w := e.do(onRemote, req{path: "/pwa/config.json", header: bearer(tok)})
	var cfg struct {
		StatusURLs []string `json:"status_urls"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &cfg) != nil || strings.Join(cfg.StatusURLs, " ") != strings.Join(urls, " ") {
		t.Fatalf("config: %d %s", w.Code, w.Body)
	}
	urls = nil
	w = e.do(onRemote, req{path: "/pwa/config.json", header: bearer(tok)})
	if !strings.Contains(w.Body.String(), `"status_urls":[]`) {
		t.Fatalf("no relays: %s", w.Body)
	}
}
