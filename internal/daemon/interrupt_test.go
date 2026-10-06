package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/persona"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// ctxLLM is the scripted model with a real provider's manners: a cancelled
// request fails.
type ctxLLM struct{ *fakeLLM }

func (c ctxLLM) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.fakeLLM.Complete(ctx, req)
}

// booker is butler with a long job: "book the table" runs a browse that
// only a stop ends.
func booker(last string, req llm.Request) llm.Response {
	switch {
	case strings.Contains(last, "book the table"):
		return call("b1", "browse", `{}`)
	case strings.Contains(last, "book and email"):
		m := sends("boss")
		m.Message.Blocks = append(m.Message.Blocks, call("b2", "browse", `{}`).Message.Blocks...)
		return m
	}
	return butler(last, req)
}

// newBooker is a test daemon whose browse tool runs until it is stopped.
func newBooker(t *testing.T) (*testDaemon, chan struct{}, chan error) {
	td := newTestDaemon(t, booker)
	td.agent.SetProvider(ctxLLM{td.llm})
	started, ended := make(chan struct{}, 4), make(chan error, 4)
	td.agent.Tools().Register(tools.New("browse", "browse", nil, tools.RiskRead, func(ctx context.Context, _ tools.Call) (string, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			ended <- ctx.Err()
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
			ended <- nil
			return "booked", nil
		}
	}))
	return td, started, ended
}

func waitStarted(t *testing.T, started chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never started")
	}
}

func waitEnded(t *testing.T, ended chan error) {
	t.Helper()
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the job ran to the end: the stop never reached it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stop waited behind the turn it was meant to stop")
	}
}

func ownerSays(text string) channels.Inbound {
	return channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: text, IsOwner: true}
}

// "Stop" on a phone doesn't queue behind the turn it is meant to stop: it
// cuts it short at once, lets go of what the owner sent meanwhile, and the
// owner hears what was stopped.
func TestStopCutsTheRunningTurnShort(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	td.handle(ctx, ownerSays("book the table at Nobu"))
	waitStarted(t, started)
	td.handle(ctx, ownerSays("for four people"))
	td.handle(ctx, ownerSays("Stop!"))
	waitEnded(t, ended)
	got := td.ch.next(t)
	want := "OK, I've stopped. I was on “book the table at Nobu”; anything already done stays done. I let go of the message you sent after it as well; send it again if you still want it."
	if got != want {
		t.Fatalf("owner told %q\nwant %q", got, want)
	}
	time.Sleep(100 * time.Millisecond)
	if msgs := td.ch.messages(); len(msgs) != 1 {
		t.Fatalf("the stop or the dropped message got answers of their own: %q", msgs)
	}
	// The conversation carries on cleanly: the model sees the stop and no
	// tool call left hanging.
	h, _ := td.store.History(ctx, ownerKey, 4)
	if last := h[len(h)-1]; last.PlainText() != stopNote {
		t.Fatalf("history ends %+v", last)
	}
	if got := td.owner(t, "what's up?"); got != "Heard: what's up?" {
		t.Fatalf("next turn %q", got)
	}
}

// What the stopped turn had asked for approval is turned down with it.
func TestStopDropsWhatTheStoppedTurnAsked(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	td.handle(ctx, ownerSays("book and email the boss"))
	waitStarted(t, started)
	td.handle(ctx, ownerSays("cancel that"))
	waitEnded(t, ended)
	if got := td.ch.next(t); !strings.Contains(got, "I've dropped the request that was waiting for your OK, too.") {
		t.Fatalf("owner told %q", got)
	}
	ap, _ := td.store.GetApproval(ctx, 1)
	if ap.Status != "denied" || !strings.Contains(ap.DecidedBy, "stop") {
		t.Fatalf("#1 %+v", ap)
	}
	if got := td.owner(t, "yes"); !strings.HasPrefix(got, "Heard: yes") || len(td.ran()) != 0 {
		t.Fatalf("a yes after the stop: %q, sent %v", got, td.ran())
	}
}

// With nothing running in the chat it was said in, a stop reaches what the
// owner has running elsewhere: a long job started on the screen.
func TestStopReachesWorkOnAnotherChannel(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	done := make(chan string, 1)
	go func() {
		reply, _ := td.MessageEvents(ctx, channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "book the table at Nobu", IsOwner: true}, agent.Events{})
		done <- reply
	}()
	waitStarted(t, started)
	td.handle(ctx, ownerSays("stop"))
	waitEnded(t, ended)
	if got := td.ch.next(t); got != "OK, I've stopped what I was doing on the screen: “book the table at Nobu”. Anything already done stays done." {
		t.Fatalf("phone told %q", got)
	}
	if got := <-done; !strings.HasPrefix(got, "OK, I've stopped. I was on “book the table at Nobu”") {
		t.Fatalf("screen told %q", got)
	}
}

// From the screen's composer too, a stop doesn't wait for the turn.
func TestStopFromTheScreenComposer(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	screen := func(text string) channels.Inbound {
		return channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: text, IsOwner: true}
	}
	done := make(chan string, 1)
	go func() {
		reply, _ := td.MessageEvents(ctx, screen("book the table at Nobu"), agent.Events{})
		done <- reply
	}()
	waitStarted(t, started)
	reply, err := td.MessageEvents(ctx, screen("stop"), agent.Events{})
	if err != nil || reply != "OK, I've stopped “book the table at Nobu”. Anything already done stays done." {
		t.Fatalf("reply %q err %v", reply, err)
	}
	waitEnded(t, ended)
	<-done
}

// An approved action runs on after the page that approved it closes, but
// the owner's stop still reaches it.
func TestStopReachesAnApprovedActionBeingCarriedOut(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	td.agent.Tools().Register(tools.New("pay", "pay", nil, tools.RiskWrite, func(ctx context.Context, _ tools.Call) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		ended <- ctx.Err()
		return "", ctx.Err()
	}))
	id := raise(t, td, ownerKey, "pay", `{"to":"plumber"}`)
	done := make(chan string, 1)
	go func() {
		reply, _ := td.DecideApproval(ctx, id, true)
		done <- reply
	}()
	waitStarted(t, started)
	td.handle(ctx, ownerSays("stop"))
	waitEnded(t, ended)
	want := "OK, I've stopped. I was carrying out #1 (pay: to plumber); anything already done stays done."
	if got := <-done; got != want {
		t.Fatalf("screen told %q", got)
	}
	if got := td.ch.next(t); got != want {
		t.Fatalf("phone told %q", got)
	}
}

// payer registers a "pay" tool that takes three seconds unless it is
// stopped, and reports how it ended.
func payer(td *testDaemon, started chan struct{}, ended chan error) {
	td.agent.Tools().Register(tools.New("pay", "pay", nil, tools.RiskWrite, func(ctx context.Context, _ tools.Call) (string, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			ended <- ctx.Err()
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
			ended <- nil
			return "paid", nil
		}
	}))
}

// A routine run's request, approved with "yes 1" in the owner's chat, is
// carried out in the run's own conversation, detached from the owner's
// turn. The owner's "stop" in their chat still reaches it at once, and they
// hear what was stopped, not "I was on “yes 1”" once the payment had gone.
func TestStopReachesAnApprovedActionFromABackgroundRun(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	payer(td, started, ended)
	id := raise(t, td, ownerKey+"#protocol-20260927-150000", "pay", `{"to":"plumber"}`)
	begun := time.Now()
	td.handle(ctx, ownerSays(fmt.Sprintf("yes %d", id)))
	waitStarted(t, started)
	td.handle(ctx, ownerSays("stop"))
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the payment ran to the end: the stop never reached it")
		}
	case <-time.After(time.Second):
		t.Fatal("the stop didn't reach the approved action")
	}
	got := td.ch.next(t)
	if want := "OK, I've stopped. I was carrying out #1 (pay: to plumber); anything already done stays done."; got != want {
		t.Fatalf("owner told %q\nwant %q", got, want)
	}
	if time.Since(begun) >= 3*time.Second {
		t.Fatal("the reply waited for the action to end on its own")
	}
	time.Sleep(100 * time.Millisecond)
	if msgs := td.ch.messages(); len(msgs) != 1 {
		t.Fatalf("told more than once: %q", msgs)
	}
	// The owner's chat keeps the exchange: a stop doesn't lose the "yes 1".
	h, _ := td.store.History(ctx, ownerKey, 10)
	var said []string
	for _, m := range h {
		said = append(said, m.PlainText())
	}
	if !slices.Contains(said, "yes 1") {
		t.Fatalf("history %q", said)
	}
}

// From the screen, the same stop names the action once, where it was
// asked, not the "yes 1" that set it going as a second thing.
func TestStopFromTheScreenNamesTheApprovedActionOnce(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	payer(td, started, ended)
	raise(t, td, ownerKey+"#protocol-20260927-150000", "pay", `{"to":"plumber"}`)
	td.handle(ctx, ownerSays("yes 1"))
	waitStarted(t, started)
	reply, err := td.MessageEvents(ctx, channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "stop", IsOwner: true}, agent.Events{})
	if want := "OK, I've stopped what I was doing on Telegram: carrying out #1 (pay: to plumber). Anything already done stays done."; err != nil || reply != want {
		t.Fatalf("screen told %q (%v)\nwant %q", reply, err, want)
	}
	waitEnded(t, ended)
	if got, want := td.ch.next(t), "OK, I've stopped. I was carrying out #1 (pay: to plumber); anything already done stays done."; got != want {
		t.Fatalf("phone told %q\nwant %q", got, want)
	}
}

// gatedChannel holds its sends until the gate opens.
type gatedChannel struct {
	*fakeChannel
	entered, gate chan struct{}
}

func (g *gatedChannel) Send(ctx context.Context, chatID, text string) error {
	g.entered <- struct{}{}
	<-g.gate
	return g.fakeChannel.Send(ctx, chatID, text)
}

// A stop that comes once the approved action is done changes nothing: the
// reply says what happened, and that it had finished.
func TestAStopAfterTheActionIsDoneSaysItHadFinished(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	g := &gatedChannel{fakeChannel: td.ch, entered: make(chan struct{}, 1), gate: make(chan struct{})}
	td.channels["telegram"] = g
	raise(t, td, ownerKey, "send", `{"to":"boss"}`)
	done := onTheScreen(td, "yes 1") // decided on the screen; the phone hears how it went
	<-g.entered                      // sent, and telling the phone
	if reply, err := td.MessageEvents(ctx, channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "stop", IsOwner: true}, agent.Events{}); err != nil || !strings.HasPrefix(reply, "OK, I've stopped") {
		t.Fatalf("stop: %q %v", reply, err)
	}
	close(g.gate)
	if got, want := <-done, "Sent.\n\n(That had already finished when you said stop.)"; got != want {
		t.Fatalf("screen told %q\nwant %q", got, want)
	}
	if !slices.Equal(td.ran(), []string{"boss"}) {
		t.Fatalf("sent %v", td.ran())
	}
}

// The same from a request raised in the owner's own chat: the stop names
// what it was carrying out.
func TestStopNamesTheApprovedActionItCut(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	payer(td, started, ended)
	raise(t, td, ownerKey, "pay", `{"to":"plumber"}`)
	td.handle(ctx, ownerSays("yes 1"))
	waitStarted(t, started)
	td.handle(ctx, ownerSays("stop"))
	waitEnded(t, ended)
	if got, want := td.ch.next(t), "OK, I've stopped. I was carrying out #1 (pay: to plumber); anything already done stays done."; got != want {
		t.Fatalf("owner told %q\nwant %q", got, want)
	}
}

// A stop with more to say cuts the turn short, then the rest is answered.
func TestStopWithACorrection(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	td.handle(ctx, ownerSays("book the table at Nobu"))
	waitStarted(t, started)
	td.handle(ctx, ownerSays("Stop, not Nobu. Try Sushi Den"))
	waitEnded(t, ended)
	if got := td.ch.next(t); !strings.HasPrefix(got, "OK, I've stopped.") {
		t.Fatalf("first %q", got)
	}
	if got := td.ch.next(t); got != "Heard: Stop, not Nobu. Try Sushi Den" {
		t.Fatalf("then %q", got)
	}
}

// Nothing running: "stop" and "cancel that" are ordinary messages ("cancel
// that" still turns down what the twin just asked), and nobody else can
// stop the owner's work.
func TestStopWithNothingRunning(t *testing.T) {
	td := newTestDaemon(t, butler)
	if got := td.owner(t, "stop"); got != "Heard: stop" {
		t.Fatalf("reply %q", got)
	}
	td.owner(t, "email the boss")
	if got := td.owner(t, "cancel that"); got != "Dropped it." {
		t.Fatalf("reply %q", got)
	}
	tb, started, _ := newBooker(t)
	ctx := context.Background()
	tb.handle(ctx, ownerSays("book the table at Nobu"))
	waitStarted(t, started)
	tb.handle(ctx, channels.Inbound{Channel: "telegram", ChatID: "family", Sender: "bob", Text: "stop"})
	select {
	case got := <-tb.ch.out:
		if strings.Contains(got, "stopped") {
			t.Fatalf("someone else stopped the owner's work: %q", got)
		}
	case <-time.After(200 * time.Millisecond):
	}
	if _, ok := tb.Interrupt(ctx); !ok {
		t.Fatal("the owner's job should still be running")
	}
	eventually(t, "the stopped turn to say so", func() bool {
		return slices.ContainsFunc(tb.ch.messages(), func(m string) bool { return strings.HasPrefix(m, "owner: OK, I've stopped.") })
	})
}

// "Cancel that" and "never mind" are the owner's natural no to what the
// twin just asked in that chat. With nothing running there they answer it,
// and never reach work elsewhere; only a plain "stop" does that.
func TestCancelThatAnswersTheQuestionNotWorkElsewhere(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	screen := onTheScreen(td, "book the table at Nobu")
	waitStarted(t, started)
	if got := td.owner(t, "email the boss"); got != "I've drafted it. Shall I send it?" {
		t.Fatalf("asked %q", got)
	}
	td.handle(ctx, ownerSays("cancel that"))
	if got := td.ch.next(t); got != "Dropped it." {
		t.Fatalf("owner told %q", got)
	}
	if ap, _ := td.store.GetApproval(ctx, 1); ap.Status != "denied" {
		t.Fatalf("#1 is %s", ap.Status)
	}
	td.handle(ctx, ownerSays("never mind"))
	if got := td.ch.next(t); strings.Contains(got, "stopped") {
		t.Fatalf("owner told %q", got)
	}
	select {
	case <-ended:
		t.Fatal("the screen's job was stopped by an answer on the phone")
	case <-time.After(100 * time.Millisecond):
	}
	td.handle(ctx, ownerSays("stop")) // a plain stop does reach it
	waitEnded(t, ended)
	<-screen
}

// onTheScreen starts a turn from the presence screen's composer; its reply
// arrives on the channel returned.
func onTheScreen(td *testDaemon, text string) chan string {
	done := make(chan string, 1)
	go func() {
		reply, _ := td.MessageEvents(context.Background(), channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: text, IsOwner: true}, agent.Events{})
		done <- reply
	}()
	return done
}

// On IRC or by email anyone can claim to be the owner, so a "stop" there
// reaches only that chat's own work.
func TestAStopFromAForgeableChannelStaysThere(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	irc := &fakeChannel{name: "irc", owner: "owner", out: make(chan string, 8)}
	td.channels["irc"] = irc
	screen := onTheScreen(td, "book the table at Nobu")
	waitStarted(t, started)
	td.handle(ctx, channels.Inbound{Channel: "irc", ChatID: "owner", Sender: "owner", Text: "stop", IsOwner: true})
	if got := irc.next(t); strings.Contains(got, "stopped") {
		t.Fatalf("irc told %q", got)
	}
	select {
	case <-ended:
		t.Fatal("a stop on IRC reached the owner's work on the screen")
	case <-time.After(100 * time.Millisecond):
	}
	td.Interrupt(ctx)
	waitEnded(t, ended)
	<-screen
}

// A message taken off the mailbox but still waiting for its turn (the
// conversation busy carrying out an approved action, say) is what a stop
// sent meanwhile reaches: it never starts.
func TestStopReachesAMessageWaitingForItsTurn(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	c := td.conv(ownerKey)
	c.begin() // the conversation is busy
	td.handle(ctx, ownerSays("tell me a story"))
	eventually(t, "the message taken off the mailbox", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.inbox) == 0
	})
	td.handle(ctx, ownerSays("stop"))
	td.end(ownerKey, c)
	if got, want := td.ch.next(t), "OK, I've stopped. I was on “tell me a story”; anything already done stays done."; got != want {
		t.Fatalf("owner told %q\nwant %q", got, want)
	}
	if slices.Contains(td.llm.heard(), "tell me a story") {
		t.Fatal("the stopped message was answered anyway")
	}
}

// A message still in the mailbox when "stop" comes, with nothing running,
// is let go, and the owner hears so.
func TestStopLetsGoOfAMessageNotYetStarted(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	c := td.conv(ownerKey)
	c.mu.Lock()
	c.inbox = append(c.inbox, queued{ctx: ctx, in: ownerSays("tell me a story"), at: time.Now()})
	c.draining = true // its worker hasn't taken it yet
	c.mu.Unlock()
	td.handle(ctx, ownerSays("stop"))
	if got, want := td.ch.next(t), "OK, I hadn't started on that yet, so I've let it go. Send it again if you still want it."; got != want {
		t.Fatalf("owner told %q\nwant %q", got, want)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.inbox) != 0 {
		t.Fatalf("still queued: %+v", c.inbox)
	}
}

func TestIsStop(t *testing.T) {
	d := &Daemon{cfg: &config.Config{Name: "Mirrin"}, persona: persona.Persona{Name: "Mirrin", Pronunciation: "Mirrin", WakeWord: "mirrin"}}
	for text, want := range map[string][3]bool{ // stop, whole, firm
		"stop":                    {true, true, true},
		"Stop!":                   {true, true, true},
		"no no, stop":             {true, true, true},
		"Mirrin, stop":            {true, true, true},
		"ok stop now please":      {true, true, true},
		"abort":                   {true, true, true},
		"cancel that":             {true, true, false},
		"Never mind.":             {true, true, false},
		"don't do that":           {true, true, false},
		"stop stop stop":          {true, true, true},
		"Stop, not that flight!":  {true, false, true},
		"cancel that, try 8pm":    {true, false, false},
		"stop nudging":            {false, false, false},
		"no":                      {false, false, false},
		"wait":                    {false, false, false},
		"stop by the shop at 5":   {false, false, false},
		"can you stop the timer?": {false, false, false},
	} {
		stop, whole, firm := d.isStop(text)
		if [3]bool{stop, whole, firm} != want {
			t.Errorf("isStop(%q) = %v %v %v, want %v", text, stop, whole, firm, want)
		}
	}
}

// Regression (approvals merged with media): stopping a turn that answers a
// voice note told the owner "I was on “”": the mailbox registered the turn
// with the note's empty text, before it was transcribed.
func TestStopDuringAVoiceNoteTurnNamesIt(t *testing.T) {
	td, started, ended := newBooker(t)
	stubSpeech(t, nil, "book the table at Nobu", nil)
	var fetches atomic.Int32
	ctx := context.Background()
	td.handle(ctx, voiceFrom(true, 2, &fetches))
	waitStarted(t, started)
	td.handle(ctx, ownerSays("Stop!"))
	waitEnded(t, ended)
	if got := td.ch.next(t); strings.Contains(got, "“”") || !strings.Contains(got, "(voice note) book the table at Nobu") {
		t.Fatalf("owner told %q", got)
	}
	// Stopped while it was still being opened, it says so.
	c := td.conv(ownerKey)
	c.mu.Lock()
	r := c.queuedTurn(voiceFrom(true, 2, &fetches))
	c.mu.Unlock()
	defer untrack(r)
	if r.label() != "opening your voice note" {
		t.Fatalf("a voice note waiting to be heard is %q", r.label())
	}
}

// Regression (approvals merged with media): a voice note saying "stop"
// never cut the running turn. Its words were known only once transcribed,
// after the turn it was meant to stop; then the model answered "(voice
// note) Stop!". Now a short voice note from the owner is heard while their
// turn runs, and a stop in it stops that turn at once. It is heard once.
func TestAVoiceNoteThatSaysStopStopsTheTurn(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	td.handle(ctx, ownerSays("book the table at Nobu"))
	waitStarted(t, started)
	sp := stubSpeech(t, nil, "Stop!", nil)
	var fetches atomic.Int32
	td.handle(ctx, voiceFrom(true, 1, &fetches))
	td.handle(ctx, ownerSays("and a taxi after")) // sent after the stop: kept
	waitEnded(t, ended)
	if got := td.ch.next(t); !strings.Contains(got, "I've stopped. I was on “book the table at Nobu”") || strings.Contains(got, "let go of") {
		t.Fatalf("owner told %q", got)
	}
	if got := td.ch.next(t); got != "Heard: and a taxi after" {
		t.Fatalf("the message after the stop was answered %q", got)
	}
	time.Sleep(100 * time.Millisecond)
	if msgs := td.ch.messages(); len(msgs) != 2 {
		t.Fatalf("the voice note got an answer of its own: %q", msgs)
	}
	sp.mu.Lock()
	heard := len(sp.heard)
	sp.mu.Unlock()
	if heard != 1 || fetches.Load() != 1 {
		t.Fatalf("heard %d times, downloaded %d times", heard, fetches.Load())
	}
	for _, m := range td.llm.heard() {
		if strings.Contains(m, "Stop!") {
			t.Fatalf("the model was handed the stop: %q", td.llm.heard())
		}
	}
}

// A voice note that isn't a stop, heard while a turn runs, waits its turn
// and is answered as what was said, heard only once.
func TestAVoiceNoteHeardAheadIsAnsweredInItsTurn(t *testing.T) {
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		if last == "hold on" {
			return call("h1", "hold", `{}`)
		}
		if strings.Contains(last, "held") {
			return say("Done holding.")
		}
		return butler(last, req)
	})
	release, holding := make(chan struct{}), make(chan struct{}, 1)
	td.agent.Tools().Register(tools.New("hold", "hold", nil, tools.RiskRead, func(ctx context.Context, _ tools.Call) (string, error) {
		holding <- struct{}{}
		<-release
		return "held", nil
	}))
	ctx := context.Background()
	td.handle(ctx, ownerSays("hold on"))
	<-holding
	sp := stubSpeech(t, nil, "for four people", nil)
	var fetches atomic.Int32
	td.handle(ctx, voiceFrom(true, 1, &fetches))
	eventually(t, "the voice note heard ahead", func() bool { sp.mu.Lock(); defer sp.mu.Unlock(); return len(sp.heard) == 1 })
	close(release)
	if got := td.ch.next(t); got != "Done holding." {
		t.Fatalf("the running turn: %q", got)
	}
	if got := td.ch.next(t); got != "Heard: (voice note) for four people" {
		t.Fatalf("the voice note answered as %q", got)
	}
	sp.mu.Lock()
	heard := len(sp.heard)
	sp.mu.Unlock()
	if heard != 1 || fetches.Load() != 1 {
		t.Fatalf("heard %d times, downloaded %d times", heard, fetches.Load())
	}
}

// A voice note that says stop, with nothing running in its own chat, reaches
// the owner's work elsewhere as a typed stop does.
func TestAVoiceNoteStopReachesWorkElsewhere(t *testing.T) {
	td, started, ended := newBooker(t)
	ctx := context.Background()
	done := make(chan string, 1)
	go func() {
		reply, _ := td.message(ctx, channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "book the table at Nobu", IsOwner: true}, agent.Events{})
		done <- reply
	}()
	waitStarted(t, started)
	stubSpeech(t, nil, "Stop.", nil)
	var fetches atomic.Int32
	td.handle(ctx, voiceFrom(true, 1, &fetches))
	waitEnded(t, ended)
	if got := td.ch.next(t); !strings.Contains(got, "on the screen") {
		t.Fatalf("owner told %q", got)
	}
	<-done
}
