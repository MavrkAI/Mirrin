package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/heartbeat"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

const (
	friday      = "you've been guarding Friday afternoons."
	noticedLine = "Good morning, sir. I've noticed you've been guarding Friday afternoons. Shall I keep them clear when people ask?"
)

// noticedBrain words the noticed line as line, counts how often it is asked
// to, and echoes everything else.
type noticedBrain struct {
	mu        sync.Mutex
	line      string
	sensitive string // the model's answer to the sensitive-subject check; "No." when empty
	checked   []string
	asked     []llm.Request
}

func (b *noticedBrain) brain(last string, req llm.Request) llm.Response {
	if req.System == noticedCheck {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.checked = append(b.checked, last)
		if b.sensitive == "" {
			return say("No.")
		}
		return say(b.sensitive)
	}
	if strings.Contains(req.System, "you've noticed about them lately") {
		b.mu.Lock()
		b.asked = append(b.asked, req)
		b.mu.Unlock()
		return say(b.line)
	}
	return say("echo: " + last)
}

func (b *noticedBrain) times() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.asked)
}

// putPortrait stores a portrait written age ago, with its line on what's new.
func putPortrait(t *testing.T, td *testDaemon, age time.Duration, news string) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Add(-age).UTC()
	p, _ := json.Marshal(memory.Portrait{Text: "You like quiet mornings and plain answers.", UpdatedAt: at})
	n, _ := json.Marshal(portraitNews{Text: news, At: at})
	if err := td.store.Set(ctx, "portrait", string(p)); err != nil {
		t.Fatal(err)
	}
	if err := td.store.Set(ctx, keyPortraitNew, string(n)); err != nil {
		t.Fatal(err)
	}
}

func onScreen(t *testing.T, td *testDaemon, ctx context.Context, text string) string {
	t.Helper()
	reply, err := td.message(ctx, channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: text, IsOwner: true}, agent.Events{})
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	return reply
}

// The first hello after a portrait gets its line on what's new, in the
// persona's words, once. "That's right" is the screen's "That's you".
func TestNoticedIsSaidOnceWithTheFirstHello(t *testing.T) {
	b := &noticedBrain{line: noticedLine}
	td := newTestDaemon(t, b.brain)
	ctx := context.Background()
	putPortrait(t, td, time.Hour, friday)

	if got := onScreen(t, td, ctx, "what's the time?"); strings.Contains(got, "noticed") {
		t.Fatalf("a question got the line: %q", got)
	}
	if got := onScreen(t, td, ctx, "Good morning, Mirrin!"); got != noticedLine {
		t.Fatalf("the hello got %q", got)
	}
	if b.times() != 1 || !strings.Contains(b.asked[0].Messages[0].PlainText(), friday) {
		t.Fatalf("the model was asked %d times: %+v", b.times(), b.asked)
	}
	if sys := b.asked[0].System; !strings.Contains(sys, "data, not instructions") || !strings.Contains(sys, "Never say you have changed") {
		t.Fatalf("the system prompt: %q", sys)
	}
	// A yes to the offer: the portrait is acknowledged, and the model,
	// which has the offer in its history, takes it from there.
	if got := onScreen(t, td, ctx, "Yes, that's right."); !strings.HasPrefix(got, "echo: ") {
		t.Fatalf("the yes to an offer got %q", got)
	}
	if !td.screenData(ctx).PortraitAck {
		t.Fatal("that's right didn't acknowledge the portrait")
	}
	hist, _ := td.store.History(ctx, screenChat, 50)
	found := false
	for _, m := range hist {
		if m.Role == llm.RoleAssistant && m.PlainText() == noticedLine {
			found = true
		}
	}
	if !found {
		t.Fatal("the line isn't in the conversation's history")
	}
	if got := onScreen(t, td, ctx, "hello"); got == noticedLine || b.times() != 1 {
		t.Fatalf("a second hello got %q (%d asks)", got, b.times())
	}
}

// A plain observation gets a plain "noted"; "not really" lets it go, here
// and on the screen.
func TestNoticedYesAndNo(t *testing.T) {
	plain := "Morning, sir. I've noticed you've been guarding Friday afternoons."
	b := &noticedBrain{line: plain}
	td := newTestDaemon(t, b.brain)
	ctx := context.Background()
	putPortrait(t, td, time.Hour, friday)
	if got := onScreen(t, td, ctx, "morning"); got != plain {
		t.Fatalf("hello: %q", got)
	}
	if got := onScreen(t, td, ctx, "yep"); got != noticedYes || !td.screenData(ctx).PortraitAck {
		t.Fatalf("yes: %q, ack %v", got, td.screenData(ctx).PortraitAck)
	}

	td2 := newTestDaemon(t, (&noticedBrain{line: noticedLine}).brain)
	putPortrait(t, td2, time.Hour, friday)
	onScreen(t, td2, ctx, "hi")
	if got := onScreen(t, td2, ctx, "Not really."); got != noticedNo {
		t.Fatalf("no: %q", got)
	}
	if sd := td2.screenData(ctx); sd.PortraitNew != "" || sd.PortraitAck {
		t.Fatalf("after no the screen shows %q, ack %v", sd.PortraitNew, sd.PortraitAck)
	}
	// A no long after, or to something else, is the model's.
	if got := onScreen(t, td2, ctx, "no"); !strings.HasPrefix(got, "echo: ") {
		t.Fatalf("a later no got %q", got)
	}
}

// Nothing on health, money, relationships or secrets is said out of the
// blue, and the line isn't kept to try later.
func TestNoticedKeepsSensitiveThingsQuiet(t *testing.T) {
	b := &noticedBrain{line: noticedLine}
	td := newTestDaemon(t, b.brain)
	ctx := context.Background()
	putPortrait(t, td, time.Hour, "you've been seeing a therapist on Thursdays.")
	if got := onScreen(t, td, ctx, "hello"); got == noticedLine || b.times() != 0 {
		t.Fatalf("a sensitive line was said: %q (%d asks)", got, b.times())
	}
	if told, _ := td.store.Get(ctx, keyNoticedTold); told == "" {
		t.Fatal("the sensitive line wasn't passed over for good")
	}
}

// What the word list misses, the model's check catches, and the check
// fails closed: anything but a plain no, an error, or no model at all
// keeps the line quiet.
func TestNoticedModelCheckFailsClosed(t *testing.T) {
	for _, c := range []struct {
		name, answer string
		provider     llm.Provider // instead of the brain, when set
		none         bool         // no model at all
	}{
		{name: "yes", answer: "Yes."},
		{name: "unsure", answer: "Possibly"},
		{name: "error", provider: failingLLM{}},
		{name: "no model", none: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := &noticedBrain{line: noticedLine, sensitive: c.answer}
			td := newTestDaemon(t, b.brain)
			ctx := context.Background()
			switch {
			case c.provider != nil:
				td.agent.SetProvider(c.provider)
			case c.none:
				td.agent.SetProvider(nil)
			}
			putPortrait(t, td, time.Hour, "you've been ringing someone every evening.")
			in := channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "hello", IsOwner: true}
			if got, _, _ := td.portraitNoticed(api.WithPeer(ctx, api.Peer{Loopback: true}), in, "hello", agent.Events{}); got == noticedLine || b.times() != 0 {
				t.Fatalf("said: %q (%d asks)", got, b.times())
			}
		})
	}
	// A plain no lets it through, and the check saw the line.
	b := &noticedBrain{line: noticedLine}
	td := newTestDaemon(t, b.brain)
	putPortrait(t, td, time.Hour, friday)
	if got := onScreen(t, td, context.Background(), "hello"); got != noticedLine || len(b.checked) != 1 || !strings.Contains(b.checked[0], friday) {
		t.Fatalf("said %q, checked %q", got, b.checked)
	}
}

type failingLLM struct{}

func (failingLLM) Name() string { return "failing" }
func (failingLLM) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("down")
}

// A yes to the line's offer reaches the model, never an approval the twin
// asked about before the hello: the offer was the twin's latest words.
func TestNoticedYesToTheOfferIsNotAnApproval(t *testing.T) {
	b := &noticedBrain{line: noticedLine}
	td := newTestDaemon(t, b.brain)
	ctx := context.Background()
	putPortrait(t, td, time.Hour, friday)
	id := raise(t, td, screenChat, "send", `{"to":"bob"}`)
	td.noticed(ctx, screenChat, fmt.Sprintf("Shall I send the email to Bob? Reply \"yes %d\" or \"no %d\".", id, id), "asked about the email")
	if got := onScreen(t, td, ctx, "good morning"); got != noticedLine {
		t.Fatalf("hello: %q", got)
	}
	if got := onScreen(t, td, ctx, "yes"); got != "echo: yes" {
		t.Fatalf("the yes to the offer got %q", got)
	}
	ap, err := td.store.GetApproval(ctx, id)
	if err != nil || ap.Status != "pending" || len(td.ran()) != 0 {
		t.Fatalf("the approval: %+v (%v), sent %v", ap, err, td.ran())
	}
	if !td.screenData(ctx).PortraitAck {
		t.Fatal("the yes didn't acknowledge the portrait")
	}
}

// "Sure" to an offer is a yes to it too, and still the model's, even with
// a recent approval waiting in the chat.
func TestNoticedSureToTheOfferGoesToTheModel(t *testing.T) {
	b := &noticedBrain{line: noticedLine}
	td := newTestDaemon(t, b.brain)
	ctx := context.Background()
	putPortrait(t, td, time.Hour, friday)
	raise(t, td, screenChat, "send", `{"to":"bob"}`)
	onScreen(t, td, ctx, "hello")
	if got := onScreen(t, td, ctx, "sure"); got != "echo: sure" {
		t.Fatalf("sure got %q", got)
	}
}

// A yes after something else has asked the owner a question is that
// question's, not the line's.
func TestNoticedYesAfterAnotherQuestionIsNotTheLines(t *testing.T) {
	plain := "Morning, sir. I've noticed you've been guarding Friday afternoons."
	b := &noticedBrain{line: plain}
	td := newTestDaemon(t, b.brain)
	ctx := context.Background()
	putPortrait(t, td, time.Hour, friday)
	onScreen(t, td, ctx, "morning")
	time.Sleep(10 * time.Millisecond)
	raise(t, td, screenChat, "send", `{"to":"bob"}`)
	if got := onScreen(t, td, ctx, "yes"); got == noticedYes || td.screenData(ctx).PortraitAck {
		t.Fatalf("the yes went to the portrait: %q", got)
	}
}

// "Ok" isn't a yes to a plain observation, and the answer to the line is
// kept in the history with the line.
func TestNoticedOkIsNotAConfirmation(t *testing.T) {
	plain := "Morning, sir. I've noticed you've been guarding Friday afternoons."
	b := &noticedBrain{line: plain}
	td := newTestDaemon(t, b.brain)
	ctx := context.Background()
	putPortrait(t, td, time.Hour, friday)
	onScreen(t, td, ctx, "morning")
	if got := onScreen(t, td, ctx, "ok"); got == noticedYes || td.screenData(ctx).PortraitAck {
		t.Fatalf("ok confirmed the portrait: %q", got)
	}

	td2 := newTestDaemon(t, (&noticedBrain{line: plain}).brain)
	putPortrait(t, td2, time.Hour, friday)
	onScreen(t, td2, ctx, "morning")
	onScreen(t, td2, ctx, "not really")
	hist, _ := td2.store.History(ctx, screenChat, 50)
	var said []string
	for _, m := range hist {
		said = append(said, m.PlainText())
	}
	if n := len(said); n < 2 || said[n-2] != "not really" || said[n-1] != noticedNo {
		t.Fatalf("history: %q", said)
	}
}

// An offer is a question that offers something, not a pleasantry.
func TestNoticedOffers(t *testing.T) {
	for line, want := range map[string]bool{
		noticedLine: true,
		"Morning. I've noticed you walk at lunch. Should I keep that hour free?": true,
		"Morning. I've noticed you walk at lunch. Want me to keep it free?":      true,
		"Morning. I've noticed you walk at lunch.":                               false,
		"Morning. I've noticed you walk at lunch. How are you?":                  false,
		"Morning! How was the weekend? I've noticed you walk at lunch.":          false,
	} {
		if got := offers(line); got != want {
			t.Errorf("%q: %v, want %v", line, got, want)
		}
	}
}

// At most once a week, and never once the owner has said stop.
func TestNoticedIsWeeklyAndCanBeStopped(t *testing.T) {
	b := &noticedBrain{line: noticedLine}
	td := newTestDaemon(t, b.brain)
	ctx := context.Background()
	putPortrait(t, td, 2*time.Hour, friday)
	if got := onScreen(t, td, ctx, "hello"); got != noticedLine {
		t.Fatalf("hello: %q", got)
	}
	// A portrait refreshed by hand the same week doesn't bring another.
	putPortrait(t, td, time.Hour, "you've started walking at lunch.")
	if got := onScreen(t, td, ctx, "hello"); got == noticedLine || b.times() != 1 {
		t.Fatalf("a second line within the week: %q (%d asks)", got, b.times())
	}
	// "Stop telling me these" just after a line turns it off.
	td.store.Unset(ctx, keyNoticedTold)
	td.store.Set(ctx, keyNoticedAt, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	if got := onScreen(t, td, ctx, "Stop telling me these."); !strings.HasPrefix(got, "Understood.") {
		t.Fatalf("stop: %q", got)
	}
	td.store.Set(ctx, keyNoticedAt, time.Now().Add(-8*24*time.Hour).UTC().Format(time.RFC3339))
	putPortrait(t, td, time.Minute, "you've started walking at lunch.")
	if got := onScreen(t, td, ctx, "hello"); got == noticedLine || b.times() != 1 {
		t.Fatalf("after stop: %q (%d asks)", got, b.times())
	}
	// Without a line just told, "stop telling me these" is the model's.
	td3 := newTestDaemon(t, b.brain)
	if got := onScreenDaemon(t, td3, "stop telling me these"); !strings.HasPrefix(got, "echo: ") {
		t.Fatalf("stop with nothing told: %q", got)
	}
}

func onScreenDaemon(t *testing.T, td *testDaemon, text string) string {
	return onScreen(t, td, context.Background(), text)
}

// Only on this computer: a hello from a paired phone, or in a messaging
// app, doesn't get it; one said aloud does.
func TestNoticedOnlyHere(t *testing.T) {
	b := &noticedBrain{line: noticedLine}
	td := newTestDaemon(t, b.brain)
	ctx := context.Background()
	putPortrait(t, td, time.Hour, friday)
	phone := api.WithPeer(ctx, api.Peer{Device: &devices.Device{ID: "p", Name: "Phone", Kind: devices.KindPWA, Scopes: []devices.Scope{devices.View, devices.Chat}}})
	if got := onScreen(t, td, phone, "hello"); got == noticedLine {
		t.Fatal("a phone's hello got the line")
	}
	if got := td.owner(t, "hello"); got == noticedLine {
		t.Fatal("a messaging app's hello got the line")
	}
	if b.times() != 0 {
		t.Fatalf("the model was asked %d times", b.times())
	}
	reply, err := td.message(ctx, channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: "Good evening.", IsOwner: true}, agent.Events{})
	if err != nil || reply != noticedLine {
		t.Fatalf("aloud: %q (%v)", reply, err)
	}
}

// Without the model the line is still said, plainly.
func TestNoticedPlainWithoutTheModel(t *testing.T) {
	if got := plainNoticed("Good morning, sir.", "sir", friday); got != "Good morning, sir. I've noticed you've been guarding Friday afternoons." {
		t.Fatalf("plain: %q", got)
	}
	if got := plainNoticed("", "Akshay", "you walk at lunch"); got != "Hello, Akshay. I've noticed you walk at lunch." {
		t.Fatalf("plain without a hello: %q", got)
	}
}

// Aloud, it waits while a look-only wall screen is paired: the wall shows
// what is said in the room.
func TestNoticedNotAloudBesideAWall(t *testing.T) {
	b := &noticedBrain{line: noticedLine}
	td := newTestDaemon(t, b.brain)
	ctx := context.Background()
	putPortrait(t, td, time.Hour, friday)
	store := devices.NewMemory()
	if _, _, err := store.Add("Kitchen", devices.KindKiosk, []devices.Scope{devices.View}, "lan", ""); err != nil {
		t.Fatal(err)
	}
	deviceHubs.Store(td.Daemon, &deviceHub{store: store})
	t.Cleanup(func() { deviceHubs.Delete(td.Daemon) })
	aloud := channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: "hello", IsOwner: true}
	if reply, _ := td.message(ctx, aloud, agent.Events{}); reply == noticedLine || b.times() != 0 {
		t.Fatalf("said aloud beside a wall: %q", reply)
	}
	// Typed on this computer, the wall never sees it.
	if got := onScreen(t, td, ctx, "hello"); got != noticedLine {
		t.Fatalf("typed here: %q", got)
	}
}

// A yes or no after the twin has sent something of its own to the chat (a
// briefing, a bill, a travel offer) is that message's, not the line's.
func TestNoticedYesOrNoAfterAMessageOfItsOwnIsNotTheLines(t *testing.T) {
	ctx := context.Background()
	for _, answer := range []string{"no", "yes"} {
		b := &noticedBrain{line: noticedLine}
		td := newTestDaemon(t, b.brain)
		screenOnly(td) // no messaging app: what the twin sends lands here
		notifications(t)
		putPortrait(t, td, time.Hour, friday)
		if got := onScreen(t, td, ctx, "good morning"); got != noticedLine {
			t.Fatalf("hello: %q", got)
		}
		time.Sleep(10 * time.Millisecond)
		if err := td.Notify(ctx, screenChat, "Your 9:30 clashes with lunch. Shall I move the 9:30?"); err != nil {
			t.Fatal(err)
		}
		got := onScreen(t, td, ctx, answer)
		if got == noticedNo || got == noticedYes {
			t.Fatalf("%s went to the line: %q", answer, got)
		}
		sd := td.screenData(ctx)
		if sd.PortraitAck || sd.PortraitNew == "" {
			t.Fatalf("%s changed the portrait: ack %v, new %q", answer, sd.PortraitAck, sd.PortraitNew)
		}
	}
}

// "Stop telling me these" after a travel offer stops the travel offers,
// not the noticed lines, even with a line told earlier the same day.
func TestStopAfterATravelOfferIsTheTravelOffers(t *testing.T) {
	ctx := context.Background()
	for _, chat := range []string{screenChat, ownerKey} {
		b := &noticedBrain{line: noticedLine}
		td := newTestDaemon(t, b.brain)
		if chat == screenChat {
			screenOnly(td) // no messaging app: the offer lands here
			notifications(t)
		}
		putPortrait(t, td, time.Hour, friday)
		if got := onScreen(t, td, ctx, "good morning"); got != noticedLine {
			t.Fatalf("hello: %q", got)
		}
		time.Sleep(10 * time.Millisecond)
		raw, _ := json.Marshal(heartbeat.TravelOffer{Ask: "You told me Dan moved here. Want a nudge tomorrow to see if Dan's free for coffee?",
			Remind: "See if Dan's free for coffee", Who: "Dan", Zone: "Europe/Berlin", At: clock()})
		if err := td.store.Set(ctx, heartbeat.TravelOfferKey, string(raw)); err != nil {
			t.Fatal(err)
		}
		if err := td.Notify(ctx, chat, "You're in Berlin now. You told me Dan moved here. Want a nudge tomorrow to see if Dan's free for coffee?"); err != nil {
			t.Fatal(err)
		}
		var got string
		if chat == screenChat {
			got = onScreen(t, td, ctx, "Stop telling me these.")
		} else {
			got = td.owner(t, "Stop telling me these.")
		}
		if strings.HasPrefix(got, "Understood. I'll keep") {
			t.Fatalf("%s: the noticed lines were turned off: %q", chat, got)
		}
		if off, _ := td.store.Get(ctx, keyNoticedOff); off != "" {
			t.Fatalf("%s: noticing turned off", chat)
		}
		if off, _ := td.store.Get(ctx, heartbeat.TravelPeopleOffKey); off != "1" {
			t.Fatalf("%s: travel offers still on, reply %q", chat, got)
		}
	}
}
