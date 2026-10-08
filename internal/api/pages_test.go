package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/push"
	"github.com/MavrkAI/Mirrin/internal/qr"
)

// fakePages is the twin behind the device and safety pages.
type fakePages struct {
	mu        sync.Mutex
	routes    []Route
	status    BackupStatus
	checked   []BackupWhere
	setup     []string // the recipient of each phrase set up
	moved     []config.Backup
	now       int
	review    RestoreReview
	reviewed  int
	trust     TrustInfo
	checkFail error
	spending  SpendingInfo
}

func (f *fakePages) Spending(context.Context) SpendingInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spending
}

func (f *fakePages) SetSpendingLimits(_ context.Context, per, month float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spending.PerAction, f.spending.Monthly = per, month
	return nil
}

func (f *fakePages) Routes(context.Context) []Route {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Route(nil), f.routes...)
}
func (f *fakePages) TailnetPeers(context.Context) []TailnetPeer {
	return []TailnetPeer{{Name: "akshays-iphone", OS: "iOS", Online: true}}
}
func (f *fakePages) BackupStatus(context.Context) BackupStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}
func (f *fakePages) BackupCheck(_ context.Context, w BackupWhere) (config.Backup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, w)
	if f.checkFail != nil {
		return config.Backup{}, f.checkFail
	}
	return config.Backup{Target: w.Target, Path: w.Path}, nil
}
func (f *fakePages) BackupSetup(_ context.Context, p backup.Phrase, where config.Backup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setup = append(f.setup, p.Recipient())
	f.status = BackupStatus{On: true, Target: where.Target, Where: where.Path, Health: "ok", KitID: backup.KitID(p)}
	return nil
}
func (f *fakePages) BackupMove(_ context.Context, where config.Backup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moved = append(f.moved, where)
	return nil
}
func (f *fakePages) BackupNow(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now++
	return nil
}
func (f *fakePages) RestoreReview(context.Context) RestoreReview {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.review
}
func (f *fakePages) FinishRestoreReview(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviewed++
	f.review.Pending = false
	return nil
}
func (f *fakePages) Trust(context.Context) TrustInfo { return f.trust }

const tsBase = "https://twin.example.ts.net"

func pagesEnv(t *testing.T) (*env, *fakePages) {
	t.Helper()
	e := newEnv(t)
	f := &fakePages{routes: []Route{{Kind: RouteTailscale, BaseURL: tsBase, Ready: true}, {Kind: RouteRelay, Problem: "No relay of your own is set up."}}}
	e.s.WithLocalPages(f)
	return e, f
}

// The pages and their data answer on this computer only: never on a
// listener other devices reach, not even for a device with admin while
// reach.admin_remote is on.
func TestLocalPagesAnswerOnlyOnThisComputer(t *testing.T) {
	e, _ := pagesEnv(t)
	e.s.AllowAdminRemote()
	_, admin, _ := e.store.Add("Laptop", devices.KindCLI, devices.AllScopes, "", "")
	pages := []string{"/devices/add", "/devices/page", "/backup", "/trust", "/restore/review", "/spending"}
	data := []string{"/devices/add/state", "/devices/routes", "/backup/status", "/trust/info", "/restore/review/state", "/spending/info"}
	for _, p := range append(append([]string{}, pages...), data...) {
		for _, at := range []where{onRemote, onLegacy} {
			w := e.do(at, req{path: p, header: map[string]string{"Authorization": "Bearer " + admin, "Accept": "text/html"}})
			if w.Code != http.StatusNotFound {
				t.Errorf("%s on %s: %d, want 404", p, at, w.Code)
			}
		}
	}
	for _, p := range []string{"/backup/kit", "/backup/kit/finish", "/backup/now", "/backup/target", "/restore/review/done", "/devices/add/test"} {
		w := e.do(onRemote, req{method: "POST", path: p, header: bearer(admin), body: "{}"})
		if w.Code != http.StatusNotFound {
			t.Errorf("POST %s on the remote listener: %d, want 404", p, w.Code)
		}
	}
	// On this computer the menu's link opens each page, with the browser's
	// own key and without the master key in the address.
	for _, p := range pages {
		w := e.do(onLoopback, req{path: p + "?token=" + master, header: map[string]string{"Accept": "text/html"}})
		if w.Code != http.StatusFound || w.Header().Get("Location") != p {
			t.Fatalf("%s from the menu: %d → %q", p, w.Code, w.Header().Get("Location"))
		}
		c := cookieFrom(w, cookieDev)
		if c == nil {
			t.Fatalf("%s: no cookie of the browser's own", p)
		}
		w = e.do(onLoopback, req{path: p, cookies: []*http.Cookie{c}, header: map[string]string{"Accept": "text/html"}})
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<main>") || strings.Contains(w.Body.String(), master) {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	if u := e.s.PageURL("/devices/add?view=reach"); u != "http://127.0.0.1:7742/devices/add?view=reach&token="+master {
		t.Fatalf("PageURL: %s", u)
	}
}

// A property of every code the page makes: it carries the pairing link for
// the route and nothing else, never the master key or any device's key.
func TestAddCodesNeverCarryAKey(t *testing.T) {
	e, _ := pagesEnv(t)
	keys := []string{master}
	for i := 0; i < 8; i++ {
		_, tok, err := e.store.Add("Device", devices.KindPWA, nil, "", "")
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok)
	}
	_, local, _ := e.store.AddLocal("Safari")
	keys = append(keys, local)
	route := Route{Kind: RouteTailscale, BaseURL: tsBase, Ready: true}
	for i := 0; i < 1000; i++ {
		o, err := e.store.NewOffer(devices.KindPWA, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		p := addPayload(route, o, "Mirrin")
		for _, k := range keys {
			if strings.Contains(p, k) {
				t.Fatalf("offer %d: the code carries a key: %s", i, p)
			}
		}
		if strings.Contains(p, devices.TokenPrefix) {
			t.Fatalf("offer %d: something shaped like a device key: %s", i, p)
		}
		u, err := url.Parse(p)
		if err != nil || u.Scheme+"://"+u.Host != tsBase || u.Path != "/pair" || u.RawQuery != "" {
			t.Fatalf("offer %d: %s", i, p)
		}
		f, _ := url.ParseQuery(u.Fragment)
		if f.Get("o") != o.ID || f.Get("s") != o.Secret || f.Get("v") != "2" || len(f) != 4 {
			t.Fatalf("offer %d: the fragment is %q", i, u.Fragment)
		}
	}
	// What the page gets is that same link, drawn.
	ts := loopbackServer(t, e)
	ev := addStream(t, ts.URL)
	first := ev.next(t)
	if first.Step != stepOffer || !strings.HasPrefix(first.Link, tsBase+"/pair#v=2&o=of_") || !strings.HasPrefix(first.QR, "<svg") {
		t.Fatalf("offer event: %+v", first)
	}
	// The drawing says exactly the link (and so carries no key either).
	if want, err := qr.SVG(first.Link, 280); err != nil || string(want) != first.QR {
		t.Fatalf("the page's code isn't its link drawn: %v", err)
	}
	for _, k := range keys {
		if strings.Contains(first.Link, k) {
			t.Fatal("the page's code carries a key")
		}
	}
}

// loopbackServer serves e's API as the loopback listener.
func loopbackServer(t *testing.T, e *env) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := listener{kind: kindLoopback, via: "loopback"}
		e.s.Handler().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), listenerKey, l)))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// stream reads "Add your phone"'s events.
type stream struct {
	stop context.CancelFunc // closes the page
	ch   chan AddEvent
}

func addStream(t *testing.T, base string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, "GET", base+"/devices/add/state", nil)
	r.Header.Set("Authorization", "Bearer "+master)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		cancel()
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &stream{ch: make(chan AddEvent, 64), stop: cancel}
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev AddEvent
			if json.Unmarshal([]byte(line), &ev) == nil {
				s.ch <- ev
			}
		}
		close(s.ch)
	}()
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	return s
}

func (s *stream) next(t *testing.T) AddEvent {
	t.Helper()
	select {
	case ev, ok := <-s.ch:
		if !ok {
			t.Fatal("the stream ended")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3 s")
	}
	return AddEvent{}
}

func (s *stream) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case ev := <-s.ch:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(d):
	}
}

// phoneHeaders is what the pairing page's own fetches send.
func phoneHeaders(c *http.Cookie) (map[string]string, []*http.Cookie) {
	h := map[string]string{"Origin": tsBase, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}
	if c == nil {
		return h, nil
	}
	return h, []*http.Cookie{c}
}

// withTestPush turns on notifications with a sender that records each
// notification, and runs the dispatcher.
func withTestPush(t *testing.T, e *env) chan string {
	t.Helper()
	v, err := push.LoadOrCreateVAPID(filepath.Join(t.TempDir(), "vapid.pem"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := push.Open("")
	sent := make(chan string, 16)
	d := push.NewDispatcher(store, func(_ context.Context, sub push.Subscription, _ []byte, kind, _ string) error {
		sent <- kind + ":" + sub.DeviceID
		return nil
	})
	e.s.WithPush(v, store, d, func(string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	return sent
}

func subscription() string {
	var sub push.Subscription
	sub.Endpoint = "https://fcm.googleapis.com/fcm/send/test"
	sub.Keys.P256DH = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	sub.Keys.Auth = "BTBZMqHH6r4Tts7J_aSIgg"
	b, _ := json.Marshal(sub)
	return string(b)
}

// The page lights each step as the phone takes it, in order: the link
// opened, the claim, the Home Screen app, notifications, Face ID, and the
// test notification that follows by itself. "That wasn't me" cuts the
// phone off at once.
func TestAddStepsFollowThePhone(t *testing.T) {
	e, _ := pagesEnv(t)
	sent := withTestPush(t, e)
	ts := loopbackServer(t, e)
	s := addStream(t, ts.URL)
	offer := s.next(t)
	if offer.Step != stepOffer || offer.Route == nil || offer.Route.BaseURL != tsBase || len(offer.Peers) != 1 || offer.Twin != "Mirrin" {
		t.Fatalf("offer: %+v", offer)
	}
	frag, _ := url.ParseQuery(offer.Link[strings.Index(offer.Link, "#")+1:])
	o, secret := frag.Get("o"), frag.Get("s")
	h, _ := phoneHeaders(nil)

	// A wrong secret lights nothing, and the answer is the same.
	if w := e.do(onRemote, req{method: "POST", path: "/pair/seen", header: h, body: `{"o":"` + o + `","s":"wrong"}`}); w.Code != http.StatusNoContent {
		t.Fatalf("seen, wrong secret: %d", w.Code)
	}
	s.quiet(t, 150*time.Millisecond)
	if w := e.do(onRemote, req{method: "POST", path: "/pair/seen", header: h, body: `{"o":"` + o + `","s":"` + secret + `"}`}); w.Code != http.StatusNoContent {
		t.Fatalf("seen: %d", w.Code)
	}
	if ev := s.next(t); ev.Step != stepScanned {
		t.Fatalf("after the link opened: %+v", ev)
	}
	w := e.do(onRemote, req{method: "POST", path: "/pair/claim", header: h, body: `{"o":"` + o + `","s":"` + secret + `","name":"Akshay's iPhone","kind":"pwa"}`})
	if w.Code != 200 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	var claim ClaimResponse
	_ = json.Unmarshal(w.Body.Bytes(), &claim)
	cookie := cookieFrom(w, cookieSecure)
	ev := s.next(t)
	if ev.Step != stepClaimed || ev.Device == nil || ev.Device.Name != "Akshay's iPhone" || ev.Device.ID != claim.Device.ID {
		t.Fatalf("after the claim: %+v", ev)
	}
	// The Home Screen app has no cookie of its own yet: it spends the ticket.
	if w := e.do(onRemote, req{method: "POST", path: "/pair/ticket", header: h, body: `{"t":"` + claim.Ticket + `"}`}); w.Code != 200 {
		t.Fatalf("ticket: %d %s", w.Code, w.Body.String())
	}
	if ev := s.next(t); ev.Step != stepInstalled {
		t.Fatalf("after the ticket: %+v", ev)
	}
	hc, cookies := phoneHeaders(cookie)
	if w := e.do(onRemote, req{method: "POST", path: "/push/subscribe", header: hc, cookies: cookies, body: subscription()}); w.Code != 200 {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body.String())
	}
	if ev := s.next(t); ev.Step != stepNotifications {
		t.Fatalf("after notifications: %+v", ev)
	}
	if _, err := e.store.AddPasskey(claim.Device.ID, devices.Passkey{ID: "cred", PublicKey: "key", Created: time.Now()}, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if ev := s.next(t); ev.Step != stepPasskey {
		t.Fatalf("after Face ID: %+v", ev)
	}
	select {
	case got := <-sent:
		if got != "test:"+claim.Device.ID {
			t.Fatalf("sent %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no test notification after Face ID")
	}
	// The app's service worker says it arrived.
	if w := e.do(onRemote, req{method: "POST", path: "/push/received", header: hc, cookies: cookies, body: `{"tag":"test"}`}); w.Code != http.StatusNoContent {
		t.Fatalf("received: %d", w.Code)
	}
	if ev := s.next(t); ev.Step != stepTestReceived {
		t.Fatalf("after the test notification: %+v", ev)
	}
	// "That wasn't me".
	lw := e.do(onLoopback, req{method: "POST", path: "/devices/" + claim.Device.ID + "/revoke", header: bearer(master)})
	if lw.Code != 200 {
		t.Fatalf("revoke: %d %s", lw.Code, lw.Body.String())
	}
	if ev := s.next(t); ev.Step != stepRevoked {
		t.Fatalf("after revoking: %+v", ev)
	}
	if w := e.do(onRemote, req{path: "/status", cookies: cookies}); w.Code != http.StatusUnauthorized {
		t.Fatalf("the phone's next request after revoking: %d, want 401", w.Code)
	}
	if subs := e.s.notifying(); len(subs) != 0 {
		t.Fatalf("a revoked phone keeps notifications: %v", subs)
	}
}

// The test notification goes by itself once notifications and Face ID are
// both on, whichever came first, and only once however many passkeys follow.
func TestAddTestNotificationFollowsBothSteps(t *testing.T) {
	e, _ := pagesEnv(t)
	var mu sync.Mutex
	var sent []string
	e.s.pushOn = func(string) bool { return true }
	e.s.pushTest = func(id string) { mu.Lock(); sent = append(sent, id); mu.Unlock() }
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(sent) }
	d, _, err := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	a := e.s.adds.open()
	defer e.s.adds.close(a)
	e.s.adds.mu.Lock()
	e.s.adds.byDevice[d.ID] = a
	e.s.adds.mu.Unlock()
	a.device = d.ID
	e.s.addProgress(d.ID, stepPasskey) // Face ID first
	if count() != 0 {
		t.Fatal("a test went before notifications were on")
	}
	e.s.addProgress(d.ID, stepNotifications)
	e.s.addProgress(d.ID, stepPasskey) // another passkey
	e.s.addProgress(d.ID, stepNotifications)
	if count() != 1 || sent[0] != d.ID {
		t.Fatalf("tests sent: %v, want one", sent)
	}
}

// claimOffer claims an offer from a phone and returns the response.
func claimOffer(e *env, id, secret, name string) *httptest.ResponseRecorder {
	h, _ := phoneHeaders(nil)
	return e.do(onRemote, req{method: "POST", path: "/pair/claim", header: h, body: `{"o":"` + id + `","s":"` + secret + `","name":"` + name + `","kind":"pwa"}`})
}

// Codes a page showed and no longer shows pair nothing: once its phone has
// paired, and once the page is closed. A second device that got in with
// another of the page's codes (a claim racing a refresh) is shown on its
// own, and "That wasn't me" still points at the first phone.
func TestAddPageWithdrawsItsOtherCodes(t *testing.T) {
	e, _ := pagesEnv(t)
	ts := loopbackServer(t, e)
	s := addStream(t, ts.URL)
	first := s.next(t)
	frag, _ := url.ParseQuery(first.Link[strings.Index(first.Link, "#")+1:])
	a := e.s.adds.session(frag.Get("o"), "")
	if a == nil {
		t.Fatal("the page's offer isn't known")
	}
	// Two codes live on one page (as when a claim races a refresh).
	second, _ := e.store.NewOffer(devices.KindPWA, nil, 0)
	e.s.adds.remember(a, second)
	w := claimOffer(e, frag.Get("o"), frag.Get("s"), "Akshay's iPhone")
	if w.Code != 200 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	var mine ClaimResponse
	_ = json.Unmarshal(w.Body.Bytes(), &mine)
	for ev := s.next(t); ev.Step != stepClaimed; ev = s.next(t) {
	}
	if w := claimOffer(e, second.ID, second.Secret, "Stranger"); w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "offer_withdrawn") {
		t.Fatalf("the page's other code after its phone paired: %d %s", w.Code, w.Body.String())
	}
	// The race itself: a code still live when the first phone paired.
	third, _ := e.store.NewOffer(devices.KindPWA, nil, 0)
	e.s.adds.remember(a, third)
	w = claimOffer(e, third.ID, third.Secret, "Stranger")
	if w.Code != 200 {
		t.Fatalf("racing claim: %d %s", w.Code, w.Body.String())
	}
	var other ClaimResponse
	_ = json.Unmarshal(w.Body.Bytes(), &other)
	ev := s.next(t)
	if ev.Step != stepClaimedOther || ev.Device == nil || ev.Device.ID != other.Device.ID || ev.Device.Name != "Stranger" {
		t.Fatalf("second device: %+v", ev)
	}
	a.mu.Lock()
	still := a.device
	a.mu.Unlock()
	if still != mine.Device.ID || e.s.adds.session("", other.Device.ID) != nil {
		t.Fatalf("the page's phone became %s", still)
	}
	// Its progress lights nothing on this page.
	e.s.addProgress(other.Device.ID, stepInstalled)
	s.quiet(t, 150*time.Millisecond)

	// A closed page's code pairs nothing.
	s2 := addStream(t, ts.URL)
	shown := s2.next(t)
	f2, _ := url.ParseQuery(shown.Link[strings.Index(shown.Link, "#")+1:])
	s2.stop()
	deadline := time.Now().Add(3 * time.Second)
	for e.s.adds.session(f2.Get("o"), "") != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w := claimOffer(e, f2.Get("o"), f2.Get("s"), "Later"); w.Code != http.StatusGone {
		t.Fatalf("claim of a closed page's code: %d %s", w.Code, w.Body.String())
	}
}

// A fresh code appears while the page is open, until a phone pairs with
// one; a page with no working route shows the fixes, Tailscale first, and
// a code once a route works.
func TestAddOffersRefreshAndWaitForARoute(t *testing.T) {
	e, f := pagesEnv(t)
	e.s.adds.refresh, e.s.adds.recheck = 250*time.Millisecond, 80*time.Millisecond
	ts := loopbackServer(t, e)
	s := addStream(t, ts.URL)
	a, b := s.next(t), s.next(t)
	if a.Step != stepOffer || b.Step != stepOffer || a.Link == b.Link {
		t.Fatalf("no fresh code: %+v / %+v", a.Link, b.Link)
	}
	// Only the code on screen pairs: the older one was withdrawn when the
	// new one appeared. The newest pairs, and then no more codes come.
	frag, _ := url.ParseQuery(a.Link[strings.Index(a.Link, "#")+1:])
	h, _ := phoneHeaders(nil)
	if w := e.do(onRemote, req{method: "POST", path: "/pair/claim", header: h, body: `{"o":"` + frag.Get("o") + `","s":"` + frag.Get("s") + `","kind":"pwa"}`}); w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "offer_withdrawn") {
		t.Fatalf("claim of an older code: %d %s", w.Code, w.Body.String())
	}
	for latest, tries := b, 0; ; tries++ {
		frag, _ = url.ParseQuery(latest.Link[strings.Index(latest.Link, "#")+1:])
		w := e.do(onRemote, req{method: "POST", path: "/pair/claim", header: h, body: `{"o":"` + frag.Get("o") + `","s":"` + frag.Get("s") + `","kind":"pwa"}`})
		if w.Code == 200 {
			break
		}
		if w.Code != http.StatusGone || tries > 4 {
			t.Fatalf("claim of the newest code: %d %s", w.Code, w.Body.String())
		}
		// A refresh overtook the claim: the code now on screen is the next one.
		if latest = s.next(t); latest.Step != stepOffer {
			t.Fatalf("%+v", latest)
		}
	}
	for {
		ev := s.next(t)
		if ev.Step == stepClaimed {
			break
		}
		if ev.Step != stepOffer && ev.Step != stepScanned {
			t.Fatalf("%+v", ev)
		}
	}
	s.quiet(t, 300*time.Millisecond)

	f.mu.Lock()
	f.routes = []Route{{Kind: RouteRelay, Problem: "No relay of your own is set up.", Fix: "relay fix"}, {Kind: RouteTailscale, Problem: "Tailscale isn't installed on this computer.", Fix: "ts fix", FixURL: "https://tailscale.com/download"}}
	f.mu.Unlock()
	s = addStream(t, ts.URL)
	ev := s.next(t)
	if ev.Step != stepNoRoute || len(ev.Routes) != 2 || ev.Routes[0].Kind != RouteTailscale || ev.Routes[0].Fix != "ts fix" || ev.QR != "" || ev.Link != "" {
		t.Fatalf("empty state: %+v", ev)
	}
	f.mu.Lock()
	f.routes = []Route{{Kind: RouteRelay, BaseURL: "https://twin.example.com", Ready: true}}
	f.mu.Unlock()
	for {
		ev = s.next(t)
		if ev.Step == stepOffer {
			break
		}
	}
	if !strings.HasPrefix(ev.Link, "https://twin.example.com/pair#") {
		t.Fatalf("code once the relay works: %s", ev.Link)
	}
}

func TestBestRouteAndProblems(t *testing.T) {
	rs := []Route{
		{Kind: RouteFiles, BaseURL: "https://home.example:7743", Ready: true},
		{Kind: RouteTailscale, BaseURL: tsBase, Ready: true},
		{Kind: RouteRelay, BaseURL: "http://plain.example", Ready: true}, // a phone app needs https
		{Kind: RouteCloud, Problem: "not built"},
	}
	if b, ok := BestRoute(rs); !ok || b.Kind != RouteTailscale {
		t.Fatalf("best: %+v", b)
	}
	p := RouteProblems(append(rs, Route{Kind: RouteRelay, Problem: "x"}, Route{Kind: RouteTailscale, Problem: "y"}))
	if len(p) != 2 || p[0].Kind != RouteTailscale || p[1].Kind != RouteRelay {
		t.Fatalf("problems (Tailscale first, the paid route never): %+v", p)
	}
	if _, ok := BestRoute(nil); ok {
		t.Fatal("a route from nothing")
	}
}

// The Recovery Kit is shown once and saved only when the owner types the
// check word back: a wrong word saves nothing, five wrong words throw the
// words away, and a reload means new words.
func TestRecoveryKitNeedsTheCheckWord(t *testing.T) {
	e, f := pagesEnv(t)
	kit := func() (id string, words []string) {
		t.Helper()
		w := e.do(onLoopback, req{method: "POST", path: "/backup/kit", header: bearer(master), body: `{"target":"folder","path":"/Volumes/Backup"}`})
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("kit: %d %s", w.Code, w.Body.String())
		}
		var k struct {
			Kit   string   `json:"kit"`
			Words []string `json:"words"`
			Check int      `json:"check"`
			KitID string   `json:"kit_id"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &k)
		if len(k.Words) != 12 || k.Check != 7 || k.Kit == "" || k.KitID == "" {
			t.Fatalf("kit: %+v", k)
		}
		return k.Kit, k.Words
	}
	finish := func(id, word string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"kit": id, "word": word})
		return e.do(onLoopback, req{method: "POST", path: "/backup/kit/finish", header: bearer(master), body: string(b)})
	}
	id, words := kit()
	if len(f.checked) != 1 || f.checked[0].Path != "/Volumes/Backup" {
		t.Fatalf("the destination wasn't checked before the words: %+v", f.checked)
	}
	w := finish(id, words[5]) // word 6
	if w.Code != 400 || !strings.Contains(w.Body.String(), "wrong_word") || !strings.Contains(w.Body.String(), "word 7") {
		t.Fatalf("wrong word: %d %s", w.Code, w.Body.String())
	}
	if len(f.setup) != 0 {
		t.Fatal("backups were turned on with an unchecked word")
	}
	if w := finish(id, "  "+strings.ToUpper(words[6])+" "); w.Code != 200 {
		t.Fatalf("word 7: %d %s", w.Code, w.Body.String())
	}
	p, _ := backup.ParsePhrase(strings.Join(words, " "))
	if len(f.setup) != 1 || f.setup[0] != p.Recipient() {
		t.Fatal("backups weren't turned on with the words shown")
	}
	// The words are gone once saved.
	if w := finish(id, words[6]); w.Code != http.StatusGone {
		t.Fatalf("finishing twice: %d", w.Code)
	}
	id, words = kit()
	for i := 0; i < kitTries-1; i++ {
		if w := finish(id, "zzzz"); w.Code != 400 {
			t.Fatalf("try %d: %d", i, w.Code)
		}
	}
	if w := finish(id, "zzzz"); w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "too many") {
		t.Fatalf("the last try: %d %s", w.Code, w.Body.String())
	}
	if w := finish(id, words[6]); w.Code != http.StatusGone || len(f.setup) != 1 {
		t.Fatalf("thrown-away words still worked: %d", w.Code)
	}
	// Words wait half an hour at most.
	id, words = kit()
	e.s.kits.now = func() time.Time { return time.Now().Add(kitTTL + time.Minute) }
	if w := finish(id, words[6]); w.Code != http.StatusGone {
		t.Fatalf("stale words: %d", w.Code)
	}
	e.s.kits.now = nil
	// A destination that doesn't work says why, before any words.
	f.checkFail = &HumanError{Sentence: "Backups can't go in Mirrin's own folder.", Fix: "Choose a folder on another disk."}
	w = e.do(onLoopback, req{method: "POST", path: "/backup/kit", header: bearer(master), body: `{"target":"folder","path":"/x"}`})
	if w.Code != 400 || strings.Contains(w.Body.String(), "words") || !strings.Contains(w.Body.String(), "own folder") {
		t.Fatalf("bad destination: %d %s", w.Code, w.Body.String())
	}
}

// Moving backups keeps the words; Back up now asks the twin.
func TestBackupMoveAndNow(t *testing.T) {
	e, f := pagesEnv(t)
	if w := e.do(onLoopback, req{method: "POST", path: "/backup/target", header: bearer(master), body: `{"target":"folder","path":"/Volumes/B"}`}); w.Code != 200 {
		t.Fatalf("move: %d", w.Code)
	}
	if len(f.moved) != 1 || f.moved[0].Path != "/Volumes/B" || len(f.setup) != 0 {
		t.Fatalf("moved %+v setup %v", f.moved, f.setup)
	}
	if w := e.do(onLoopback, req{method: "POST", path: "/backup/now", header: bearer(master)}); w.Code != 200 || f.now != 1 {
		t.Fatalf("now: %d %d", w.Code, f.now)
	}
	f.checkFail = errors.New("the folder /Volumes/B isn't there")
	w := e.do(onLoopback, req{method: "POST", path: "/backup/target", header: bearer(master), body: `{"target":"folder","path":"/Volumes/B"}`})
	var ae apiError
	_ = json.Unmarshal(w.Body.Bytes(), &ae)
	if w.Code != 400 || ae.Message != "The folder /Volumes/B isn't there." {
		t.Fatalf("a failed move: %d %+v", w.Code, ae)
	}
}

// After a restore, the review page's devices can be removed one by one,
// and finishing the review is remembered.
func TestRestoreReviewRevokesTheSelected(t *testing.T) {
	e, f := pagesEnv(t)
	f.review = RestoreReview{Pending: true, RestoredAt: time.Now()}
	old, oldTok, _ := e.store.Add("Old phone", devices.KindPWA, nil, "", "")
	mine, mineTok, _ := e.store.Add("My phone", devices.KindPWA, nil, "", "")
	w := e.do(onLoopback, req{path: "/restore/review/state", header: bearer(master)})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"pending":true`) {
		t.Fatalf("state: %d %s", w.Code, w.Body.String())
	}
	if w := e.do(onLoopback, req{method: "POST", path: "/devices/" + old.ID + "/revoke", header: bearer(master)}); w.Code != 200 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := e.do(onRemote, req{path: "/status", header: bearer(oldTok)}); w.Code != 401 {
		t.Fatalf("the removed device: %d", w.Code)
	}
	if w := e.do(onRemote, req{path: "/status", header: bearer(mineTok)}); w.Code != 200 {
		t.Fatalf("the kept device: %d", w.Code)
	}
	if w := e.do(onLoopback, req{method: "POST", path: "/restore/review/done", header: bearer(master)}); w.Code != 200 || f.reviewed != 1 || !strings.Contains(w.Body.String(), `"pending":false`) {
		t.Fatalf("done: %d %s", w.Code, w.Body.String())
	}
	_ = mine
}

// Trust lists every connection the threat model does, and each listener's
// certificate.
func TestTrustFollowsTheThreatModel(t *testing.T) {
	doc, err := os.ReadFile("../../docs/threat-model.md")
	if err != nil {
		t.Fatal(err)
	}
	// Each row of the threat model's outbound table, by the words it starts with.
	rows := map[string]string{"Model use": "model", "Presence screen with weather": "weather", "Google connected": "google",
		"Enabled messaging channels": "channels", "Web, browser": "web", "Optional online voice": "voice", "Optional quick judgments": "jev",
		"Encrypted backup": "backup", "Explicit update": "updates", "Explicit cloud linking": "linked"}
	ids := map[string]bool{}
	for _, o := range OutboundCatalog() {
		ids[o.ID] = true
		if o.On || o.What == "" || o.When == "" || o.Data == "" {
			t.Errorf("%s: %+v", o.ID, o)
		}
		if strings.Contains(o.What+o.When+o.Data, "Mirrin Cloud") {
			t.Errorf("%s names the paid service", o.ID)
		}
	}
	table := regexp.MustCompile(`(?m)^\| ([^|]+?) \| `).FindAllStringSubmatch(string(doc[strings.Index(string(doc), "### Outbound connections"):]), -1)
	seen := 0
	for _, m := range table {
		if m[1] == "Trigger" || strings.HasPrefix(m[1], "---") {
			continue
		}
		seen++
		matched := false
		for prefix, id := range rows {
			if strings.HasPrefix(m[1], prefix) {
				matched = true
				if !ids[id] {
					t.Errorf("the threat model's %q has no line on the Trust page", m[1])
				}
			}
		}
		if !matched {
			t.Errorf("a new row in the threat model, %q: add it to OutboundCatalog", m[1])
		}
	}
	if seen < len(rows) {
		t.Fatalf("read %d rows of the threat model's outbound table", seen)
	}
	for _, id := range []string{"push", "tailscale", "relay", "certificates"} {
		if !ids[id] {
			t.Errorf("no line for %s", id)
		}
	}
	e, _ := pagesEnv(t)
	w := e.do(onLoopback, req{path: "/trust/info", header: bearer(master)})
	var info TrustInfo
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &info) != nil || len(info.Outbound) != len(OutboundCatalog()) || info.Certs == nil {
		t.Fatalf("trust: %d %s", w.Code, w.Body.String())
	}
}

// iPhoneUA is Safari on an iPhone.
const iPhoneUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1"

// "Add your phone" in a real browser: this computer's page in one tab, an
// iPhone (by its user agent and screen) in another, reaching the twin over
// HTTPS. The page lights every step in order within 3 seconds of the scan,
// and "That wasn't me" cuts the phone off.
func TestAddYourPhoneInABrowser(t *testing.T) {
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
	e, f := pagesEnv(t)
	sent := withTestPush(t, e)
	remote := httptest.NewUnstartedServer(e.s.remoteHandler(RemoteOptions{Hostnames: []string{"127.0.0.1"}, Via: "tailscale",
		Limits: Limits{UnauthenticatedPerMinute: 1e6, UnauthenticatedBurst: 1e6, AuthenticatedPerMinute: 1e6, AuthenticatedBurst: 1e6}}))
	remote.StartTLS()
	defer remote.Close()
	f.routes = []Route{{Kind: RouteTailscale, BaseURL: remote.URL, Ready: true}}
	mac := loopbackServer(t, e)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome),
		chromedp.UserDataDir(chromeProfile(t)),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("ignore-certificate-errors", true), // the test's own HTTPS listener
	)
	actx, cancelA := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelA()
	macTab, cancel := chromedp.NewContext(actx)
	defer cancel()
	macTab, cancelT := context.WithTimeout(macTab, 60*time.Second)
	defer cancelT()
	eval := func(ctx context.Context, expr string, out any) {
		t.Helper()
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, out, func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
	}

	// This computer's page records each event it hears, with the time.
	var link string
	if err := chromedp.Run(macTab,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(`(() => { const ES = window.EventSource; window.__steps = [];
				window.EventSource = function (u, o) { const es = new ES(u, o); es.addEventListener('message', e => { try { window.__steps.push([JSON.parse(e.data).step, performance.now()]); } catch {} }); return es; };
				window.EventSource.CONNECTING = 0; window.EventSource.OPEN = 1; window.EventSource.CLOSED = 2; })()`).Do(ctx)
			return err
		}),
		chromedp.Navigate(mac.URL+"/devices/add?token="+master),
		chromedp.Poll(`!!(document.querySelector('#qrImg').src && link)`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`link`, &link),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, remote.URL+"/pair#v=2&o=of_") {
		t.Fatalf("the page's code: %s", link)
	}

	// The phone opens the link, pairs, and its Home Screen app (no cookie
	// yet) spends the install ticket. It is a second tab of the browser
	// that is already running.
	phoneTab, cancelP := chromedp.NewContext(macTab)
	defer cancelP()
	if err := chromedp.Run(phoneTab,
		emulation.SetUserAgentOverride(iPhoneUA).WithPlatform("iPhone"),
		chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile, chromedp.EmulateTouch),
		chromedp.Navigate(link),
		chromedp.WaitVisible("#go", chromedp.ByID),
		chromedp.SetValue("#name", "Akshay's iPhone", chromedp.ByID),
		chromedp.Click("#go", chromedp.ByID),
	); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		var onScreen bool
		if chromedp.Run(phoneTab, chromedp.Evaluate(`location.pathname === '/ui' && document.readyState === 'complete'`, &onScreen)) == nil && onScreen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the phone never reached the screen after pairing")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var status int
	eval(phoneTab, `fetch('/pair/ticket', {method: 'POST', credentials: 'omit', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({t: sessionStorage.getItem('mirrin-install-ticket')})}).then(r => r.status)`, &status)
	if status != 200 {
		t.Fatalf("the Home Screen app's ticket: %d", status)
	}
	eval(phoneTab, `fetch('/push/subscribe', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: `+jsQuote(subscription())+`}).then(r => r.status)`, &status)
	if status != 200 {
		t.Fatalf("notifications: %d", status)
	}
	var phone devices.Device
	for _, d := range e.store.List() {
		if d.Name == "Akshay's iPhone" {
			phone = d
		}
	}
	if phone.ID == "" || phone.Kind != devices.KindPWA {
		t.Fatalf("the phone isn't paired: %+v", e.store.List())
	}
	if _, err := e.store.AddPasskey(phone.ID, devices.Passkey{ID: "cred", PublicKey: "key", Created: time.Now()}, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-sent:
		if got != "test:"+phone.ID {
			t.Fatalf("sent %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no test notification after Face ID")
	}
	// What the app's service worker does when the test notification lands.
	eval(phoneTab, `fetch('/push/received', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: '{"tag":"test"}'}).then(r => r.status)`, &status)
	if status != http.StatusNoContent {
		t.Fatalf("received: %d", status)
	}

	var steps [][]any
	if err := chromedp.Run(macTab,
		chromedp.Poll(`document.querySelectorAll('.steps li.lit').length === 6 && !document.querySelector('#allSet').hidden`, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Evaluate(`window.__steps`, &steps),
	); err != nil {
		var body string
		_ = chromedp.Run(macTab, chromedp.Evaluate(`document.body.innerText + JSON.stringify(window.__steps)`, &body))
		t.Fatalf("the page's steps: %v\n%s", err, body)
	}
	var order []string
	var first, last float64
	for _, s := range steps {
		name, at := s[0].(string), s[1].(float64)
		if name == stepOffer {
			continue
		}
		if len(order) == 0 {
			first = at
		}
		order = append(order, name)
		last = at
	}
	if strings.Join(order, " ") != strings.Join(AddSteps, " ") {
		t.Fatalf("steps in the order %v, want %v", order, AddSteps)
	}
	if last-first > 3000 {
		t.Fatalf("the steps took %.0f ms, want under 3 s", last-first)
	}

	// "That wasn't me".
	var who string
	if err := chromedp.Run(macTab,
		chromedp.WaitVisible("#notMe", chromedp.ByID),
		chromedp.Click("#notMe", chromedp.ByID),
		chromedp.Poll(`document.querySelector('#whoMsg').textContent.includes("can't reach")`, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Text("#whoMsg", &who, chromedp.ByID),
	); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.store.Get(phone.ID); !d.Revoked() || !strings.Contains(who, "Akshay's iPhone") {
		t.Fatalf("after That wasn't me: revoked %v, page says %q", d.Revoked(), who)
	}
	eval(phoneTab, `fetch('/status').then(r => r.status)`, &status)
	if status != http.StatusUnauthorized {
		t.Fatalf("the phone's next request: %d, want 401", status)
	}
}

func jsQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The Trust page's "check it yourself" command connects to the listener's
// own port: a Tailscale listener on :7743 isn't on 443.
func TestRemoteCertsKeepThePort(t *testing.T) {
	e := newEnv(t)
	e.s.lmu.Lock()
	e.s.reached = []Base{{URL: "https://x.ts.net:7743"}, {URL: "https://twin.example.com"}}
	e.s.lmu.Unlock()
	got := e.s.RemoteCerts()
	if len(got) != 2 || got[0].Host != "x.ts.net" || got[0].Port != 7743 || got[1].Host != "twin.example.com" || got[1].Port != 443 {
		t.Fatalf("certs: %+v", got)
	}
}
