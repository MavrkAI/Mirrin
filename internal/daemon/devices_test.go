package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/tasks"
)

const testMaster = "0123456789abcdef0123456789abcdef"

func TestNewDeviceIsAnnouncedWithAWayToRevokeIt(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	var shown []string
	prev := desktopNotify
	desktopNotify = func(_, body string) error { shown = append(shown, body); return nil }
	t.Cleanup(func() { desktopNotify = prev })

	srv := api.New("127.0.0.1:0", testMaster, d)
	d.attachDevices(t.Context(), srv)
	store := d.deviceStore()
	if store == nil || srv.Devices() != store {
		t.Fatal("the API and /revoke don't share the registry")
	}
	dev, tok, err := store.Add("Akshay's iPhone", devices.KindPWA, nil, "tailscale", "100.64.0.9")
	if err != nil {
		t.Fatal(err)
	}
	d.devicePaired(api.PairEvent{Device: dev, IP: "100.64.0.9", Via: "tailscale", How: api.HowClaimed})
	if len(shown) != 1 {
		t.Fatalf("shown %q", shown)
	}
	msg := shown[0]
	// With no chat to reply in, it says how to revoke without one.
	for _, want := range []string{`"Akshay's iPhone"`, "phone or tablet", "100.64.0.9 over Tailscale", "see the screen, talk and approve requests", "mirrin devices revoke " + dev.ID[:8], "wasn't you"} {
		if !strings.Contains(msg, want) {
			t.Errorf("notice lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "reply /revoke") {
		t.Errorf("a desktop notification can't be replied to: %s", msg)
	}

	ctx := context.Background()
	list, err := d.command(ctx, "cli:local", "/revoke")
	if err != nil || !strings.Contains(list, dev.ID[:8]) || !strings.Contains(list, "Akshay's iPhone") {
		t.Fatalf("list: %q %v", list, err)
	}
	got, err := d.command(ctx, "cli:local", "/revoke "+dev.ID[:8])
	if err != nil || !strings.Contains(got, "can't reach me any more") || strings.Contains(got, "shared key") {
		t.Fatalf("revoke: %q %v", got, err)
	}
	if _, ok := srv.Devices().Authenticate(tok); ok {
		t.Fatal("the revoked device still authenticates with the API")
	}
	if got, _ := d.command(ctx, "cli:local", "/revoke zzzz"); !strings.Contains(got, "don't have a paired device") {
		t.Fatalf("unknown id: %q", got)
	}
	if got, _ := d.command(ctx, "cli:local", "/revoke"); !strings.Contains(got, "No devices are paired") {
		t.Fatalf("empty list: %q", got)
	}
}

func TestOldScreenNoticeSaysWhatHappened(t *testing.T) {
	e := api.PairEvent{Device: devices.Device{ID: "0123456789abcdef", Name: "Chrome on Linux", Kind: devices.KindPWA, Scopes: devices.DefaultScopes(devices.KindPWA), SharedKey: true}, IP: "192.168.1.9", Via: "lan", How: api.HowLegacyScreen}
	msg := pairedNotice(e, true)
	for _, want := range []string{"set up before devices had their own keys", "on your network", "old shared key", "reply /revoke 01234567", "change that key"} {
		if !strings.Contains(msg, want) {
			t.Errorf("notice lacks %q: %s", want, msg)
		}
	}
	if msg := pairedNotice(e, false); !strings.Contains(msg, "`mirrin devices revoke 01234567`") {
		t.Errorf("notice without a chat: %s", msg)
	}
}

// Revoking a device that came in with the old shared key changes that key,
// and says so.
func TestRevokingAnOldScreenChangesTheSharedKey(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	quietDesktop(t)
	master, err := api.LoadOrCreateToken(d.Config().DataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New("127.0.0.1:0", master, d)
	d.attachDevices(t.Context(), srv)
	dev, _, err := d.deviceStore().AddShared("Chrome on Linux", devices.KindPWA, "lan", "192.168.1.9")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.command(context.Background(), "whatsapp:61400", "/revoke "+dev.ID[:8])
	if err != nil || !strings.Contains(got, "can't reach me any more") || !strings.Contains(got, "changed that key") {
		t.Fatalf("revoke: %q %v", got, err)
	}
	now, _ := os.ReadFile(api.TokenPath(d.Config().DataDir))
	if strings.TrimSpace(string(now)) == master {
		t.Fatal("the shared key wasn't changed")
	}
}

// Regression: /revoke answered on any chat that counts as the owner's,
// including a paired phone without the admin scope (through POST /message)
// and email or IRC, where anyone can claim to be the owner.
func TestRevokeNeedsTheOwnersOwnChat(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	srv := api.New("127.0.0.1:0", testMaster, d)
	d.attachDevices(t.Context(), srv)
	victim, tok, _ := d.deviceStore().Add("Laptop", devices.KindCLI, nil, "", "")
	for _, key := range []string{"mail:owner@example.com", "irc:akshay"} {
		if got, _ := d.command(context.Background(), key, "/revoke "+victim.ID[:8]); !strings.Contains(got, "by email or IRC") {
			t.Errorf("%s: %q", key, got)
		}
	}
	if _, ok := d.deviceStore().Authenticate(tok); !ok {
		t.Fatal("a forgeable chat revoked a device")
	}
}

// serveDaemonAPI runs d's API on a loopback port and returns a function that
// posts to /message with a key.
func serveDaemonAPI(t *testing.T, d *Daemon) (*api.Server, func(tok, body string) (int, string)) {
	t.Helper()
	srv := api.New("127.0.0.1:0", testMaster, d)
	d.attachDevices(t.Context(), srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln, api.LoopbackOnly, "loopback"); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	post := func(tok, body string) (int, string) {
		r, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/message", bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Reply   string `json:"reply"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Reply + out.Message
	}
	return srv, post
}

// Regression: the chat scope unlocked what other scopes guard. A device
// paired to talk could revoke devices (admin) and answer requests (approve)
// by typing /revoke or "yes N".
func TestChatOnlyDeviceCantRevokeOrApprove(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	_, post := serveDaemonAPI(t, d)
	store := d.deviceStore()
	victim, victimTok, _ := store.Add("Laptop", devices.KindCLI, nil, "", "")
	_, chatOnly, _ := store.Add("Phone", devices.KindPWA, []devices.Scope{devices.View, devices.Chat}, "", "")
	_, full, _ := store.Add("Tablet", devices.KindPWA, nil, "", "")

	code, reply := post(chatOnly, `{"channel":"screen","chat_id":"local","text":"/revoke `+victim.ID[:8]+`"}`)
	if code != 200 || !strings.Contains(reply, "managed from the computer I run on") {
		t.Fatalf("/revoke from a chat-only phone: %d %q", code, reply)
	}
	if _, ok := store.Authenticate(victimTok); !ok {
		t.Fatal("a chat-only phone revoked a device")
	}
	for _, text := range []string{"yes 1", "no #1"} {
		code, reply = post(chatOnly, `{"channel":"screen","chat_id":"local","text":"`+text+`"}`)
		if code != 200 || !strings.Contains(reply, "paired to talk, not to answer requests") {
			t.Fatalf("%q from a chat-only phone: %d %q", text, code, reply)
		}
	}
	// Naming the owner's WhatsApp chat is refused before it reaches me.
	if code, reply = post(chatOnly, `{"channel":"whatsapp","chat_id":"61400","text":"yes 1"}`); code != 400 || !strings.Contains(reply, "its own chat") {
		t.Fatalf("another app's chat: %d %q", code, reply)
	}
	// A device that may approve gets the usual answer, and this computer's
	// key may manage devices.
	if code, reply = post(full, `{"channel":"screen","chat_id":"local","text":"yes 1"}`); code != 200 || strings.Contains(reply, "paired to talk") {
		t.Fatalf("yes from a device that may approve: %d %q", code, reply)
	}
	if code, reply = post(testMaster, `{"channel":"cli","chat_id":"terminal","text":"/revoke"}`); code != 200 || !strings.Contains(reply, "Laptop") {
		t.Fatalf("/revoke with the master key: %d %q", code, reply)
	}
	// A plain message from the chat-only phone is still conversation.
	if in, ok := d.deviceCantApprove(context.Background(), channels.Inbound{Text: "yes"}); ok {
		t.Fatalf("a message with no peer was stopped: %q", in)
	}
}

// Regression (devices merged with approvals): a device paired to talk but not
// to approve was stopped at "yes 1", but "yes, always" and an answer in its
// own words (which the model settles with resolve_approval) still decided.
func TestChatOnlyDeviceCantApproveInOtherWords(t *testing.T) {
	td := newTestDaemon(t, settler(map[string]string{"yep, send that one to him": `{"id":1,"decision":"approve"}`}))
	_, post := serveDaemonAPI(t, td.Daemon)
	_, chatOnly, _ := td.deviceStore().Add("Phone", devices.KindPWA, []devices.Scope{devices.View, devices.Chat}, "", "")
	say := func(tok, text string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"channel": "screen", "chat_id": "local", "text": text})
		code, reply := post(tok, string(b))
		if code != 200 {
			t.Fatalf("%q: %d %q", text, code, reply)
		}
		return reply
	}
	say(testMaster, "email the boss")
	for _, text := range []string{"yep, send that one to him", "yes always", "yes 1 and don't ask again"} {
		if got := say(chatOnly, text); len(td.ran()) != 0 || !strings.Contains(got, "paired to talk") {
			t.Fatalf("%q from a chat-only phone: reply %q, sent %v", text, got, td.ran())
		}
	}
	if ap, _ := td.store.GetApproval(context.Background(), 1); ap.Status != "pending" {
		t.Fatalf("#1 is %s", ap.Status)
	}
	if got := say(testMaster, "yes 1"); len(td.ran()) != 1 {
		t.Fatalf("this computer's yes: reply %q, sent %v", got, td.ran())
	}
}

// An approval decided on the screen's cards by a paired phone is kept (and
// audited) as that phone's decision, not as the owner at this computer's.
func TestApprovalFromAPairedPhoneNamesIt(t *testing.T) {
	td := newTestDaemon(t, butler)
	srv := api.New("127.0.0.1:0", testMaster, td.Daemon).WithScreen(td.Daemon)
	td.attachDevices(t.Context(), srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln, api.LoopbackOnly, "loopback"); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	phone, tok, err := td.deviceStore().Add("Akshay's iPhone", devices.KindPWA, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	td.owner(t, "email the boss")
	r, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/approvals/1/approve", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("approve: %d", resp.StatusCode)
	}
	ap, _ := td.store.GetApproval(context.Background(), 1)
	if ap.Status != "approved" || !strings.Contains(ap.DecidedBy, "Akshay's iPhone ["+phone.ID+"]") {
		t.Fatalf("#1 %s, decided by %q", ap.Status, ap.DecidedBy)
	}
	if b := screenDecider(context.Background()); b != (Decider{Method: "screen"}) {
		t.Fatalf("the tray's own screen decided as %+v", b)
	}

	// Decided in chat from the same phone, it is named the same way:
	// "yes 2", and an answer in its own words settled by resolve_approval.
	say := func(tok, text string) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"channel": "screen", "chat_id": "local", "text": text})
		r, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/message", bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%q: %d", text, resp.StatusCode)
		}
	}
	say(testMaster, "email the bank")
	say(tok, "yes 2")
	if ap, _ := td.store.GetApproval(context.Background(), 2); ap.Status != "approved" || !strings.Contains(ap.DecidedBy, "Akshay's iPhone ["+phone.ID+"]") {
		t.Fatalf("#2 %s in chat, decided by %q", ap.Status, ap.DecidedBy)
	}
	td.llm.brain = settler(map[string]string{"yep, send that one to him": `{"id":3,"decision":"approve"}`})
	say(testMaster, "email the landlord")
	say(tok, "yep, send that one to him")
	if ap, _ := td.store.GetApproval(context.Background(), 3); ap.Status != "approved" || !strings.Contains(ap.DecidedBy, "Akshay's iPhone ["+phone.ID+"]") || !strings.Contains(ap.DecidedBy, "reply") {
		t.Fatalf("#3 %s in its own words, decided by %q", ap.Status, ap.DecidedBy)
	}
	// This computer's own key is still the owner.
	say(testMaster, "email the plumber")
	say(testMaster, "yes 4")
	if ap, _ := td.store.GetApproval(context.Background(), 4); ap.Status != "approved" || ap.DecidedBy != "the owner (screen)" {
		t.Fatalf("#4 %s from this computer, decided by %q", ap.Status, ap.DecidedBy)
	}
}

// Regression (devices merged with approvals' "yes, always"): a bare yes from
// a device paired to talk was let through when nothing was pending, so once
// the owner's request was decided on the screen's cards, a chat-only phone's
// "yes" confirmed the owner's "Before I stop asking" check and turned on a
// standing permission.
func TestChatOnlyDeviceCantConfirmAnAlways(t *testing.T) {
	td := newTestDaemon(t, butler)
	_, post := serveDaemonAPI(t, td.Daemon)
	_, chatOnly, _ := td.deviceStore().Add("Phone", devices.KindPWA, []devices.Scope{devices.View, devices.Chat}, "", "")
	say := func(tok, text string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"channel": "screen", "chat_id": "local", "text": text})
		code, reply := post(tok, string(b))
		if code != 200 {
			t.Fatalf("%q: %d %q", text, code, reply)
		}
		return reply
	}
	say(testMaster, "email the boss")
	if got := say(testMaster, "yes, always"); !strings.Contains(got, "Before I stop asking") {
		t.Fatalf("the owner's yes, always: %q", got)
	}
	if _, err := td.DecideApproval(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	got := say(chatOnly, "yes")
	if !strings.Contains(got, "paired to talk") || len(td.Config().Autonomy.AlwaysAllow) != 0 {
		t.Fatalf("a chat-only phone's yes: %q, always_allow %v", got, td.Config().Autonomy.AlwaysAllow)
	}
	// Belt and braces: the check itself refuses a device without approve.
	c := td.conv("screen:local")
	ctx := api.WithPeer(context.Background(), api.Peer{Device: &devices.Device{ID: "p", Name: "Phone", Kind: devices.KindPWA, Scopes: []devices.Scope{devices.View, devices.Chat}}})
	in := channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "yes", IsOwner: true}
	if reply, _ := td.confirmAlways(ctx, c, in, alwaysOffer{id: 1, tool: "send", at: clock()}, true, nil); !strings.Contains(reply, "paired to talk") || len(td.Config().Autonomy.AlwaysAllow) != 0 {
		t.Fatalf("confirmAlways from a chat-only device: %q, always_allow %v", reply, td.Config().Autonomy.AlwaysAllow)
	}
	// The owner's own yes on this computer still confirms it.
	say(testMaster, "email the bank")
	say(testMaster, "yes, always")
	say(testMaster, "yes")
	if aa := td.Config().Autonomy.AlwaysAllow; len(aa) != 1 || aa[0] != "send" {
		t.Fatalf("the owner's own confirmation: always_allow %v", aa)
	}
}

// With nothing waiting and no check offered, a chat-only device's bare yes
// is conversation, as before.
func TestChatOnlyDevicesPlainYesIsConversation(t *testing.T) {
	td := newTestDaemon(t, butler)
	_, post := serveDaemonAPI(t, td.Daemon)
	_, chatOnly, _ := td.deviceStore().Add("Phone", devices.KindPWA, []devices.Scope{devices.View, devices.Chat}, "", "")
	code, reply := post(chatOnly, `{"channel":"screen","chat_id":"local","text":"yes"}`)
	if code != 200 || strings.Contains(reply, "paired to talk") {
		t.Fatalf("%d %q", code, reply)
	}
}

// Pairing or cutting off a device asks for a backup soon, so a restore never
// brings back a device the owner removed; a browser on this computer getting
// its own key doesn't.
func TestDeviceChangesAskForABackup(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	fresh := func() *backup.Scheduler {
		s := &backup.Scheduler{DataDir: t.TempDir(), Engine: func() (*backup.Engine, error) { return nil, nil }}
		d.backups = s
		return s
	}
	serveDaemonAPI(t, d)
	store := d.deviceStore()
	s := fresh()
	if _, _, err := store.AddLocal("Safari on this Mac"); err != nil || s.Pending() {
		t.Fatalf("a local browser's key asked for a backup (%v)", err)
	}
	phone, _, err := store.Add("Phone", devices.KindPWA, nil, "", "")
	if err != nil || !s.Pending() {
		t.Fatalf("pairing a phone didn't ask for a backup (%v)", err)
	}
	s = fresh()
	if _, err := store.Revoke(phone.ID); err != nil || !s.Pending() {
		t.Fatalf("revoking the phone didn't ask for a backup (%v)", err)
	}
}

func TestRevokeWithTheAPIOff(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	if got, _ := d.command(context.Background(), "cli:local", "/revoke abcd"); !strings.Contains(got, "switched off") {
		t.Fatalf("%q", got)
	}
}

func TestRevokeWithADamagedDevicesFile(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	quietDesktop(t)
	path := devices.Path(d.Config().DataDir)
	if err := os.MkdirAll(d.Config().DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.attachDevices(t.Context(), api.New("127.0.0.1:0", testMaster, d))
	got, _ := d.command(context.Background(), "cli:local", "/revoke abcd")
	if !strings.Contains(got, "couldn't read the list of paired devices") || !strings.Contains(got, path) || strings.Contains(got, "API is off") {
		t.Fatalf("%q", got)
	}
}

// The registry is let go when the twin's run ends.
func TestDeviceHubEndsWithTheRun(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	d.attachDevices(ctx, api.New("127.0.0.1:0", testMaster, d))
	if d.deviceHub() == nil {
		t.Fatal("no hub while running")
	}
	cancel()
	for range 100 {
		if d.deviceHub() == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the hub outlived the run")
}

func quietDesktop(t *testing.T) {
	prev := desktopNotify
	desktopNotify = func(string, string) error { return nil }
	t.Cleanup(func() { desktopNotify = prev })
}

// A device paired to talk but not to approve may say stop (it is a chat
// control), but its stop doesn't turn down what the stopped turn had asked
// the owner: that would be deciding requests by another name.
func TestChatOnlyDevicesStopLeavesRequestsWaiting(t *testing.T) {
	td, started, ended := newBooker(t)
	_, post := serveDaemonAPI(t, td.Daemon)
	_, chatOnly, _ := td.deviceStore().Add("Phone", devices.KindPWA, []devices.Scope{devices.View, devices.Chat}, "", "")
	first := make(chan string, 1)
	go func() {
		_, reply := post(testMaster, `{"channel":"screen","chat_id":"local","text":"book and email the boss"}`)
		first <- reply
	}()
	waitStarted(t, started)
	code, got := post(chatOnly, `{"channel":"screen","chat_id":"local","text":"stop"}`)
	if code != 200 || !strings.Contains(got, "stopped") {
		t.Fatalf("the phone's stop: %d %q", code, got)
	}
	waitEnded(t, ended)
	if reply := <-first; strings.Contains(reply, "dropped the request") {
		t.Fatalf("the stopped turn says %q", reply)
	}
	if ap, _ := td.store.GetApproval(context.Background(), 1); ap.Status != "pending" {
		t.Fatalf("a chat-only phone's stop decided #1: %s by %q", ap.Status, ap.DecidedBy)
	}
	// This computer's own stop still turns down what it stops.
	go func() {
		_, reply := post(testMaster, `{"channel":"screen","chat_id":"local","text":"book and email the bank"}`)
		first <- reply
	}()
	waitStarted(t, started)
	post(testMaster, `{"channel":"screen","chat_id":"local","text":"stop"}`)
	waitEnded(t, ended)
	<-first
	if ap, _ := td.store.GetApproval(context.Background(), 2); ap.Status != "denied" {
		t.Fatalf("this computer's stop left #2 %s", ap.Status)
	}
}

// Owner commands follow the device's scopes: a device paired to talk can't
// clear the conversation every screen shares, reload protocols or retry a
// task, and one without view can't read the audit log or spending.
func TestOwnerCommandsFollowTheDevicesScopes(t *testing.T) {
	td := newTestDaemon(t, butler)
	_, post := serveDaemonAPI(t, td.Daemon)
	_, talker, _ := td.deviceStore().Add("Phone", devices.KindPWA, []devices.Scope{devices.View, devices.Chat}, "", "")
	_, blind, _ := td.deviceStore().Add("Speaker", devices.KindCLI, []devices.Scope{devices.Chat}, "", "")
	_, full, _ := td.deviceStore().Add("Tablet", devices.KindPWA, nil, "", "")
	td.owner(t, "hello") // something to forget
	send := func(tok, text string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"channel": "screen", "chat_id": "local", "text": text})
		code, reply := post(tok, string(b))
		if code != 200 {
			t.Fatalf("%q: %d %q", text, code, reply)
		}
		return reply
	}
	send(testMaster, "remember this line")
	for _, cmd := range []string{"/forget", "/reload", "/tasks retry abc"} {
		if got := send(talker, cmd); !strings.Contains(got, "paired to talk") {
			t.Errorf("%s from a chat-only phone: %q", cmd, got)
		}
	}
	if h, _ := td.store.History(context.Background(), "screen:local", 10); len(h) == 0 {
		t.Fatal("a chat-only phone cleared the shared conversation")
	}
	for _, cmd := range []string{"/status", "/audit", "/spend", "/tasks", "/pending", "/help"} {
		if got := send(talker, cmd); strings.Contains(got, "paired to talk") || strings.Contains(got, "without view") {
			t.Errorf("%s from a phone with view: %q", cmd, got)
		}
	}
	for _, cmd := range []string{"/audit", "/spend", "/status"} {
		if got := send(blind, cmd); !strings.Contains(got, "without view") {
			t.Errorf("%s from a device without view: %q", cmd, got)
		}
	}
	if got := send(full, "/forget"); !strings.Contains(got, "Conversation cleared") {
		t.Fatalf("/forget from a device that may approve: %q", got)
	}
	if got := send(testMaster, "/reload"); !strings.Contains(got, "Reloaded") {
		t.Fatalf("/reload from this computer: %q", got)
	}
	// The twin's own channels carry no peer: the owner's chat app may do all of it.
	if got, _ := td.command(context.Background(), ownerKey, "/forget"); !strings.Contains(got, "Conversation cleared") {
		t.Fatalf("/forget from the owner's chat app: %q", got)
	}
}

// A device paired to talk may answer a task's question with a bare yes even
// while a request waits somewhere: the twin asked it the task's question,
// not about the request. What the twin asked about a request still isn't
// its to answer.
func TestChatOnlyDeviceAnswersATasksQuestion(t *testing.T) {
	td := newTestDaemon(t, dinner)
	_, post := serveDaemonAPI(t, td.Daemon)
	_, chatOnly, _ := td.deviceStore().Add("Phone", devices.KindPWA, []devices.Scope{devices.View, devices.Chat}, "", "")
	td.owner(t, "email the boss") // #1 waits in the owner's Telegram chat
	task, err := td.tasks.Start(context.Background(), "screen:local", "Dinner", "book a table")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the task to ask", func() bool {
		cur, ok := td.taskByID(task.ID)
		return ok && cur.Status == tasks.WaitingUser
	})
	code, got := post(chatOnly, `{"channel":"screen","chat_id":"local","text":"yes"}`)
	if code != 200 || got != "Thanks. Carrying on with Dinner." {
		t.Fatalf("a chat-only phone's answer to the task: %d %q", code, got)
	}
	if ap, _ := td.store.GetApproval(context.Background(), 1); ap.Status != "pending" {
		t.Fatalf("#1 %s", ap.Status)
	}
}

// Every paired screen talks in one conversation (screen:local), so what one
// phone says there must show on the others: a turn typed on a screen is
// published marked with the page it came from (which skips its own lines).
// A screen that sends no page id isn't echoed, as before.
func TestAScreensExchangeShowsOnTheOtherScreens(t *testing.T) {
	td := newTestDaemon(t, butler)
	seen := listen(t, td.bus)
	in := channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "what's up?", IsOwner: true}
	if _, err := td.MessageEvents(api.WithClient(context.Background(), "phoneA"), in, agent.Events{}); err != nil {
		t.Fatal(err)
	}
	in.Text = "and now?"
	if _, err := td.MessageEvents(context.Background(), in, agent.Events{}); err != nil {
		t.Fatal(err)
	}
	var heard, said int
	for _, ev := range seen() {
		from, _ := ev.Data.(map[string]string)
		switch {
		case ev.Kind == "heard" && ev.Text == "what's up?" && from["origin"] == "phoneA":
			heard++
		case ev.Kind == "said" && ev.Text == "Heard: what's up?" && from["origin"] == "phoneA":
			said++
		case (ev.Kind == "heard" || ev.Kind == "said") && strings.Contains(ev.Text, "and now?"):
			t.Fatalf("a screen that sent no page id was echoed: %+v", ev)
		}
	}
	if heard != 1 || said != 1 {
		t.Fatalf("heard %d, said %d, want one each", heard, said)
	}
	if recent := td.bus.Recent(); !slices.ContainsFunc(recent, func(e events.Event) bool { return e.Kind == "heard" && e.Text == "what's up?" }) {
		t.Fatal("a screen opened later wouldn't see the exchange")
	}
}
