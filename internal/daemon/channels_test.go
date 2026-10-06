package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/skills/phone"
	"github.com/MavrkAI/Mirrin/internal/tasks"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// newChannelsDaemon is a daemon with memory and an event bus but no model and no
// real channels: enough to exercise delivery and channel supervision.
func newChannelsDaemon(t *testing.T) *Daemon {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = false
	cfg.DataDir = filepath.Join(home, "data")
	store, err := memory.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	// The first hello on a new channel has tests of its own (firstchannel_test.go).
	_ = store.Set(context.Background(), firstHelloKey, "already")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		time.Sleep(20 * time.Millisecond) // let supervisors wind down before the store closes
		store.Close()
	})
	return &Daemon{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: store, loc: time.Local,
		channels: map[string]channels.Channel{}, bus: events.New(), runCtx: ctx}
}

// fastRestarts shortens supervision delays for the test and swaps the rebuild step.
func fastRestarts(t *testing.T, next func() channels.Channel) {
	t.Helper()
	oldMin, oldMax, oldRebuild := restartMin, restartMax, rebuild
	restartMin, restartMax = 10*time.Millisecond, 40*time.Millisecond
	rebuild = func(*Daemon, string, *config.Config) channels.Channel { return next() }
	t.Cleanup(func() { restartMin, restartMax, rebuild = oldMin, oldMax, oldRebuild })
}

// fastRetry shortens the pause before a failed send is tried again.
func fastRetry(t *testing.T) {
	t.Helper()
	old := sendRetry
	sendRetry = time.Millisecond
	t.Cleanup(func() { sendRetry = old })
}

type fakeTransport struct {
	channels.Tracker
	name, owner string
	start       func(f *fakeTransport, ctx context.Context, h channels.Handler) error
	send        func(chatID, text string) error // overrides sendErr
	sendErr     error
	hold        string        // sends to this chat wait for release
	release     chan struct{} // closed to let held sends through
	typing      atomic.Int32
	closed      atomic.Int32

	mu   sync.Mutex
	sent []string
}

func (f *fakeTransport) Name() string        { return f.name }
func (f *fakeTransport) OwnerChatID() string { return f.owner }

func (f *fakeTransport) Start(ctx context.Context, h channels.Handler) error {
	if f.start != nil {
		return f.start(f, ctx, h)
	}
	f.Up()
	<-ctx.Done()
	return nil
}

func (f *fakeTransport) Send(_ context.Context, chatID, text string) error {
	if f.hold == chatID && f.release != nil {
		<-f.release
	}
	if f.send != nil {
		return f.send(chatID, text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, chatID+": "+text)
	return nil
}

func (f *fakeTransport) Close() error {
	f.closed.Add(1)
	return nil
}

func (f *fakeTransport) Typing(context.Context, string) (time.Duration, error) {
	f.typing.Add(1)
	return time.Hour, nil
}

func (f *fakeTransport) Sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var offline = &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Name: "api.telegram.org", Err: "no such host"}}

// The service starts at login before Wi-Fi is up: the first connection
// fails, and the channel must come back by itself.
func TestChannelRecoversWhenFirstConnectFails(t *testing.T) {
	d := newChannelsDaemon(t)
	var attempts atomic.Int32
	mk := func() channels.Channel {
		return &fakeTransport{name: "telegram", owner: "42", start: func(f *fakeTransport, ctx context.Context, _ channels.Handler) error {
			if attempts.Add(1) == 1 {
				return offline
			}
			f.Up()
			<-ctx.Done()
			return nil
		}}
	}
	fastRestarts(t, mk)
	d.launch("telegram", mk())
	waitFor(t, "telegram reconnects", func() bool { st, _ := d.channelStatus("telegram"); return st.State == channels.Connected })
	if attempts.Load() != 2 {
		t.Fatalf("attempts: %d", attempts.Load())
	}
	if d.ownerChatKey() != "telegram:42" {
		t.Fatalf("owner key %q", d.ownerChatKey())
	}
}

func TestFatalErrorStopsTheChannelAndSaysWhy(t *testing.T) {
	d := newChannelsDaemon(t)
	fastRestarts(t, func() channels.Channel {
		t.Error("a rejected token must not be retried")
		return nil
	})
	d.launch("telegram", &fakeTransport{name: "telegram", start: func(*fakeTransport, context.Context, channels.Handler) error {
		return channels.Fatal(errors.New("Telegram doesn't accept this bot token"))
	}})
	waitFor(t, "channel stops", func() bool { _, running := d.channelStatus("telegram"); return !running })
	time.Sleep(50 * time.Millisecond)
	st, _ := d.channelStatus("telegram")
	up, detail := d.channelHealth("telegram")
	if st.State != channels.Failed || up || detail != "Telegram doesn't accept this bot token" {
		t.Fatalf("status %+v, health %v %q", st, up, detail)
	}
}

// A channel that is running but can't get through (Discord without its
// intent, Slack's socket failing) must not show as connected.
func TestBrokenChannelIsNotReportedConnected(t *testing.T) {
	d := newChannelsDaemon(t)
	d.launch("slack", &fakeTransport{name: "slack", start: func(f *fakeTransport, ctx context.Context, _ channels.Handler) error {
		f.Retrying(errors.New("slack socket: server disconnect"))
		<-ctx.Done()
		return nil
	}})
	waitFor(t, "status reported", func() bool { st, _ := d.channelStatus("slack"); return st.Err != "" })
	for _, st := range d.ConnectorStates(context.Background()) {
		if st.Name != "slack" {
			continue
		}
		if st.Running || !strings.Contains(st.Error, "server disconnect") {
			t.Fatalf("Channels page shows %+v", st)
		}
	}
	if up, _ := d.channelHealth("slack"); up {
		t.Fatal("health check counts a broken channel as up")
	}
}

func TestStoppingDuringBackoffLeavesNothingBehind(t *testing.T) {
	d := newChannelsDaemon(t)
	var rebuilt atomic.Bool
	fastRestarts(t, func() channels.Channel { rebuilt.Store(true); return &fakeTransport{name: "telegram"} })
	restartMin = 100 * time.Millisecond
	d.launch("telegram", &fakeTransport{name: "telegram", start: func(*fakeTransport, context.Context, channels.Handler) error { return offline }})
	waitFor(t, "first attempt fails", func() bool { _, running := d.channelStatus("telegram"); return !running })
	d.StopChannel("telegram")
	time.Sleep(250 * time.Millisecond)
	if _, ok := d.channel("telegram"); ok || rebuilt.Load() {
		t.Fatal("a stopped channel came back")
	}
}

// A reminder for the owner goes out on another channel when the preferred
// one refuses it (WhatsApp unlinked, a token revoked), instead of failing.
func TestOwnerMessagesFallBackToAnotherChannel(t *testing.T) {
	d := newChannelsDaemon(t)
	fastRetry(t)
	wa := &fakeTransport{name: "whatsapp", owner: "61400@s.whatsapp.net", sendErr: errors.New("whatsapp not connected")}
	tg := &fakeTransport{name: "telegram", owner: "42"}
	sig := &fakeTransport{name: "signal", owner: "+61400"}
	wa.Up()
	tg.Up()
	sig.Up()
	d.channels = map[string]channels.Channel{"whatsapp": wa, "telegram": tg, "signal": sig}
	ctx := context.Background()

	if err := d.Send(ctx, "whatsapp:61400@s.whatsapp.net", "Reminder: leave for the airport"); err != nil {
		t.Fatal(err)
	}
	if got := tg.Sent(); len(got) != 1 || got[0] != "42: Reminder: leave for the airport" {
		t.Fatalf("telegram got %v", got)
	}
	// A channel that is reconnecting is skipped.
	tg.Retrying(errors.New("poll failed"))
	if err := d.Send(ctx, "whatsapp:61400@s.whatsapp.net", "Approval needed: yes 3?"); err != nil {
		t.Fatal(err)
	}
	if got := sig.Sent(); len(got) != 1 || !strings.HasPrefix(got[0], "+61400: Approval needed") {
		t.Fatalf("signal got %v", got)
	}
	// A reply meant for someone else is never redirected to the owner.
	if err := d.Send(ctx, "whatsapp:999@s.whatsapp.net", "See you at 8"); err == nil {
		t.Fatal("expected the failure to be reported")
	}
	if len(tg.Sent()) != 1 || len(sig.Sent()) != 1 {
		t.Fatal("a stranger's reply was sent to the owner")
	}
}

// While one turn runs for minutes, the transport keeps reading: the next
// message in that chat waits its turn instead of going stale, another chat is
// answered meanwhile, and "typing…" shows while the twin works.
func TestLongTurnDoesNotHoldUpOtherMessages(t *testing.T) {
	d := newChannelsDaemon(t)
	d.paused.Store(true) // replies without a model
	release := make(chan struct{})
	delivered := make(chan struct{})
	ch := &fakeTransport{name: "telegram", owner: "42", hold: "42", release: release}
	ch.start = func(f *fakeTransport, ctx context.Context, h channels.Handler) error {
		f.Up()
		h(ctx, channels.Inbound{Channel: "telegram", ChatID: "42", Text: "book the usual table", IsOwner: true})
		h(ctx, channels.Inbound{Channel: "telegram", ChatID: "42", Text: "also add milk", IsOwner: true})
		h(ctx, channels.Inbound{Channel: "telegram", ChatID: "7", Text: "hello"})
		close(delivered)
		<-ctx.Done()
		return nil
	}
	d.launch("telegram", ch)
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("the receive loop was blocked by a running turn")
	}
	waitFor(t, "other chat answered", func() bool { return len(ch.Sent()) == 1 })
	if !strings.HasPrefix(ch.Sent()[0], "7: ") {
		t.Fatalf("sent %v", ch.Sent())
	}
	if ch.typing.Load() == 0 {
		t.Fatal("no typing indicator while working")
	}
	close(release)
	waitFor(t, "held chat answered twice", func() bool { return len(ch.Sent()) == 3 })
}

type recordingProvider struct {
	name   string
	mu     sync.Mutex
	effort []string
}

func (p *recordingProvider) Name() string { return p.name }

func (p *recordingProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.effort = append(p.effort, req.Effort)
	p.mu.Unlock()
	return &llm.Response{Message: llm.Text(llm.RoleAssistant, "Friday at seven works. Thank you, goodbye. [HANGUP]"), StopReason: llm.StopEndTurn}, nil
}

func (p *recordingProvider) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.effort...)
}

// Twilio waits about 12 s for each answer on a live call, so turns must use
// the voice model and effort, not the main model at high effort.
func TestPhoneTurnsUseTheVoiceModel(t *testing.T) {
	d := newChannelsDaemon(t)
	main, fast := &recordingProvider{name: "main"}, &recordingProvider{name: "voice"}
	d.agent = agent.New(d.cfg, main, d.store, tools.NewRegistry(), approvals.New(d.cfg.Autonomy), d.log)
	d.agent.SetVoiceProvider(fast)
	if _, err := d.phoneTurn(context.Background(), "123", "book a table for two", "We have Friday at seven"); err != nil {
		t.Fatal(err)
	}
	if len(main.calls()) != 0 || len(fast.calls()) != 1 || fast.calls()[0] != d.cfg.LLM.VoiceEffort {
		t.Fatalf("main %v, voice %v", main.calls(), fast.calls())
	}
	if !strings.Contains(phoneKey("123"), "#") {
		t.Fatal("call conversations must stay scratch (pruned) conversations")
	}
}

// When a call ends the owner hears how it went, in the chat that asked for it.
func TestCallOutcomeIsReported(t *testing.T) {
	d := newChannelsDaemon(t)
	tg := &fakeTransport{name: "telegram", owner: "42"}
	tg.Up()
	d.channels = map[string]channels.Channel{"telegram": tg}
	d.agent = agent.New(d.cfg, &recordingProvider{name: "main"}, d.store, tools.NewRegistry(), approvals.New(d.cfg.Autonomy), d.log)

	d.phoneEnded(phone.Ended{To: "+61299990000", Status: "busy", ChatKey: "telegram:42"})
	d.phoneEnded(phone.Ended{To: "+61299990000", Status: "completed", ChatKey: "whatsapp:61400@s.whatsapp.net#task-1",
		Transcript: []string{"twin: Hi, booking for two?", "them: Friday at seven", "twin: Perfect, goodbye."}})
	got := tg.Sent()
	if len(got) != 2 || !strings.Contains(got[0], "busy") || !strings.Contains(got[1], "Friday at seven") {
		t.Fatalf("owner was told %v", got)
	}
}

// Offline for hours: every failed attempt is a fresh instance, and each one
// must let go of what it opened (WhatsApp's store, say) before the next is
// built.
func TestEachAttemptReleasesWhatItHeld(t *testing.T) {
	d := newChannelsDaemon(t)
	var mu sync.Mutex
	var made []*fakeTransport
	mk := func() channels.Channel {
		mu.Lock()
		defer mu.Unlock()
		f := &fakeTransport{name: "signal", start: func(f *fakeTransport, ctx context.Context, _ channels.Handler) error {
			mu.Lock()
			n := len(made)
			mu.Unlock()
			if n < 4 {
				return offline
			}
			<-ctx.Done()
			return nil
		}}
		made = append(made, f)
		return f
	}
	fastRestarts(t, mk)
	d.launch("signal", mk())
	waitFor(t, "a few attempts", func() bool { mu.Lock(); defer mu.Unlock(); return len(made) >= 4 })
	waitFor(t, "the connecting one runs", func() bool { _, running := d.channelStatus("signal"); return running })
	mu.Lock()
	for i, f := range made[:len(made)-1] {
		if f.closed.Load() != 1 {
			t.Errorf("attempt %d closed %d times", i, f.closed.Load())
		}
	}
	last := made[len(made)-1]
	mu.Unlock()
	if last.closed.Load() != 0 {
		t.Fatal("the running instance was closed")
	}
	d.StopChannel("signal")
	waitFor(t, "stopped instance closed", func() bool { return last.closed.Load() == 1 })
}

// Two Connect clicks at once: the first supervisor must stop, not keep
// restarting a second copy (two Telegram pollers fight with 409s forever).
func TestStartingTwiceLeavesOneRunning(t *testing.T) {
	d := newChannelsDaemon(t)
	var starts atomic.Int32
	fastRestarts(t, func() channels.Channel {
		starts.Add(1)
		return &fakeTransport{name: "telegram", owner: "42"}
	})
	first := &fakeTransport{name: "telegram", owner: "42"}
	second := &fakeTransport{name: "telegram", owner: "42"}
	d.launch("telegram", first)
	d.launch("telegram", second)
	waitFor(t, "first one stops", func() bool { return first.closed.Load() == 1 })
	time.Sleep(100 * time.Millisecond)
	if ch, _ := d.channel("telegram"); ch != second || starts.Load() != 0 || second.closed.Load() != 0 {
		t.Fatalf("running %p (want %p), restarts %d", ch, second, starts.Load())
	}
}

// Replies to other people never reach the owner instead, even while their
// channel is down; a reply the owner's channel refused is not read out loud
// by the voice channel; a reminder with nowhere else to go still is.
func TestFallbackOnlyReachesTheOwnerAndOnlyWhereItShould(t *testing.T) {
	d := newChannelsDaemon(t)
	fastRetry(t)
	tg := &fakeTransport{name: "telegram", owner: "42", sendErr: errors.New("telegram: 502 Bad Gateway")}
	voice := &fakeTransport{name: "voice", owner: "local"}
	tg.Up()
	d.channels = map[string]channels.Channel{"telegram": tg, "voice": voice}
	stranger := channels.Inbound{Channel: "whatsapp", ChatID: "999@s.whatsapp.net", Text: "hi"}
	ctx := context.WithValue(context.Background(), answeringKey{}, stranger)

	if err := d.Send(ctx, stranger.Key(), "Hello! He'll get back to you."); err == nil {
		t.Fatal("expected the failure to be reported")
	}
	if err := d.Send(context.Background(), "telegram:42", "Here's your summary."); err != nil {
		t.Logf("fell back to a notification: %v", err)
	}
	if got := voice.Sent(); len(got) != 0 {
		t.Fatalf("voice said %v", got)
	}
	// WhatsApp isn't running at all: a reminder goes wherever the owner is.
	tg.sendErr = nil
	delete(d.channels, "telegram")
	if err := d.Send(context.Background(), "whatsapp:61400@s.whatsapp.net", "Reminder: call mum"); err != nil {
		t.Fatal(err)
	}
	if got := voice.Sent(); len(got) != 1 || got[0] != "local: Reminder: call mum" {
		t.Fatalf("voice said %v", got)
	}
}

// A long message that fails part-way is retried and then forwarded with only
// the rest, so the owner doesn't read the first part twice.
func TestPartlySentMessageIsNotRepeated(t *testing.T) {
	d := newChannelsDaemon(t)
	fastRetry(t)
	var tgGot []string
	tg := &fakeTransport{name: "telegram", owner: "42"}
	tg.send = func(_, text string) error {
		return channels.SendParts(text, 12, func(part string) error {
			if len(tgGot) >= 1 {
				return errors.New("telegram: 502 Bad Gateway")
			}
			tgGot = append(tgGot, part)
			return nil
		})
	}
	sig := &fakeTransport{name: "signal", owner: "+61400"}
	tg.Up()
	sig.Up()
	d.channels = map[string]channels.Channel{"telegram": tg, "signal": sig}
	if err := d.Send(context.Background(), "telegram:42", "first part\nsecond bit\nthird bit"); err != nil {
		t.Fatal(err)
	}
	if got := sig.Sent(); len(tgGot) != 1 || len(got) != 1 || got[0] != "+61400: second bit\nthird bit" {
		t.Fatalf("telegram %q, signal %q", tgGot, got)
	}
}

// An approval asked on WhatsApp that reached the owner on Signal (WhatsApp
// couldn't deliver it) can be answered on Signal.
func TestForwardedApprovalCanBeAnsweredWhereItArrived(t *testing.T) {
	d := newChannelsDaemon(t)
	fastRetry(t)
	d.agent = agent.New(d.cfg, &recordingProvider{name: "main"}, d.store, tools.NewRegistry(), approvals.New(d.cfg.Autonomy), d.log)
	d.tasks = tasks.New(context.Background(), tasks.Deps{})
	wa := &fakeTransport{name: "whatsapp", owner: "61400@s.whatsapp.net", sendErr: errors.New("whatsapp not connected")}
	sig := &fakeTransport{name: "signal", owner: "+61400"}
	irc := &fakeTransport{name: "irc", owner: "akshay"}
	wa.Up()
	sig.Up()
	d.channels = map[string]channels.Channel{"whatsapp": wa, "signal": sig, "irc": irc}
	ctx := context.Background()
	waKey := "whatsapp:61400@s.whatsapp.net"
	id, err := d.store.CreateApproval(ctx, waKey, "send_email", nil, "Email the landlord")
	if err != nil {
		t.Fatal(err)
	}
	ask := fmt.Sprintf("Approval needed: Email the landlord. Reply \"yes %d\" or \"no %d\".", id, id)
	if err := d.Send(ctx, waKey, ask); err != nil || len(sig.Sent()) != 1 {
		t.Fatalf("ask not forwarded: %v %v", err, sig.Sent())
	}
	owner := func(ch, chat, text string) channels.Inbound {
		return channels.Inbound{Channel: ch, ChatID: chat, Text: text, IsOwner: true}
	}
	for _, in := range []channels.Inbound{
		{Channel: "signal", ChatID: "+61400", Text: "yes", IsOwner: false}, // not the owner
		owner("irc", "akshay", "yes"),                                      // a nick anyone can take
		owner("telegram", "42", "yes"),                                     // never got the ask
	} {
		if _, ok, _ := d.decision(ctx, in, nil); ok {
			t.Fatalf("%s decided it", in.Key())
		}
	}
	reply, err := d.dispatch(ctx, owner("signal", "+61400", "yes"), agent.Events{})
	if err != nil || reply == "" {
		t.Fatalf("reply %q, err %v", reply, err)
	}
	if ap, err := d.store.GetApproval(ctx, id); err != nil || ap.Status != "approved" {
		t.Fatalf("approval: %+v %v", ap, err)
	}
	// The outcome is the reply in this chat; the home chat, which refuses,
	// doesn't forward a second copy of it back here.
	if got := sig.Sent(); len(got) != 1 {
		t.Fatalf("outcome forwarded back to the answering chat: %v", got)
	}
}

// A voice note gets its answer through the daemon: recorded in the
// conversation and the audit log, and the pause notice while paused.
func TestAttachmentOnlyMessagesAreAnsweredLikeAnyOther(t *testing.T) {
	d := newChannelsDaemon(t)
	tg := &fakeTransport{name: "telegram", owner: "42"}
	tg.Up()
	d.channels = map[string]channels.Channel{"telegram": tg}
	ctx := context.Background()
	voiceNote := channels.Inbound{Channel: "telegram", ChatID: "42", Media: channels.Voice, IsOwner: true}

	d.paused.Store(true)
	d.handleQueued(ctx, voiceNote)
	d.paused.Store(false)
	d.handleQueued(ctx, voiceNote)
	if got := tg.Sent(); len(got) != 2 || got[0] != "42: "+d.pausedReply(true) || got[1] != "42: "+channels.CantOpen(channels.Voice) {
		t.Fatalf("sent %v", got)
	}
	hist, _ := d.store.History(ctx, "telegram:42", 10)
	if len(hist) != 2 || !strings.Contains(hist[0].PlainText(), "voice message") || hist[1].PlainText() != channels.CantOpen(channels.Voice) {
		t.Fatalf("history %+v", hist)
	}
	audit, _ := d.store.RecentAudit(ctx, 10)
	var in, out int
	for _, e := range audit {
		switch e.Kind {
		case "message.in":
			in++
		case "message.out":
			out++
		}
	}
	if in != 2 || out != 2 {
		t.Fatalf("audit: %d in, %d out", in, out)
	}
}

// A channel that lost its connection and is trying again (WhatsApp says so
// through its Reporter) is sent to the Channels page as "reconnecting", with
// since when, so the page shows it amber rather than as a red error: the
// page read state and since, which the API never sent (ui merged with media).
func TestAReconnectingChannelIsSentAsReconnecting(t *testing.T) {
	td := newTestDaemon(t, butler)
	ft := &fakeTransport{name: "telegram", owner: "owner"}
	ft.Up()
	ft.Retrying(errors.New("network is unreachable"))
	td.chmu.Lock()
	td.channels["telegram"] = ft
	td.chmu.Unlock()
	cfg := td.Config()
	cfg.Channels.Telegram.Enabled = true
	*td.cfg = cfg
	var got api.ConnectorState
	for _, st := range td.ConnectorStates(context.Background()) {
		if st.Name == "telegram" {
			got = st
		}
	}
	if got.State != "reconnecting" || got.Since.IsZero() || got.Running {
		t.Fatalf("telegram reconnecting is sent as %+v", got)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"state":"reconnecting"`) || !strings.Contains(string(b), `"since":"`) {
		t.Fatalf("the page reads state and since: %s", b)
	}
	ft.Up()
	for _, st := range td.ConnectorStates(context.Background()) {
		if st.Name == "telegram" && (st.State != "" || !st.Running) {
			t.Fatalf("connected again: %+v", st)
		}
	}
}
