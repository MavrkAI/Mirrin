package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

func quickHold(t *testing.T) {
	t.Helper()
	old := holdAfter
	holdAfter = 40 * time.Millisecond
	t.Cleanup(func() { holdAfter = old })
}

// A long turn on a messaging channel gets one holding line, in the
// persona's voice, before the answer; the indicator comes back after it.
func TestLongTurnsGetOneHoldingLine(t *testing.T) {
	quickHold(t)
	release := make(chan struct{})
	td := newTestDaemon(t, func(string, llm.Request) llm.Response {
		<-release
		return say("Your flight is at 9:40, sir.")
	})
	tg := &fakeTransport{name: "telegram", owner: "owner"}
	tg.Up()
	td.channels["telegram"] = tg
	done := make(chan struct{})
	go func() {
		defer close(done)
		td.handleQueued(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Text: "when's my flight?", IsOwner: true})
	}()
	eventually(t, "the holding line", func() bool { return len(tg.Sent()) == 1 })
	if got := tg.Sent()[0]; got != "owner: On it, sir — this one needs a minute." {
		t.Fatalf("holding line %q", got)
	}
	// "typing…" once at the start, and again after the line.
	eventually(t, "typing shown again", func() bool { return tg.typing.Load() >= 2 })
	time.Sleep(3 * holdAfter) // still working: no second one
	close(release)
	<-done
	got := tg.Sent()
	if len(got) != 2 || got[1] != "owner: Your flight is at 9:40, sir." {
		t.Fatalf("sent %q", got)
	}
}

// The holding line never lands after the answer, however the two race.
func TestHoldingLineNeverFollowsTheAnswer(t *testing.T) {
	old := holdAfter
	t.Cleanup(func() { holdAfter = old })
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("Here you go.") })
	for i := 0; i < 30; i++ {
		holdAfter = time.Duration(i%6) * time.Millisecond
		var mu sync.Mutex
		var order []string
		tg := &fakeTransport{name: "telegram", owner: "owner", send: func(_, text string) error {
			if strings.HasPrefix(text, "On it") {
				time.Sleep(2 * time.Millisecond) // a slow send, to widen the race
			}
			mu.Lock()
			order = append(order, text)
			mu.Unlock()
			return nil
		}}
		tg.Up()
		td.chmu.Lock() // an earlier run's timer may be looking the channel up
		td.channels["telegram"] = tg
		td.chmu.Unlock()
		td.handleQueued(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Text: "hi", IsOwner: true})
		time.Sleep(8 * time.Millisecond)
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if got[len(got)-1] != "Here you go." || len(got) > 2 {
			t.Fatalf("run %d sent %q", i, got)
		}
	}
}

// A quick answer, or anything said during the turn, means no holding line;
// someone other than the owner gets it plainly; email gets none.
func TestHoldingLineOnlyWhenItHelps(t *testing.T) {
	quickHold(t)
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("Done.") })
	// A quick turn is one well inside the wait: 40ms is less than a turn
	// takes on a slow machine (Windows' timers alone tick every 15ms).
	holdAfter = 2 * time.Second
	td.handleQueued(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Text: "thanks", IsOwner: true})
	time.Sleep(100 * time.Millisecond)
	if got := td.ch.messages(); len(got) != 1 || got[0] != "owner: Done." {
		t.Fatalf("quick turn sent %q", got)
	}
	holdAfter = 40 * time.Millisecond

	// Said something on the way (an approval request, a note): no holding line after it.
	ctx, stop := td.holdOn(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", IsOwner: true})
	if err := td.Send(ctx, ownerKey, "Approval needed: reply yes 3."); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * holdAfter)
	stop()
	if got := td.ch.messages(); len(got) != 2 {
		t.Fatalf("holding line after the twin spoke: %q", got)
	}

	if line := td.holdingLine(false); line != "On it — this one needs a minute." {
		t.Fatalf("to someone else: %q", line)
	}
	for _, ch := range []string{"mail", "cli", "voice"} {
		ctx, stop := td.holdOn(context.Background(), channels.Inbound{Channel: ch, ChatID: "x", IsOwner: true})
		if ctx.Value(holdKey{}) != nil {
			t.Errorf("%s got a holding line", ch)
		}
		stop()
	}
}

// The line follows the persona's form of address.
func TestHoldingLineSpeaksAsThePersona(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	td.cfg.User.Honorific = ""
	td.persona.Address = "boss"
	if line := td.holdingLine(true); !strings.HasPrefix(line, "On it, boss — ") {
		t.Fatalf("line %q", line)
	}
	td.persona.Address = ""
	if line := td.holdingLine(true); line != "On it — this one needs a minute." {
		t.Fatalf("no form of address: %q", line)
	}
}
