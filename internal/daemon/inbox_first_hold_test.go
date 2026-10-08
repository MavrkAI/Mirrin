package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// screenSaid is everything the screen's conversation holds, one message a
// line.
func screenSaid(t *testing.T, td *testDaemon) string {
	t.Helper()
	h, err := td.store.History(context.Background(), screenChat, 50)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, m := range h {
		b.WriteString(m.PlainText())
		b.WriteString("\n")
	}
	return b.String()
}

// A first day with no messaging app: Google is connected from the welcome
// page before the first hello. The look at the inbox says nothing until the
// hello is said and its briefing offer is answered, so a bare "yes" on the
// screen still settles the offer; and the look doesn't make a second
// every-morning offer on top of the briefing.
func TestFirstInboxLookWaitsForTheHelloAndItsOffer(t *testing.T) {
	var mu sync.Mutex
	looked := 0
	td, fake := inboxDaemon(t, 10*time.Millisecond, func(last string, req llm.Request) llm.Response {
		if isLook(req) {
			mu.Lock()
			looked++
			mu.Unlock()
			return say("PICKS: 1\nSam Lee needs you to sign the lease by Friday.\nShall I draft a reply to Sam? You'll see it before anything is sent.")
		}
		if strings.HasPrefix(last, "Time: ") {
			return say("Afternoon, Akshay. Type to me whenever you like.")
		}
		return say("All set.")
	})
	oldPoll := inboxFirstPoll
	inboxFirstPoll = 5 * time.Millisecond
	t.Cleanup(func() { inboxFirstPoll = oldPoll })
	oldNotify := desktopNotify
	desktopNotify = func(string, string) error { return nil }
	t.Cleanup(func() { desktopNotify = oldNotify })
	off := false
	td.cmu.Lock()
	td.cfg.UI.Weather = &off
	td.cmu.Unlock()
	td.chmu.Lock()
	delete(td.channels, "telegram") // only the screen reaches the owner
	td.chmu.Unlock()
	ctx := context.Background()
	td.stampInstall(ctx) // a fresh install, its hello still to come
	addMail(fake, "sam1", "Sam Lee <sam@example.com>", "The lease", "Can you sign by Friday?")

	connectWithoutWaiting(t, td)
	time.Sleep(100 * time.Millisecond)
	if said := screenSaid(t, td); strings.Contains(said, "skip the inbox") {
		t.Fatalf("the heads-up came before the first hello:\n%s", said)
	}

	reply, err := td.Hello(ctx, func(string) {}, nil)
	if err != nil || reply.Offer == nil {
		t.Fatalf("hello: %+v %v", reply, err)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	n := looked
	mu.Unlock()
	if n != 0 || strings.Contains(screenSaid(t, td), "skip the inbox") {
		t.Fatalf("the look went ahead with the briefing offer waiting:\n%s", screenSaid(t, td))
	}

	in := channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "yes", IsOwner: true}
	if _, err := td.message(ctx, in, agent.Events{}); err != nil {
		t.Fatal(err)
	}
	if !td.briefingAccepted() {
		t.Fatal("the bare yes didn't settle the briefing offer")
	}

	settleGoogle(td)
	said := screenSaid(t, td)
	if !strings.Contains(said, "skip the inbox") || !strings.Contains(said, "Sam Lee") {
		t.Fatalf("the look didn't follow the answer:\n%s", said)
	}
	if strings.Contains(said, inboxFirstDaily) {
		t.Fatalf("a second every-morning offer on top of the briefing:\n%s", said)
	}
}

// A twin with no install time (an upgrade, or a one-off) isn't held, and a
// morning briefing already running leaves out the every-morning offer.
func TestFirstInboxLookHoldAndDailyOffer(t *testing.T) {
	td, _ := inboxDaemon(t, time.Millisecond, func(string, llm.Request) llm.Response { return say("ok") })
	ctx := context.Background()
	if td.inboxFirstHeld(ctx) {
		t.Fatal("held with no install time")
	}
	if td.morningOffered(ctx) {
		t.Fatal("no briefing, yet the every-morning offer is left out")
	}
	td.stampInstall(ctx)
	if !td.inboxFirstHeld(ctx) {
		t.Fatal("not held with the first hello to come")
	}
	_ = td.store.Set(ctx, "first_look_at", time.Now().Format(time.RFC3339))
	if td.inboxFirstHeld(ctx) {
		t.Fatal("held after the hello with no offer waiting")
	}
	_ = td.store.Set(ctx, briefingOfferKey, "later")
	if !td.morningOffered(ctx) {
		t.Fatal("offered every morning after a later")
	}
}
