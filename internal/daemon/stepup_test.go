package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/push"
	"github.com/MavrkAI/Mirrin/internal/stepup"
	"github.com/MavrkAI/Mirrin/internal/stepup/stepuptest"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// stepUpTwin is a test twin with a dangerous "pay" tool, its API on a
// loopback listener and on a listener other devices reach (plain HTTP at
// localhost, which browsers treat as secure, so passkeys work), a phone
// that just paired, and a push dispatcher that records who hears what.
type stepUpTwin struct {
	*testDaemon
	t             *testing.T
	local, remote string
	phone, tablet devices.Device
	phoneTok      string
	auth          *stepuptest.Authenticator
	mu            sync.Mutex
	pushed        []string // "device kind"
}

func newStepUpTwin(t *testing.T) *stepUpTwin {
	t.Helper()
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		if strings.Contains(last, "pay the cab") {
			return call("p1", "pay", `{"amount":"42.10","to":"cab"}`)
		}
		return butler(last, req)
	})
	td.agent.Tools().Register(tools.New("pay", "pay someone", tools.Schema(map[string]tools.Prop{"amount": {Type: "string"}, "to": {Type: "string"}}), tools.RiskDangerous,
		func(context.Context, tools.Call) (string, error) { return "paid", nil }))
	w := &stepUpTwin{testDaemon: td, t: t}
	ctx, cancel := context.WithCancel(context.Background())
	srv := api.New("127.0.0.1:0", testMaster, td.Daemon).WithScreen(td.Daemon)
	td.attachDevices(ctx, srv)

	store, _ := push.Open("")
	dispatch := push.NewDispatcher(store, func(_ context.Context, sub push.Subscription, _ []byte, kind, _ string) error {
		w.mu.Lock()
		w.pushed = append(w.pushed, sub.DeviceID+" "+kind)
		w.mu.Unlock()
		return nil
	})
	dispatch.Settings = func() (string, push.Config) { return td.Config().Name, push.Config{} }
	pushDispatchers.Store(td.Daemon, dispatch)
	go dispatch.Run(ctx)
	td.attachStepUp(ctx, srv)

	var wg sync.WaitGroup
	serve := func(exp api.Exposure, via string, hosts ...string) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = srv.ServeListener(ctx, ln, api.ListenerConfig{Exposure: exp, Via: via, Hostnames: hosts})
		}()
		return ln.Addr().String()
	}
	w.local = "http://" + serve(api.LoopbackOnly, "loopback")
	w.remote = "http://" + strings.Replace(serve(api.Remote, "relay r2", "localhost"), "127.0.0.1", "localhost", 1)
	t.Cleanup(func() { cancel(); wg.Wait(); pushDispatchers.Delete(td.Daemon) })

	o, err := td.deviceStore().NewOffer(devices.KindPWA, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if w.phone, w.phoneTok, err = td.deviceStore().Claim(o.ID, o.Secret, "Akshay's iPhone", devices.KindPWA, "relay r2", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if w.tablet, _, err = td.deviceStore().Add("Kitchen iPad", devices.KindPWA, nil, "relay r2", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{w.phone.ID, w.tablet.ID} {
		sub := push.Subscription{Endpoint: "https://fcm.googleapis.com/" + id, DeviceID: id}
		sub.Keys.P256DH = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
		sub.Keys.Auth = "BTBZMqHH6r4Tts7J_aSIgg"
		if err := store.Put(sub); err != nil {
			t.Fatal(err)
		}
	}
	w.auth = stepuptest.New(w.remote)
	return w
}

func (w *stepUpTwin) post(base, path, tok string, body []byte, header map[string]string) (int, []byte) {
	w.t.Helper()
	r, _ := http.NewRequest("POST", base+path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (w *stepUpTwin) enrol() {
	w.t.Helper()
	code, body := w.post(w.remote, "/stepup/register/begin", w.phoneTok, nil, nil)
	if code != 200 {
		w.t.Fatalf("begin: %d %s", code, body)
	}
	var begin struct {
		PublicKey json.RawMessage `json:"publicKey"`
		Session   string          `json:"session"`
	}
	_ = json.Unmarshal(body, &begin)
	cred, err := w.auth.Create(begin.PublicKey)
	if err != nil {
		w.t.Fatal(err)
	}
	if code, body := w.post(w.remote, "/stepup/register/finish", w.phoneTok, cred, map[string]string{stepup.Header: begin.Session}); code != 200 {
		w.t.Fatalf("finish: %d %s", code, body)
	}
}

func (w *stepUpTwin) status(id int64) string {
	ap, err := w.store.GetApproval(context.Background(), id)
	if err != nil {
		w.t.Fatal(err)
	}
	return ap.Status
}

func TestPhoneApprovesAPaymentWithAPasskey(t *testing.T) {
	w := newStepUpTwin(t)
	w.owner(t, "pay the cab")
	if w.status(1) != "pending" {
		t.Fatal("no approval")
	}
	// Without a passkey the phone is refused, with the plain alternative.
	code, body := w.post(w.remote, "/approvals/1/approve", w.phoneTok, nil, nil)
	if code != 403 || !strings.Contains(string(body), "passkey_required") || !strings.Contains(string(body), "reply yes 1") {
		t.Fatalf("no passkey: %d %s", code, body)
	}
	// Enrolling is announced: in the owner's chat, and by push to the other
	// devices, not the one that enrolled.
	w.enrol()
	msg := w.ch.next(t)
	for _, want := range []string{`"Akshay's iPhone" can now approve with Face ID`, "just after it paired", "/revoke " + w.phone.ID[:8]} {
		if !strings.Contains(msg, want) {
			t.Errorf("owner told %q, lacking %q", msg, want)
		}
	}
	eventually(t, "a push about the passkey", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.pushed) > 0
	})
	w.mu.Lock()
	pushed := append([]string(nil), w.pushed...)
	w.mu.Unlock()
	if len(pushed) != 1 || pushed[0] != w.tablet.ID+" security" {
		t.Fatalf("pushed %q", pushed)
	}
	if es, _ := w.store.RecentAuditOfKind(context.Background(), "passkey.enrolled", 1); len(es) != 1 || !strings.Contains(es[0].Detail, w.phone.ID) {
		t.Fatalf("audit %+v", es)
	}

	// The 428, then the passkey's signature: the payment is made, and the
	// approval and audit log name the phone and the passkey.
	code, body = w.post(w.remote, "/approvals/1/approve", w.phoneTok, nil, nil)
	if code != 428 {
		t.Fatalf("with a passkey: %d %s", code, body)
	}
	var chal struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(body, &chal)
	assertion, err := w.auth.Get(body)
	if err != nil {
		t.Fatal(err)
	}
	if w.status(1) != "pending" {
		t.Fatal("decided before the check")
	}
	code, body = w.post(w.remote, "/approvals/1/approve", w.phoneTok, assertion, map[string]string{stepup.Header: chal.Session})
	if code != 200 {
		t.Fatalf("approve: %d %s", code, body)
	}
	ap, _ := w.store.GetApproval(context.Background(), 1)
	want := fmt.Sprintf("Akshay's iPhone [%s] (passkey, relay r2, 127.0.0.1)", w.phone.ID)
	if ap.Status != "approved" || ap.DecidedBy != want {
		t.Fatalf("#1 %s by %q, want %q", ap.Status, ap.DecidedBy, want)
	}
	if es, _ := w.store.RecentAuditOfKind(context.Background(), "approval.granted", 1); len(es) != 1 || !strings.Contains(es[0].Detail, want) {
		t.Fatalf("audit %+v", es)
	}
	// Replaying it: 409, saying who decided.
	code, body = w.post(w.remote, "/approvals/1/approve", w.phoneTok, assertion, map[string]string{stepup.Header: chal.Session})
	if code != 409 || !strings.Contains(string(body), `"decided_on":"Akshay's iPhone"`) {
		t.Fatalf("replay: %d %s", code, body)
	}
	// The focused page's data, for another request.
	w.owner(t, "pay the cab")
	r, _ := http.NewRequest("GET", w.remote+"/approvals/2", nil)
	r.Header.Set("Authorization", "Bearer "+w.phoneTok)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var det map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&det)
	resp.Body.Close()
	if det["risk"] != "dangerous" || det["step_up"] != true || det["passkey"] != true || det["status"] != "pending" || det["tool"] != "pay" {
		t.Fatalf("detail %v", det)
	}
}

func TestPhonesChatYesToADangerousRequestNeedsThePage(t *testing.T) {
	w := newStepUpTwin(t)
	w.owner(t, "pay the cab")
	say := func(tok, text string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"channel": "screen", "chat_id": "local", "text": text})
		code, body := w.post(w.remote, "/message", tok, b, nil)
		if code != 200 {
			t.Fatalf("%q: %d %s", text, code, body)
		}
		var m struct{ Reply string }
		_ = json.Unmarshal(body, &m)
		return m.Reply
	}
	// The phone's "yes 1" doesn't pay: it points to the page.
	if reply := say(w.phoneTok, "yes 1"); !strings.Contains(reply, "/approve/1") || !strings.Contains(reply, "Face ID") {
		t.Fatalf("phone's yes: %q", reply)
	}
	if w.status(1) != "pending" {
		t.Fatal("the phone's chat yes approved a payment")
	}
	// The owner's own chat app, and this computer, are unchanged.
	w.owner(t, "yes 1")
	if w.status(1) != "approved" {
		t.Fatalf("the owner's yes: %s", w.status(1))
	}
	w.owner(t, "pay the cab")
	if code, body := w.post(w.local, "/approvals/2/approve", testMaster, nil, nil); code != 200 {
		t.Fatalf("this computer: %d %s", code, body)
	}
	if ap, _ := w.store.GetApproval(context.Background(), 2); ap.Status != "approved" || ap.DecidedBy != "the owner (screen)" {
		t.Fatalf("#2 %s by %q", ap.Status, ap.DecidedBy)
	}
	// A write request from the phone still needs no passkey.
	w.owner(t, "email the boss")
	if code, body := w.post(w.remote, "/approvals/3/approve", w.phoneTok, nil, nil); code != 200 || w.status(3) != "approved" {
		t.Fatalf("write: %d %s", code, body)
	}
}

func TestOwnerLetsADeviceEnrol(t *testing.T) {
	w := newStepUpTwin(t)
	ctx := context.Background()
	// From a paired device, even in its own chat, it isn't the owner's say.
	phoneCtx := api.WithPeer(ctx, api.Peer{Device: &w.tablet, Via: "relay r2"})
	if got, _ := w.command(phoneCtx, "screen:local", "/passkey "+w.tablet.ID[:8]); !strings.Contains(got, "Only you") {
		t.Fatalf("from the tablet: %q", got)
	}
	if got, _ := w.command(ctx, "irc:owner", "/passkey "+w.tablet.ID[:8]); !strings.Contains(got, "IRC") {
		t.Fatalf("by IRC: %q", got)
	}
	list, _ := w.command(ctx, ownerKey, "/passkey")
	if !strings.Contains(list, "Kitchen iPad") || !strings.Contains(list, w.tablet.ID[:8]) {
		t.Fatalf("list %q", list)
	}
	got, _ := w.command(ctx, ownerKey, "/passkey "+w.tablet.ID[:8])
	if !strings.Contains(got, `"Kitchen iPad" can set up Face ID`) {
		t.Fatalf("allow: %q", got)
	}
	if g, err := w.stepUp().Grant(w.tablet, stepup.RPFor(strings.TrimPrefix(w.remote, "http://"), false)); err != nil || g.Kind != stepup.GrantOwner {
		t.Fatalf("grant %+v %v", g, err)
	}
	// And the certificate alarm can take back what was enrolled in a window.
	w.enrol()
	removed, err := w.PasskeyRevoker().RevokePasskeysEnrolled(time.Now().Add(-time.Minute), time.Time{})
	if err != nil || len(removed) != 1 || removed[0].Device.ID != w.phone.ID {
		t.Fatalf("revoke window: %+v %v", removed, err)
	}
}

// say sends text to the screen's chat as the holder of tok, on base, and
// returns the reply.
func (w *stepUpTwin) say(base, tok, text string) string {
	w.t.Helper()
	b, _ := json.Marshal(map[string]string{"channel": "screen", "chat_id": "local", "text": text})
	code, body := w.post(base, "/message", tok, b, nil)
	if code != 200 {
		w.t.Fatalf("%q: %d %s", text, code, body)
	}
	var m struct{ Reply string }
	_ = json.Unmarshal(body, &m)
	return m.Reply
}

// settles makes the model answer words with resolve_approval for id, and
// echo tool results back, as settler does.
func (w *stepUpTwin) settles(words string, id int64) {
	w.llm.mu.Lock()
	defer w.llm.mu.Unlock()
	orig := w.llm.brain
	w.llm.brain = func(last string, req llm.Request) llm.Response {
		if last == words {
			return call("r1", "resolve_approval", fmt.Sprintf(`{"id":%d,"decision":"approve"}`, id))
		}
		if isToolResult(req) && !strings.Contains(last, "PENDING_APPROVAL") {
			return say("Tool: " + last)
		}
		return orig(last, req)
	}
}

// Regression (review): a remote device's yes in its own words, which the
// model settles with resolve_approval, skipped the step-up rule its "yes N"
// follows, and a "yes, always" from it turned on a standing permission
// that step_up=write would have asked a passkey for.
func TestRemoteWordsFollowTheStepUpRule(t *testing.T) {
	const words = "yep, send that one to him"
	t.Run("a write request under step_up write", func(t *testing.T) {
		w := newStepUpTwin(t)
		if err := w.UpdateConfig(func(c *config.Config) { c.Reach.StepUp = "write" }); err != nil {
			t.Fatal(err)
		}
		w.settles(words, 1)
		w.say(w.local, testMaster, "email the boss")
		if got := w.say(w.remote, w.phoneTok, words); !strings.Contains(got, "/approve/1") || w.status(1) != "pending" {
			t.Fatalf("own words: reply %q, #1 %s", got, w.status(1))
		}
		if got := w.say(w.remote, w.phoneTok, "yes 1"); !strings.Contains(got, "/approve/1") || w.status(1) != "pending" {
			t.Fatalf("yes 1: reply %q, #1 %s", got, w.status(1))
		}
		w.say(w.remote, w.phoneTok, "yes, always 1")
		w.say(w.remote, w.phoneTok, "yes")
		if a := w.Config().Autonomy.AlwaysAllow; len(a) != 0 || w.status(1) != "pending" || len(w.ran()) != 0 {
			t.Fatalf("always %v, #1 %s, sent %v", a, w.status(1), w.ran())
		}
		// This computer's yes, always is unchanged.
		w.say(w.local, testMaster, "yes, always 1")
		w.say(w.local, testMaster, "yes")
		if a := w.Config().Autonomy.AlwaysAllow; len(a) != 1 || w.status(1) != "approved" {
			t.Fatalf("this computer: always %v, #1 %s", a, w.status(1))
		}
	})
	t.Run("a tool that has become dangerous", func(t *testing.T) {
		w := newStepUpTwin(t)
		w.settles(words, 1)
		w.say(w.local, testMaster, "email the boss") // asked as a write
		w.agent.Tools().Register(tools.New("send", "send a message", tools.Schema(map[string]tools.Prop{"to": {Type: "string"}}), tools.RiskDangerous,
			func(context.Context, tools.Call) (string, error) { return "sent", nil }))
		if got := w.say(w.remote, w.phoneTok, words); !strings.Contains(got, "/approve/1") || w.status(1) != "pending" {
			t.Fatalf("reply %q, #1 %s", got, w.status(1))
		}
	})
	t.Run("a write request at the default level", func(t *testing.T) {
		w := newStepUpTwin(t)
		w.settles(words, 1)
		w.say(w.local, testMaster, "email the boss")
		if got := w.say(w.remote, w.phoneTok, words); w.status(1) != "approved" {
			t.Fatalf("reply %q, #1 %s", got, w.status(1))
		}
	})
}
