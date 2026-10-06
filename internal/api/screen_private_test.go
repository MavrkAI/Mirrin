package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// privateScreen is the fake backend with a full day on the screen.
type privateScreen struct {
	*fake
	data any
}

func (p privateScreen) Screen(context.Context) any { return p.data }

// aFullDay is the screen's data with something private in every field that
// can hold it.
func aFullDay() map[string]any {
	return map[string]any{
		"name": "Mirrin", "state": "idle", "time": "2026-10-03T08:00:00+10:00", "health": "all good",
		"portrait_new": "You've been guarding Friday afternoons for the solicitor.", "portrait_at": "2026-09-27T08:00:00Z", "portrait_ack": true,
		"portrait":  "Worried about the mortgage; sleeps badly before exams.",
		"notices":   []string{"From: bank@example.com Subject: Your overdraft"},
		"events":    []map[string]any{{"title": "School run", "start": "2026-10-03T08:30:00+10:00"}},
		"reminders": []map[string]any{{"id": 1, "text": "Bins out", "due": "2026-10-03T19:00:00+10:00"}},
		"weather":   map[string]any{"temp_c": 18.5, "summary": "Cloudy"},
		"persona":   "mirrin",
		"approvals": []map[string]any{{"id": 7, "summary": "Pay £42.10 to Addison Lee", "tool": "pay", "chat": "telegram:555",
			"screenshot": "/screen/shot?path=checkout.png", "created_at": "2026-10-03T07:59:00+10:00", "risk": "dangerous", "status": "pending", "always_allow": false}},
		"tasks": []map[string]any{{"id": "t1", "title": "Book the cab", "goal": "Get Sam to the clinic", "status": "running",
			"steps":    []map[string]any{{"text": "Open the cab site", "done": true}, {"text": "Pick the 5:10", "done": false}},
			"notes":    []string{"Sam's appointment is at the fertility clinic"},
			"question": "Card ending 4242?", "result": "Booked", "error": "Card declined", "updated": "2026-10-03T07:58:00+10:00"}},
		"recent": []events.Event{
			{Kind: "heard", Text: "Tell Jo I'm leaving him", Data: map[string]string{"channel": "telegram"}},
			{Kind: "said", Text: "It's 18 degrees and cloudy", Data: map[string]string{"channel": "voice"}},
			{Kind: "said", Text: "Your test results are in", Data: map[string]string{"channel": "screen", "origin": "phoneA"}},
			{Kind: "notice", Text: "Telegram is reconnecting"},
		},
		"ambient_after_seconds": 60,
		"left_for_you": []map[string]any{{"id": "l1", "source": "watch", "title": "Inbox", "at": "2026-10-03T07:00:00+10:00",
			"text": "The fertility clinic moved your scan to Friday."}},
		"connected":     map[string]any{"calendar": false, "mail": true, "voice": true},
		"paused":        true,
		"paused_until":  "2026-10-03T09:00:00+10:00",
		"quiet_hours":   "22:30-06:30",
		"standing_by":   "Kitchen iMac",
		"a_later_field": []string{"a field added later is private until it is listed"},
	}
}

// A wall screen paired to look shows the day and that something needs you,
// to anyone walking past: never the portrait, a watcher's raw lines, what an
// approval is for, a task's notes or the owner's chats. The owner's phone
// sees everything.
func TestAWallScreenSeesNothingPrivate(t *testing.T) {
	e := newEnv(t)
	e.s.WithScreen(privateScreen{e.f, aFullDay()})
	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	_, phone, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")

	w := e.do(onRemote, req{path: "/screen", header: bearer(kiosk)})
	if w.Code != 200 {
		t.Fatalf("kiosk /screen: %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, secret := range []string{"mortgage", "solicitor", "overdraft", "Addison", "£42", "telegram:555", "checkout.png", "clinic", "4242", "Booked", "declined", "leaving him", "test results", "Open the cab site", "added later", "scan to Friday", "Kitchen iMac"} {
		if strings.Contains(body, secret) {
			t.Errorf("a wall screen was sent %q: %s", secret, body)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"portrait", "portrait_new", "portrait_at", "portrait_ack", "notices", "a_later_field"} {
		if _, ok := got[k]; ok {
			t.Errorf("a wall screen got %q", k)
		}
	}
	for _, k := range []string{"name", "state", "time", "health", "events", "reminders", "weather", "persona", "ambient_after_seconds", "can", "paused", "paused_until", "quiet_hours"} {
		if _, ok := got[k]; !ok {
			t.Errorf("a wall screen lost %q: %s", k, body)
		}
	}
	if !strings.Contains(body, "School run") || !strings.Contains(body, "Bins out") {
		t.Errorf("a wall screen lost the day's plans: %s", body)
	}
	aps, _ := got["approvals"].([]any)
	if len(aps) != 1 {
		t.Fatalf("approvals %v", got["approvals"])
	}
	ap := aps[0].(map[string]any)
	if keys := sortedKeys(ap); !slices.Equal(keys, []string{"id", "risk", "status", "tool"}) || ap["status"] != "pending" || ap["tool"] != "pay" {
		t.Errorf("a wall screen's approval: %v", ap)
	}
	ts, _ := got["tasks"].([]any)
	if len(ts) != 1 {
		t.Fatalf("tasks %v", got["tasks"])
	}
	task := ts[0].(map[string]any)
	if keys := sortedKeys(task); !slices.Equal(keys, []string{"id", "status", "steps_done", "steps_total", "title", "updated"}) ||
		task["title"] != "Book the cab" || task["steps_done"] != 1.0 || task["steps_total"] != 2.0 {
		t.Errorf("a wall screen's task: %v", task)
	}
	// That something was left, and when: not what it says.
	left, _ := got["left_for_you"].([]any)
	if len(left) != 1 {
		t.Fatalf("left for you %v", got["left_for_you"])
	}
	if l := left[0].(map[string]any); !slices.Equal(sortedKeys(l), []string{"at", "id", "source", "title"}) || l["title"] != "Inbox" {
		t.Errorf("a wall screen's left for you: %v", l)
	}
	// Whether a calendar is connected, for the empty day's words: not
	// whether mail or voice is.
	if c, _ := got["connected"].(map[string]any); !slices.Equal(sortedKeys(c), []string{"calendar"}) || c["calendar"] != false {
		t.Errorf("a wall screen's connected: %v", got["connected"])
	}
	recent, _ := got["recent"].([]any)
	if len(recent) != 1 || recent[0].(map[string]any)["text"] != "It's 18 degrees and cloudy" {
		t.Errorf("a wall screen's recent lines: %v", got["recent"])
	}

	// The owner's phone, which may chat and approve, sees all of it.
	w = e.do(onRemote, req{path: "/screen", header: bearer(phone)})
	body = w.Body.String()
	for _, want := range []string{"mortgage", "overdraft", "Addison", "checkout.png", "clinic", "Card ending 4242", "Tell Jo", "test results", "added later", "scan to Friday"} {
		if w.Code != 200 || !strings.Contains(body, want) {
			t.Errorf("the phone wasn't sent %q: %d %s", want, w.Code, body)
		}
	}
}

// A screen whose data can't be read as fields is sent nothing to a wall,
// rather than everything.
func TestAWallScreenGetsNothingItCantCheck(t *testing.T) {
	e := newEnv(t)
	e.s.WithScreen(privateScreen{e.f, []string{"Worried about the mortgage"}})
	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	w := e.do(onRemote, req{path: "/screen", header: bearer(kiosk)})
	if w.Code != 200 || strings.Contains(w.Body.String(), "mortgage") || !strings.Contains(w.Body.String(), `"can":["view"]`) {
		t.Fatalf("kiosk /screen: %d %s", w.Code, w.Body)
	}
}

// A wall screen says the plain "Good morning" to whoever walks past: the
// owner's name in the persona's hellos is for their own devices.
func TestAWallScreenGreetsNoOneByName(t *testing.T) {
	e := newEnv(t)
	day := aFullDay()
	day["hellos"] = map[string]string{"morning": "Morning, Akshaya.", "afternoon": "Afternoon, Akshaya.", "evening": "Evening, Akshaya.", "late": "Hello, Akshaya."}
	e.s.WithScreen(privateScreen{e.f, day})
	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	_, phone, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	if w := e.do(onRemote, req{path: "/screen", header: bearer(kiosk)}); w.Code != 200 || strings.Contains(w.Body.String(), "Akshaya") || strings.Contains(w.Body.String(), "hellos") {
		t.Fatalf("kiosk /screen: %d %s", w.Code, w.Body)
	}
	if w := e.do(onRemote, req{path: "/screen", header: bearer(phone)}); w.Code != 200 || !strings.Contains(w.Body.String(), "Morning, Akshaya.") {
		t.Fatalf("phone /screen: %d %s", w.Code, w.Body)
	}
}

// The screenshot of what an approval would submit (a checkout, a bank's
// page) is for a device that may talk or decide, not a wall screen.
func TestAWallScreenGetsNoScreenshot(t *testing.T) {
	e := newEnv(t)
	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	_, phone, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	if w := e.do(onRemote, req{path: "/screen/shot?path=x", header: bearer(kiosk)}); w.Code != http.StatusForbidden {
		t.Fatalf("kiosk /screen/shot: %d %s", w.Code, w.Body)
	}
	if w := e.do(onRemote, req{path: "/screen/shot?path=x", header: bearer(phone)}); w.Code != 200 {
		t.Fatalf("phone /screen/shot: %d %s", w.Code, w.Body)
	}
}

// The twin's browser may be on your inbox, your bank or a booking: a wall
// screen never gets its state (title, address, what a hand-over asks) or its
// frames. The owner's phone does.
func TestAWallScreenDoesntWatchTheBrowser(t *testing.T) {
	e := newEnv(t)
	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	_, phone, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	for _, path := range []string{"/browser/state", "/browser/stream"} {
		if w := e.do(onRemote, req{path: path, header: bearer(kiosk)}); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "example.com") {
			t.Errorf("kiosk %s: %d %s", path, w.Code, w.Body)
		}
		if w := e.do(onRemote, req{path: path, header: bearer(phone)}); w.Code != 200 {
			t.Errorf("phone %s: %d %s", path, w.Code, w.Body)
		}
	}
}

// What an approval would do and who asked is kept from a wall screen on the
// request's own page too, not only on /screen: a tapped link or a typed
// address doesn't bring it back.
func TestAWallScreenCantOpenARequest(t *testing.T) {
	e := newStepEnv(t)
	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	w := e.do(onRemote, req{path: "/approvals/12", header: bearer(kiosk)})
	if w.Code != http.StatusForbidden {
		t.Fatalf("kiosk /approvals/12: %d %s", w.Code, w.Body)
	}
	for _, secret := range []string{"5:10", "£42", "Telegram", "x.png"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("a wall screen was sent %q: %s", secret, w.Body)
		}
	}
	if w := e.do(onRemote, req{path: "/approvals/12", header: bearer(e.tok)}); w.Code != 200 || !strings.Contains(w.Body.String(), "5:10") {
		t.Fatalf("phone /approvals/12: %d %s", w.Code, w.Body)
	}
}

// On the live feed a wall screen hears the character move, system notices,
// that an approval came or went, and what was said out loud in the room. It
// never hears the owner's chats on other channels, a message delivered to
// them, what an approval is for, or kinds of event it doesn't know.
func TestAWallScreensFeedKeepsChatsPrivate(t *testing.T) {
	e := newEnv(t)
	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	_, phone, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	publish := func(b *events.Bus) {
		b.Publish(events.Event{Kind: "said", Text: "Your test results are in", Data: map[string]string{"channel": "telegram"}})
		b.Publish(events.Event{Kind: "heard", Text: "what's the weather?", Data: map[string]string{"channel": "voice"}})
		b.Publish(events.Event{Kind: "said", Text: "18 degrees and cloudy", Data: map[string]string{"channel": "voice"}})
		b.Publish(events.Event{Kind: "note", Text: "Reading the overdraft email", Data: map[string]string{"channel": "screen", "origin": "phoneA"}})
		b.Publish(events.Event{Kind: "message", Text: "Morning: the overdraft is due", Data: map[string]any{"id": 1}})
		b.Publish(events.Event{Kind: "remembered", Text: "You're seeing a divorce lawyer", Data: map[string]any{"id": 2}})
		b.Publish(events.Event{Kind: "notice", Text: "Bank: overdraft notice", Data: map[string]string{"channel": "telegram"}})
		b.Publish(events.Event{Kind: "approval", Text: "#7 needs you: Pay £42.10 to Addison Lee", Data: map[string]any{"id": 7, "status": "pending", "summary": "Pay £42.10 to Addison Lee", "chat": "telegram:555"}})
		b.Publish(events.Event{Kind: "browser", Text: "handover", Data: map[string]string{"url": "https://bank.example/login", "ask": "Sign in to the bank"}})
		b.Publish(events.Event{Kind: "notice", Text: "Telegram is reconnecting"})
	}

	got := e.heardOnEvents(kiosk, publish, "Telegram is reconnecting")
	for _, secret := range []string{"test results", "overdraft", "divorce", "Addison", "telegram:555", "bank.example", "Sign in to the bank", `"message"`, `"remembered"`} {
		if strings.Contains(got, secret) {
			t.Errorf("a wall screen heard %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{"what's the weather?", "18 degrees and cloudy", `"kind":"approval","data":{"id":7,"status":"pending"}`, `"kind":"browser","text":"handover"`} {
		if !strings.Contains(got, want) {
			t.Errorf("a wall screen didn't hear %q:\n%s", want, got)
		}
	}

	got = e.heardOnEvents(phone, publish, "Telegram is reconnecting")
	for _, want := range []string{"test results", "Reading the overdraft email", `"message"`, `"remembered"`, "Bank: overdraft notice", "Addison", "Sign in to the bank"} {
		if !strings.Contains(got, want) {
			t.Errorf("the phone didn't hear %q:\n%s", want, got)
		}
	}
}

// Only a device that may do nothing but look is a wall screen. The master
// key and this computer never are.
func TestWhoIsViewOnly(t *testing.T) {
	dev := func(scopes ...devices.Scope) *devices.Device { return &devices.Device{Scopes: scopes} }
	for _, c := range []struct {
		name string
		p    Peer
		want bool
	}{
		{"a wall screen", Peer{Device: dev(devices.View)}, true},
		{"nobody", Peer{}, true},
		{"a phone", Peer{Device: dev(devices.View, devices.Chat, devices.Approve)}, false},
		{"a phone that may only approve", Peer{Device: dev(devices.View, devices.Approve)}, false},
		{"an admin device", Peer{Device: dev(devices.View, devices.Admin)}, false},
		{"a wall screen on this computer", Peer{Device: dev(devices.View), Loopback: true}, false},
		{"the master key", Peer{Master: true}, false},
	} {
		if got := viewOnly(c.p); got != c.want {
			t.Errorf("%s: viewOnly %v, want %v", c.name, got, c.want)
		}
	}
}

// heardOnEvents opens GET /events as tok on the remote listener, runs publish
// once it is listening, and returns what it heard up to the line last.
func (e *env) heardOnEvents(tok string, publish func(*events.Bus), last string) string {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), listenerKey,
		listener{kind: kindRemote, via: "tailscale", hosts: []string{"twin.example.ts.net"}}))
	defer cancel()
	r := httptest.NewRequest("GET", "http://twin.example.ts.net/events", nil).WithContext(ctx)
	r.Host, r.RemoteAddr, r.TLS = "twin.example.ts.net", "203.0.113.5:40000", &tls.ConnectionState{}
	r.Header.Set("Authorization", "Bearer "+tok)
	w := &streamWriter{h: http.Header{}}
	done := make(chan struct{})
	go func() { defer close(done); e.s.Handler().ServeHTTP(w, r) }()
	wait := func(what string) {
		deadline := time.Now().Add(3 * time.Second)
		for !strings.Contains(w.String(), what) {
			if time.Now().After(deadline) {
				e.t.Fatalf("/events never said %q:\n%s", what, w.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait("data: ") // the state it starts with: it is listening
	publish(e.f.bus)
	wait(last)
	cancel()
	<-done
	return w.String()
}

// streamWriter is a ResponseWriter a test can read while it is written.
type streamWriter struct {
	mu  sync.Mutex
	h   http.Header
	buf bytes.Buffer
}

func (s *streamWriter) Header() http.Header { return s.h }
func (s *streamWriter) WriteHeader(int)     {}
func (s *streamWriter) Flush()              {}
func (s *streamWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(b)
}
func (s *streamWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
