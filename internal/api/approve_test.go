package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/stepup"
	"github.com/MavrkAI/Mirrin/internal/stepup/stepuptest"
)

// uiScreen shows approvals on the presence screen, for the browser test.
type uiScreen struct {
	*fake
	approvals []map[string]any
}

func (u *uiScreen) Screen(context.Context) any {
	return map[string]any{"name": "Mirrin", "state": "idle", "approvals": u.approvals, "tasks": []any{}, "events": []any{}, "reminders": []any{}, "notices": []any{}, "recent": []any{}}
}

// A phone's browser, with a virtual authenticator standing in for Face ID
// (Chrome's WebAuthn domain), approving through the real pages: the
// focused page and the presence screen's own button, whose bare fetch the
// pwa.js shim steps up.
func TestStepUpInABrowser(t *testing.T) {
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
	e := newStepEnv(t)
	screen := &uiScreen{fake: e.f, approvals: []map[string]any{{"id": 14, "summary": "Transfer $500 to savings", "tool": "pay", "risk": "dangerous", "status": "pending", "created_at": time.Now().Format(time.RFC3339)}}}
	e.s.WithScreen(screen)
	e.f.setApproval(ApprovalDetail{ID: 14, Tool: "pay", What: "Transfer $500 to savings", Risk: "dangerous", Status: "pending", Input: json.RawMessage(`{"amount":500}`)})
	// Served as a remote listener at http://localhost (a secure context, so
	// WebAuthn works without a certificate).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := listener{kind: kindRemote, via: "relay r2", hosts: []string{"localhost"}}
		e.s.Handler().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), listenerKey, l)))
	}))
	defer ts.Close()
	base := strings.Replace(ts.URL, "127.0.0.1", "localhost", 1)

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
	ctx, cancelT := context.WithTimeout(ctx, 90*time.Second)
	defer cancelT()

	chromedp.ListenTarget(ctx, func(ev any) {
		switch ev := ev.(type) {
		case *runtime.EventExceptionThrown:
			t.Logf("page error: %s", ev.ExceptionDetails.Error())
		case *runtime.EventConsoleAPICalled:
			for _, a := range ev.Args {
				t.Logf("console: %s %s", a.Value, a.Description)
			}
		}
	})
	var authID webauthn.AuthenticatorID
	if err := chromedp.Run(ctx,
		network.Enable(),
		network.SetCookie(cookieDev, e.tok).WithDomain("localhost").WithPath("/").WithHTTPOnly(true),
		webauthn.Enable(),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			authID, err = webauthn.AddVirtualAuthenticator(&webauthn.VirtualAuthenticatorOptions{
				Protocol: webauthn.AuthenticatorProtocolCtap2, Transport: webauthn.AuthenticatorTransportInternal,
				HasResidentKey: true, HasUserVerification: true, IsUserVerified: true, AutomaticPresenceSimulation: true,
			}).Do(ctx)
			return err
		}),
	); err != nil {
		t.Fatal(err)
	}
	outcome := func(want string) {
		t.Helper()
		var got string
		err := chromedp.Run(ctx,
			chromedp.Poll(`document.getElementById('outcome').textContent.includes(`+jsString(want)+`)`, nil, chromedp.WithPollingTimeout(15*time.Second)),
			chromedp.Text("#outcome", &got, chromedp.ByID))
		if err != nil {
			_ = chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerText + ' / pwa-status: ' + (document.getElementById('pwa-status')?.textContent || '')`, &got))
			t.Fatalf("waiting for %q: %v\n%s", want, err, got)
		}
	}

	// The focused page: what, why and the screenshot, two buttons.
	var what string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/approve/12"),
		chromedp.WaitVisible("#approve", chromedp.ByID),
		chromedp.Text("#what", &what, chromedp.ByID),
	); err != nil {
		t.Fatal(err)
	}
	if what != "Book the 5:10 cab, £42.10" {
		t.Fatalf("what: %q", what)
	}
	// No passkey yet: refused, and the page offers to set one up.
	if err := chromedp.Run(ctx, chromedp.Click("#approve", chromedp.ByID)); err != nil {
		t.Fatal(err)
	}
	outcome("Face ID or a passkey")
	if len(e.f.decidedVia()) != 0 {
		t.Fatal("decided without a passkey")
	}
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible("#setup", chromedp.ByID),
		chromedp.Click("#setup", chromedp.ByID),
	); err != nil {
		t.Fatal(err)
	}
	outcome("Face ID is set up")
	if n := len(e.store.Passkeys(e.phone.ID)); n != 1 {
		t.Fatalf("%d passkeys after setup", n)
	}
	// With it: the 428, the passkey, the decision.
	if err := chromedp.Run(ctx, chromedp.Click("#approve", chromedp.ByID)); err != nil {
		t.Fatal(err)
	}
	outcome("Approved.")
	if got := e.f.decidedVia(); len(got) != 1 || got[0] != "12 approve passkey" {
		t.Fatalf("decided %v", got)
	}

	// Decided elsewhere while the page was open: 409, and it says where.
	if err := chromedp.Run(ctx, chromedp.Navigate(base+"/approve/13"), chromedp.WaitVisible("#approve", chromedp.ByID)); err != nil {
		t.Fatal(err)
	}
	e.f.setApproval(ApprovalDetail{ID: 13, Tool: "shell", Risk: "dangerous", Status: "approved", DecidedBy: "Akshay's iPad [0123456789abcdef] (passkey, relay r2, 203.0.113.9)", At: time.Now()})
	if err := chromedp.Run(ctx, chromedp.Click("#approve", chromedp.ByID)); err != nil {
		t.Fatal(err)
	}
	outcome("Already approved on Akshay's iPad at ")

	// The presence screen's own Approve button (ui.html, unchanged) goes
	// through the pwa.js shim.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/ui"),
		chromedp.WaitVisible(`.card.need .yes`, chromedp.ByQuery),
		chromedp.Click(`.card.need .yes`, chromedp.ByQuery),
		chromedp.Poll(`true`, nil),
	); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !contains(e.f.decidedVia(), "14 approve passkey") {
		if time.Now().After(deadline) {
			var body string
			_ = chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerText`, &body))
			t.Fatalf("the screen's button: %v\n%s", e.f.decidedVia(), body)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A passkey that can't verify its user gets nowhere.
	e.f.setApproval(ApprovalDetail{ID: 15, Tool: "pay", What: "Pay the plumber", Risk: "dangerous", Status: "pending", Input: json.RawMessage(`{}`)})
	if err := chromedp.Run(ctx,
		webauthn.SetUserVerified(authID, false),
		chromedp.Navigate(base+"/approve/15"),
		chromedp.WaitVisible("#approve", chromedp.ByID),
		chromedp.Click("#approve", chromedp.ByID),
	); err != nil {
		t.Fatal(err)
	}
	outcome("nothing was decided")
	if contains(e.f.decidedVia(), "15 approve passkey") {
		t.Fatal("approved without user verification")
	}
}

func jsString(s string) string { b, _ := json.Marshal(s); return string(b) }

// The test backend's approvals: every id is a pending write request unless
// a test says otherwise, so the older tests' remote approvals still go
// through without a passkey.
type fakeApprovals struct {
	mu    sync.Mutex
	items map[int64]*ApprovalDetail
	via   []string // how each DecideApprovalVia decided: "12 approve passkey"
}

var fakeSets sync.Map // *fake → *fakeApprovals

func approvalsOf(f *fake) *fakeApprovals {
	v, _ := fakeSets.LoadOrStore(f, &fakeApprovals{items: map[int64]*ApprovalDetail{}})
	return v.(*fakeApprovals)
}

func (f *fake) ApprovalDetail(_ context.Context, id int64) (ApprovalDetail, error) {
	set := approvalsOf(f)
	set.mu.Lock()
	defer set.mu.Unlock()
	if d, ok := set.items[id]; ok {
		if d == nil {
			return ApprovalDetail{}, ErrNoApproval
		}
		return *d, nil
	}
	return ApprovalDetail{ID: id, Tool: "send_email", What: "email the boss", Risk: "write", Status: "pending", Input: json.RawMessage(`{"to":"boss"}`)}, nil
}

func (f *fake) DecideApprovalVia(ctx context.Context, id int64, approve bool, method string) (string, error) {
	set := approvalsOf(f)
	set.mu.Lock()
	defer set.mu.Unlock()
	word := "deny"
	if approve {
		word = "approve"
	}
	d, ok := set.items[id]
	if !ok || d == nil {
		// One the test didn't set up stays pending, as DecideApproval's do.
		set.via = append(set.via, fmt.Sprintf("%d %s %s", id, word, method))
		return "done", nil
	}
	if d.Status != "pending" {
		return "", fmt.Errorf("approval #%d was already %s", id, d.Status)
	}
	d.Status = "denied"
	if approve {
		d.Status = "approved"
	}
	p := PeerFrom(ctx)
	d.DecidedBy = "the owner (" + method + ")"
	if p.Device != nil {
		d.DecidedBy = fmt.Sprintf("%s [%s] (%s, %s, %s)", p.Device.Name, p.Device.ID, method, p.Via, p.ClientIP)
	}
	d.At = time.Date(2026, 9, 28, 14, 5, 0, 0, time.Local)
	set.via = append(set.via, fmt.Sprintf("%d %s %s", id, word, method))
	return "done", nil
}

func (f *fake) setApproval(d ApprovalDetail) {
	set := approvalsOf(f)
	set.mu.Lock()
	set.items[d.ID] = &d
	set.mu.Unlock()
}

func (f *fake) decidedVia() []string {
	set := approvalsOf(f)
	set.mu.Lock()
	defer set.mu.Unlock()
	return append([]string(nil), set.via...)
}

const remoteOrigin = "https://twin.example.ts.net"

// stepEnv is a server with step-up, a phone that just claimed a pairing
// offer, and a software passkey holder for it.
type stepEnv struct {
	*env
	v     *stepup.Verifier
	level string
	phone devices.Device
	tok   string
	auth  *stepuptest.Authenticator
}

func newStepEnv(t *testing.T) *stepEnv {
	t.Helper()
	e := &stepEnv{env: newEnv(t), level: "dangerous"}
	e.v = stepup.New(e.store)
	e.s.WithStepUp(e.v, func() string { return e.level })
	o, err := e.store.NewOffer(devices.KindPWA, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.phone, e.tok, err = e.store.Claim(o.ID, o.Secret, "Akshay's iPhone", devices.KindPWA, "tailscale", "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	e.auth = stepuptest.New(remoteOrigin)
	e.f.setApproval(ApprovalDetail{ID: 12, Tool: "pay", What: "Book the 5:10 cab, £42.10", Why: "You asked on Telegram", Risk: "dangerous", Status: "pending", Input: json.RawMessage(`{"amount":"42.10"}`), Screenshot: "/screen/shot?path=x.png"})
	e.f.setApproval(ApprovalDetail{ID: 13, Tool: "shell", What: "run make deploy", Risk: "dangerous", Status: "pending", Input: json.RawMessage(`{"cmd":"make deploy"}`)})
	return e
}

func (e *stepEnv) post(path, body string, header map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	h := bearer(e.tok)
	for k, v := range header {
		h[k] = v
	}
	return e.do(onRemote, req{method: "POST", path: path, body: body, header: h})
}

// enrol sets up the phone's passkey through the API.
func (e *stepEnv) enrol() {
	e.t.Helper()
	w := e.post("/stepup/register/begin", "", nil)
	if w.Code != 200 {
		e.t.Fatalf("begin: %d %s", w.Code, w.Body)
	}
	var begin struct {
		PublicKey json.RawMessage `json:"publicKey"`
		Session   string          `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &begin); err != nil {
		e.t.Fatal(err)
	}
	cred, err := e.auth.Create(begin.PublicKey)
	if err != nil {
		e.t.Fatal(err)
	}
	if w := e.post("/stepup/register/finish", string(cred), map[string]string{stepup.Header: begin.Session}); w.Code != 200 {
		e.t.Fatalf("finish: %d %s", w.Code, w.Body)
	}
}

// challenge asks to decide and expects a 428, returning its session and
// the passkey's answer.
func (e *stepEnv) challenge(id int64, decision string) (string, string) {
	e.t.Helper()
	w := e.post(fmt.Sprintf("/approvals/%d/%s", id, decision), "", nil)
	if w.Code != http.StatusPreconditionRequired {
		e.t.Fatalf("%d %s: %d %s", id, decision, w.Code, w.Body)
	}
	var got struct {
		StepUp  json.RawMessage `json:"stepup"`
		Session string          `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Session == "" || !strings.Contains(string(got.StepUp), `"userVerification":"required"`) {
		e.t.Fatalf("428 body %s", w.Body)
	}
	assertion, err := e.auth.Get(got.StepUp)
	if err != nil {
		e.t.Fatal(err)
	}
	return got.Session, string(assertion)
}

func errorCode(w *httptest.ResponseRecorder) string {
	var body struct{ Error string }
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body.Error
}

func TestRemoteDangerousApprovalNeedsAPasskey(t *testing.T) {
	e := newStepEnv(t)
	// No passkey yet: refused, with a plain way to answer instead.
	w := e.post("/approvals/12/approve", "", nil)
	if w.Code != 403 || errorCode(w) != "passkey_required" || !strings.Contains(w.Body.String(), "reply yes 12") {
		t.Fatalf("no passkey: %d %s", w.Code, w.Body)
	}
	e.enrol()
	// With one: 428, then the signed check decides it, named as a passkey's.
	sid, assertion := e.challenge(12, "approve")
	if got := e.f.decidedVia(); len(got) != 0 {
		t.Fatalf("decided before the check: %v", got)
	}
	w = e.post("/approvals/12/approve", assertion, map[string]string{stepup.Header: sid})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reply":"done"`) {
		t.Fatalf("with the check: %d %s", w.Code, w.Body)
	}
	if got := e.f.decidedVia(); len(got) != 1 || got[0] != "12 approve passkey" {
		t.Fatalf("decided %v", got)
	}
	det, _ := e.f.ApprovalDetail(context.Background(), 12)
	if !strings.Contains(det.DecidedBy, "Akshay's iPhone ["+e.phone.ID+"] (passkey, tailscale, 203.0.113.5)") {
		t.Fatalf("decided by %q", det.DecidedBy)
	}
	// Replaying the finished check: 409, saying who decided and when.
	w = e.post("/approvals/12/approve", assertion, map[string]string{stepup.Header: sid})
	if w.Code != 409 {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	var c struct {
		DecidedBy string    `json:"decided_by"`
		DecidedOn string    `json:"decided_on"`
		At        time.Time `json:"at"`
		Message   string    `json:"message"`
	}
	if json.Unmarshal(w.Body.Bytes(), &c) != nil || c.DecidedOn != "Akshay's iPhone" || c.At.IsZero() || c.Message != "Already approved on Akshay's iPhone at 14:05." || c.DecidedBy == "" {
		t.Fatalf("409 body %s", w.Body)
	}
	// Write-risk requests still need only the approve scope.
	if w := e.post("/approvals/5/approve", "", nil); w.Code != 200 {
		t.Fatalf("write: %d %s", w.Code, w.Body)
	}
}

func TestStepUpIsBoundToTheRequest(t *testing.T) {
	e := newStepEnv(t)
	e.enrol()
	for name, c := range map[string]struct {
		path   string
		before func()
	}{
		"on #13":    {path: "/approvals/13/approve"},
		"as a deny": {path: "/approvals/12/deny"},
		"after input change": {path: "/approvals/12/approve", before: func() {
			e.f.setApproval(ApprovalDetail{ID: 12, Tool: "pay", Risk: "dangerous", Status: "pending", Input: json.RawMessage(`{"amount":"4210"}`)})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			e.f.setApproval(ApprovalDetail{ID: 12, Tool: "pay", Risk: "dangerous", Status: "pending", Input: json.RawMessage(`{"amount":"42.10"}`)})
			sid, assertion := e.challenge(12, "approve")
			if c.before != nil {
				c.before()
			}
			w := e.post(c.path, assertion, map[string]string{stepup.Header: sid})
			if w.Code != 403 || errorCode(w) != "stepup_failed" {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if got := e.f.decidedVia(); len(got) != 0 {
				t.Fatalf("decided %v", got)
			}
			// The check is spent either way.
			if w := e.post("/approvals/12/approve", assertion, map[string]string{stepup.Header: sid}); w.Code != 409 || errorCode(w) != "stepup_used" {
				t.Fatalf("spent check: %d %s", w.Code, w.Body)
			}
		})
	}
	// A check that took more than two minutes.
	e.f.setApproval(ApprovalDetail{ID: 12, Tool: "pay", Risk: "dangerous", Status: "pending", Input: json.RawMessage(`{"amount":"42.10"}`)})
	later := time.Now().Add(3 * time.Minute)
	sid, assertion := e.challenge(12, "approve")
	e.v.SetClock(func() time.Time { return later })
	if w := e.post("/approvals/12/approve", assertion, map[string]string{stepup.Header: sid}); w.Code != 403 || errorCode(w) != "stepup_expired" {
		t.Fatalf("late: %d %s", w.Code, w.Body)
	}
}

func TestPasskeyEnrolmentIsGated(t *testing.T) {
	e := newStepEnv(t)
	var heard []stepup.Enrolment
	e.v.OnEnrol(func(en stepup.Enrolment) { heard = append(heard, en) })
	// A device that didn't just pair can't enrol on its own.
	oldDev, old, err := e.store.Add("Old tablet", devices.KindPWA, nil, "lan", "")
	if err != nil {
		t.Fatal(err)
	}
	w := e.do(onRemote, req{method: "POST", path: "/stepup/register/begin", header: bearer(old)})
	if w.Code != 403 || errorCode(w) != "enrol_not_allowed" || !strings.Contains(w.Body.String(), "/passkey ") {
		t.Fatalf("no grant: %d %s", w.Code, w.Body)
	}
	// Within 15 minutes of the claim it works, once.
	e.enrol()
	if len(heard) != 1 || heard[0].Device.ID != e.phone.ID || heard[0].Grant != stepup.GrantPairing {
		t.Fatalf("heard %+v", heard)
	}
	// Another needs the passkey it has: a 428 to sign first.
	if w := e.post("/stepup/register/begin", "", nil); w.Code != 428 {
		t.Fatalf("second enrolment: %d %s", w.Code, w.Body)
	}
	// The owner can allow one, on this computer only.
	body := `{"device":"` + oldDev.ID[:8] + `"}`
	if w := e.do(onRemote, req{method: "POST", path: "/stepup/allow", body: body, header: bearer(e.tok)}); w.Code != 404 {
		t.Fatalf("allow from a phone: %d %s", w.Code, w.Body)
	}
	if w := e.do(onLoopback, req{method: "POST", path: "/stepup/allow", body: body, header: bearer(master)}); w.Code != 200 {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	w = e.do(onRemote, req{method: "POST", path: "/stepup/register/begin", header: bearer(old)})
	if w.Code != 200 {
		t.Fatalf("after the owner allowed it: %d %s", w.Code, w.Body)
	}
	// Status tells the page what to offer.
	w = e.do(onRemote, req{path: "/stepup/status", header: bearer(e.tok)})
	if !strings.Contains(w.Body.String(), `"passkey":true`) {
		t.Fatalf("status %s", w.Body)
	}
}

func TestThisComputerAndOldListenersDecideAsBefore(t *testing.T) {
	e := newStepEnv(t)
	// This computer approves a dangerous request as it always has: no
	// passkey, no 428.
	for _, tok := range []string{master} {
		w := e.do(onLoopback, req{method: "POST", path: "/approvals/12/approve", header: bearer(tok)})
		if w.Code != 200 || !e.f.saw("decide") {
			t.Fatalf("loopback: %d %s", w.Code, w.Body)
		}
	}
	if got := e.f.decidedVia(); len(got) != 0 {
		t.Fatalf("the loopback decision went through step-up: %v", got)
	}
	// The old plain-HTTP listener can't do passkeys: a dangerous yes is
	// refused with the plain alternative, a write one still works.
	w := e.do(onLegacy, req{method: "POST", path: "/approvals/13/approve", header: bearer(master)})
	if w.Code != 403 || errorCode(w) != "passkey_required" {
		t.Fatalf("legacy dangerous: %d %s", w.Code, w.Body)
	}
	if w := e.do(onLegacy, req{method: "POST", path: "/approvals/7/approve", header: bearer(master)}); w.Code != 200 {
		t.Fatalf("legacy write: %d %s", w.Code, w.Body)
	}
	// A no never needs a passkey unless step_up is all.
	if w := e.post("/approvals/13/deny", "", nil); w.Code != 200 {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	e.level = "all"
	if w := e.post("/approvals/8/deny", "", nil); w.Code != 403 {
		t.Fatalf("step_up all, a no: %d %s", w.Code, w.Body)
	}
	e.level = "write"
	if w := e.post("/approvals/9/approve", "", nil); w.Code != 403 {
		t.Fatalf("step_up write, a write yes: %d %s", w.Code, w.Body)
	}
	e.level = "off" // never less than dangerous
	e.f.setApproval(ApprovalDetail{ID: 14, Risk: "dangerous", Status: "pending"})
	if w := e.post("/approvals/14/approve", "", nil); w.Code != 403 {
		t.Fatalf("step_up off: %d %s", w.Code, w.Body)
	}
}

func TestRevokingADeviceDeletesItsPasskeys(t *testing.T) {
	e := newStepEnv(t)
	e.enrol()
	if len(e.store.Passkeys(e.phone.ID)) != 1 {
		t.Fatal("not enrolled")
	}
	if _, err := e.s.RevokeDevice(e.phone.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.store.Passkeys(e.phone.ID)); n != 0 {
		t.Fatalf("%d passkeys left", n)
	}
	if w := e.post("/approvals/12/approve", "", nil); w.Code != 401 {
		t.Fatalf("revoked phone: %d", w.Code)
	}
}

func TestFocusedApprovalPage(t *testing.T) {
	e := newStepEnv(t)
	w := e.do(onRemote, req{path: "/approve/12", header: map[string]string{"Authorization": "Bearer " + e.tok, "Accept": "text/html"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `<script src="/pwa/pwa.js"></script>`) || !strings.Contains(w.Body.String(), `id="approve"`) {
		t.Fatalf("page: %d", w.Code)
	}
	if w := e.do(onRemote, req{path: "/approve/12", header: map[string]string{"Accept": "text/html"}}); w.Code != 401 {
		t.Fatalf("unpaired page: %d", w.Code)
	}
	w = e.do(onRemote, req{path: "/approvals/12", header: bearer(e.tok)})
	var d map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &d) != nil {
		t.Fatalf("data: %d %s", w.Code, w.Body)
	}
	for k, want := range map[string]any{"what": "Book the 5:10 cab, £42.10", "why": "You asked on Telegram", "risk": "dangerous", "status": "pending", "screenshot": "/screen/shot?path=x.png", "can_decide": true, "step_up": true, "passkey": false, "can_enrol": "pairing", "name": "Mirrin"} {
		if d[k] != want {
			t.Errorf("%s = %v, want %v", k, d[k], want)
		}
	}
	if _, leaked := d["Input"]; leaked {
		t.Error("the stored input is sent to the page")
	}
	if w := e.do(onRemote, req{path: "/approvals/99999", header: bearer(e.tok)}); w.Code != 200 {
		// The fake answers every id; an unknown one is the daemon's 404.
		t.Fatalf("%d", w.Code)
	}
	e.f.setApproval(ApprovalDetail{ID: 40})
	approvalsOf(e.f).mu.Lock()
	approvalsOf(e.f).items[40] = nil
	approvalsOf(e.f).mu.Unlock()
	if w := e.do(onRemote, req{path: "/approvals/40", header: bearer(e.tok)}); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
	if w := e.post("/approvals/40/approve", "", nil); w.Code != 404 {
		t.Fatalf("unknown decision: %d", w.Code)
	}
}

func TestDecidedOn(t *testing.T) {
	for by, want := range map[string]string{
		"Akshay's iPhone [0123456789abcdef] (passkey, relay r2, 203.0.113.9)": "Akshay's iPhone",
		"the owner (channel, telegram)":                                       "Telegram",
		"the owner (screen)":                                                  "the computer I run on",
		"":                                                                    "another device",
	} {
		if got := decidedOn(by); got != want {
			t.Errorf("%q: %q, want %q", by, got, want)
		}
	}
}
