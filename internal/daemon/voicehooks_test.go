package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// spoken sends text as the owner speaking to the voice channel.
func (td *testDaemon) spoken(t *testing.T, text string) string {
	t.Helper()
	reply, err := td.message(context.Background(), channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: text, IsOwner: true}, agent.Events{})
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	return reply
}

// Anyone within earshot could approve a dangerous action by voice: a
// guest, the television. Now a spoken yes to one is refused, and the
// request goes to the owner's phone, where their own yes decides it.
func TestDangerousActionIsNotApprovedOutLoud(t *testing.T) {
	td := newTestDaemon(t, butler)
	var paid []string
	var mu sync.Mutex
	td.agent.Tools().Register(tools.New("pay", "pay someone", tools.Schema(nil), tools.RiskDangerous,
		func(_ context.Context, c tools.Call) (string, error) {
			mu.Lock()
			paid = append(paid, string(c.Input))
			mu.Unlock()
			return "paid", nil
		}))
	id := raise(t, td, "voice:local", "pay", `{"to":"Acme","amount":40}`)
	td.conv(voiceChat).noteReply(voiceChat, "Shall I pay Acme forty pounds?", true)

	for _, yes := range []string{"Yes.", "yes 1", "Go ahead."} {
		reply := td.spoken(t, yes)
		if !strings.Contains(reply, "too big to take a yes out loud") || !strings.Contains(reply, "Telegram") || !!strings.Contains(reply, "number 1") {
			t.Fatalf("%q: reply %q", yes, reply)
		}
	}
	if ap, _ := td.store.GetApproval(context.Background(), id); ap.Status != "pending" {
		t.Fatalf("a spoken yes decided it: %s", ap.Status)
	}
	mu.Lock()
	n := len(paid)
	mu.Unlock()
	if n != 0 {
		t.Fatal("a spoken yes paid")
	}
	// The phone heard about it once, with the number to reply with.
	msgs := td.ch.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], `"yes 1"`) || !strings.Contains(msgs[0], "out loud") {
		t.Fatalf("phone got %q", msgs)
	}
	// The owner's own yes, where it reached them, decides it.
	td.owner(t, "yes 1")
	if ap, _ := td.store.GetApproval(context.Background(), id); ap.Status != "approved" {
		t.Fatalf("the owner's yes on the phone didn't decide it: %s", ap.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paid) != 1 {
		t.Fatalf("paid %d times", len(paid))
	}
}

// A no is always fine out loud, and so is a yes to something that isn't
// dangerous.
func TestSpokenNoAndLesserYesStillWork(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.agent.Tools().Register(tools.New("pay", "pay", tools.Schema(nil), tools.RiskDangerous,
		func(context.Context, tools.Call) (string, error) { return "paid", nil }))
	id := raise(t, td, "voice:local", "pay", `{}`)
	if reply := td.spoken(t, "no 1"); strings.Contains(reply, "out loud") {
		t.Fatalf("a no was refused: %q", reply)
	}
	if ap, _ := td.store.GetApproval(context.Background(), id); ap.Status != "denied" {
		t.Fatalf("status %s", ap.Status)
	}
	id = raise(t, td, "voice:local", "send", `{"to":"boss"}`)
	if reply := td.spoken(t, "yes 2"); reply != "Sent." {
		t.Fatalf("a write-level yes out loud: %q", reply)
	}
	if ap, _ := td.store.GetApproval(context.Background(), id); ap.Status != "approved" {
		t.Fatalf("status %s", ap.Status)
	}
}

// A request asked as dangerous stays one out loud even if its tool now says
// less: approvals keep the risk they were asked at, and requests from before
// that count as dangerous.
func TestSpokenYesGoesByTheRiskItWasAskedAt(t *testing.T) {
	td := newTestDaemon(t, butler)
	id, err := td.store.CreateApproval(context.Background(), "voice:local", "send", []byte(`{"to":"boss"}`), "send(to=boss)", tools.RiskDangerous)
	if err != nil {
		t.Fatal(err)
	}
	if reply := td.spoken(t, "yes 1"); !strings.Contains(reply, "too big to take a yes out loud") {
		t.Fatalf("reply %q", reply)
	}
	if ap, _ := td.store.GetApproval(context.Background(), id); ap.Status != "pending" || len(td.ran()) != 0 {
		t.Fatalf("a spoken yes decided it: %s, sent %v", ap.Status, td.ran())
	}
}

// With no phone to send it to, the presence screen is where to approve it.
func TestDangerousSpokenYesWithNoPhonePointsAtTheScreen(t *testing.T) {
	td := newTestDaemon(t, butler)
	delete(td.channels, "telegram")
	prev := desktopNotify
	desktopNotify = func(_, _ string) error { return nil }
	t.Cleanup(func() { desktopNotify = prev })
	td.agent.Tools().Register(tools.New("run_shell", "run", tools.Schema(nil), tools.RiskDangerous,
		func(context.Context, tools.Call) (string, error) { return "ran", nil }))
	raise(t, td, "voice:local", "run_shell", `{"cmd":"rm -rf ~/tmp"}`)
	if reply := td.spoken(t, "yes 1"); !strings.Contains(reply, "Approve it on the screen") || strings.Contains(reply, "sent it") {
		t.Fatalf("reply %q", reply)
	}
}

// downChannel is a phone channel whose messages don't go through.
type downChannel struct{ *fakeChannel }

func (downChannel) Send(context.Context, string, string) error { return errors.New("network down") }

// A spoken yes to a dangerous action while the phone channel was briefly
// down marked the request as sent, so every later spoken yes only pointed
// at the presence screen and the phone was never tried again. What was
// noted about decided requests is dropped.
func TestSpokenYesTriesThePhoneAgainAfterItWasDown(t *testing.T) {
	td := newTestDaemon(t, butler)
	prev := desktopNotify
	desktopNotify = func(_, _ string) error { return nil }
	t.Cleanup(func() { desktopNotify = prev })
	td.agent.Tools().Register(tools.New("pay", "pay", tools.Schema(nil), tools.RiskDangerous,
		func(context.Context, tools.Call) (string, error) { return "paid", nil }))
	ctx := context.Background()
	id := raise(t, td, "voice:local", "pay", `{"to":"Acme"}`)

	td.chmu.Lock()
	td.channels["telegram"] = downChannel{td.ch}
	td.chmu.Unlock()
	if reply := td.spoken(t, "yes 1"); !strings.Contains(reply, "on the screen") {
		t.Fatalf("phone down: %q", reply)
	}
	td.chmu.Lock()
	td.channels["telegram"] = td.ch
	td.chmu.Unlock()
	if reply := td.spoken(t, "yes 1"); !strings.Contains(reply, "Telegram") {
		t.Fatalf("phone back: %q", reply)
	}
	if msgs := td.ch.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], `"yes 1"`) {
		t.Fatalf("phone got %q", msgs)
	}
	td.spoken(t, "yes 1")
	if msgs := td.ch.messages(); len(msgs) != 1 {
		t.Fatalf("the phone was told again: %q", msgs)
	}

	// Decided on the phone: nothing about it is kept once the next one comes.
	td.owner(t, "yes 1")
	if ap, _ := td.store.GetApproval(ctx, id); ap.Status != "approved" {
		t.Fatalf("status %s", ap.Status)
	}
	raise(t, td, "voice:local", "pay", `{"to":"Beta"}`)
	td.spoken(t, "yes 2")
	kept, _ := td.store.Get(ctx, aloudKey)
	if strings.Contains(kept, `"1"`) || !strings.Contains(kept, `"2"`) {
		t.Fatalf("kept %s", kept)
	}
}

// The voice channel learns from the daemon whether an approval waits (so a
// bare "yes" in its follow-up window is kept), and its trouble reaches the
// owner.
func TestVoiceChannelIsWiredToTheDaemon(t *testing.T) {
	td := newTestDaemon(t, butler)
	var shown []string
	prev := desktopNotify
	desktopNotify = func(_, body string) error { shown = append(shown, body); return nil }
	t.Cleanup(func() { desktopNotify = prev })
	seen := listen(t, td.bus)
	ch := td.newVoiceChannel(td.Config().Channels.Voice)
	if ch.Pending == nil || ch.Pending() {
		t.Fatal("nothing waits yet")
	}
	raise(t, td, "voice:local", "send", `{"to":"boss"}`)
	if !ch.Pending() {
		t.Fatal("an approval waits on the voice chat")
	}
	ch.OnProblem("I'm not getting any sound from the microphone.")
	if len(shown) != 1 {
		t.Fatalf("desktop notifications %q", shown)
	}
	found := false
	for _, ev := range seen() {
		if ev.Kind == "notice" && strings.Contains(ev.Text, "microphone") {
			found = true
		}
	}
	if !found {
		t.Fatal("the presence screen wasn't told")
	}
}

// `mirrin voice setup` (often run from the menu bar) saved new settings the
// running twin never picked up until a restart.
func TestVoiceSetupIsPickedUpWithoutARestart(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	disk, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "ggml-small.bin")
	disk.Channels.Voice.WhisperModel = model
	disk.Channels.Voice.Language = "fr"
	if err := disk.Save(); err != nil {
		t.Fatal(err)
	}
	if err := d.RunJob(context.Background(), "voice"); err != nil {
		t.Fatal(err)
	}
	if v := d.Config().Channels.Voice; v.WhisperModel != model || v.Language != "fr" {
		t.Fatalf("the running twin still has %q (%q)", v.WhisperModel, v.Language)
	}
}

// A spoken stop reaches the voice turn it interrupts at once (wireVoice's
// OnStop), as "stop" does in chat: the turn is cut short, says so out loud,
// and what it had asked for is turned down.
func TestSpokenStopCutsTheVoiceTurn(t *testing.T) {
	td, started, ended := newBooker(t)
	ch := voice.New(td.Config().Channels.Voice, "Mirrin", td.Config().DataDir)
	td.wireVoice(ch)
	if ch.OnStop == nil {
		t.Fatal("the voice channel has no way to stop a turn")
	}
	done := make(chan string, 1)
	go func() {
		reply, _ := td.message(context.Background(), channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: "book and email the boss", IsOwner: true}, agent.Events{})
		done <- reply
	}()
	waitStarted(t, started)
	ch.OnStop()
	waitEnded(t, ended)
	if got := <-done; got != "OK, stopped." {
		t.Fatalf("the stopped voice turn said %q", got)
	}
	if ap, _ := td.store.GetApproval(context.Background(), 1); ap.Status != "denied" {
		t.Fatalf("#1 %s", ap.Status)
	}
	ch.OnStop() // nothing running: nothing happens
}

func TestSiteNamesAreSaidAsPeopleSayThem(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.uniqlo.com/au/en/cart": "Uniqlo", "https://booking.jetstar.com/x": "Booking",
		"about:blank": "", "http://127.0.0.1:7742/ui": "", "": "",
	} {
		if got := siteName(in); got != want {
			t.Errorf("siteName(%q) = %q, want %q", in, got, want)
		}
	}
}
