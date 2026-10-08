package daemon

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tasks"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// reactionsIn lists the "react" events seen, as "kind: why".
func reactionsIn(evs []events.Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Kind == "react" {
			why := ""
			if r, ok := ev.Data.(events.Reaction); ok {
				why = r.Why
			}
			out = append(out, ev.Text+": "+why)
		}
	}
	return out
}

// finisher finishes every task it is given, and otherwise behaves as the butler.
func finisher(last string, req llm.Request) llm.Response {
	switch {
	case strings.Contains(last, "Background task started"):
		return call("f1", "task_update", `{"finish":"Booked. Table for two at 8."}`)
	case strings.Contains(last, "Task finished"):
		return say("Booked.")
	}
	return butler(last, req)
}

// startTask starts a task in the owner's chat and waits for it to end in want.
func startTask(t *testing.T, td *testDaemon, title string, want tasks.Status) {
	t.Helper()
	task, err := td.tasks.Start(context.Background(), ownerKey, title, "do it")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, title+" to be "+string(want), func() bool {
		for _, x := range td.tasks.List() {
			if x.ID == task.ID {
				return x.Status == want
			}
		}
		return false
	})
}

// reactedAgo makes kind's last play ago, as if that much time had passed
// (a task's outcome is heard on its own goroutine, so the clock stays put).
func (td *testDaemon) reactedAgo(kind string, ago time.Duration) {
	v, _ := reactions.LoadOrStore(td.Daemon, &reactLog{last: map[string]time.Time{}})
	l := v.(*reactLog)
	l.mu.Lock()
	l.last[kind] = clock().Add(-ago)
	l.mu.Unlock()
}

// A task that finishes gives a small nod on the screens, and one that
// stops after a problem a tilt of the head.
func TestATaskDoneIsPleasedAndOneFailedIsSorry(t *testing.T) {
	td := newTestDaemon(t, finisher)
	seen := listen(t, td.bus)
	// A task asked for in a shared chat isn't the owner's turn: nothing.
	td.taskOutcome(tasks.Task{Owner: "telegram:family", Title: "Bob's errand"}, tasks.Done)
	startTask(t, td, "Book Ottolenghi", tasks.Done)
	eventually(t, "the nod", func() bool { return len(reactionsIn(seen())) == 1 })
	td.agent.SetProvider(failingModel{})
	startTask(t, td, "Chase the refund", tasks.Failed)
	eventually(t, "the tilt", func() bool { return len(reactionsIn(seen())) == 2 })
	want := []string{"pleased: a task finished", "sorry: a task stopped after a problem"}
	if got := reactionsIn(seen()); !slices.Equal(got, want) {
		t.Fatalf("reactions %q, want %q", got, want)
	}
}

// Two tasks finishing within 20 seconds of each other nod once; one after
// that nods again.
func TestTheSameReactionPlaysAtMostOnceEvery20Seconds(t *testing.T) {
	td := newTestDaemon(t, finisher)
	seen := listen(t, td.bus)
	startTask(t, td, "Book Ottolenghi", tasks.Done)
	startTask(t, td, "Move the dentist", tasks.Done)
	time.Sleep(100 * time.Millisecond) // the outcome is heard as the task's lock is let go
	if got := reactionsIn(seen()); !slices.Equal(got, []string{"pleased: a task finished"}) {
		t.Fatalf("two within 20 seconds: %q", got)
	}
	td.reactedAgo("pleased", 21*time.Second)
	startTask(t, td, "Find a plumber", tasks.Done)
	eventually(t, "the second nod", func() bool { return len(reactionsIn(seen())) == 2 })
}

// A reply that fails in the owner's chat is sorry; someone else's failing
// moves nothing on the owner's screen.
func TestOnlyTheOwnersFailedReplyIsSorry(t *testing.T) {
	td := newTestDaemon(t, butler)
	seen := listen(t, td.bus)
	td.agent.SetProvider(failingModel{})
	td.answer(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "999", Sender: "Bob", Text: "is Tony in?"})
	if got := reactionsIn(seen()); len(got) != 0 {
		t.Fatalf("a stranger's error reached the screen: %q", got)
	}
	if got := td.ch.messages(); !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "999: ") }) {
		t.Fatalf("Bob wasn't answered: %q", got)
	}
	td.answer(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "hello", IsOwner: true})
	if got := reactionsIn(seen()); !slices.Equal(got, []string{"sorry: a reply failed"}) {
		t.Fatalf("the owner's error: %q", got)
	}
	// The screen's own turn (present) too, once the 20 seconds are up.
	withClock(t, 21*time.Second)
	if _, err := td.Message(context.Background(), channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "hello", IsOwner: true}); err == nil {
		t.Fatal("the screen's turn should have failed")
	}
	if got := reactionsIn(seen()); len(got) != 2 || got[1] != "sorry: a reply failed" {
		t.Fatalf("the screen's error: %q", got)
	} // The failed model was rebuilt from the config after the first error;
	// it went to the local stand-in, never to the real provider.
	if td.modelCalls.Load() == 0 {
		t.Fatal("the rebuilt model didn't go to the stand-in server")
	}
}

// A request for the owner makes the character glance towards Needs; the
// step approved and carried out is a nod. A denied one, one that failed
// when it ran, and anything asked for by someone else move nothing.
func TestApprovalsReactOnlyToWhatHappened(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.agent.Tools().Register(tools.New("post", "post a letter", tools.Schema(map[string]tools.Prop{"to": {Type: "string"}}), tools.RiskWrite,
		func(context.Context, tools.Call) (string, error) { return "", errors.New("the post office is shut") }))
	seen := listen(t, td.bus)
	ctx := context.Background()

	td.owner(t, "email the boss") // #1
	if got := reactionsIn(seen()); !slices.Equal(got, []string{"attentive: something needs you"}) {
		t.Fatalf("raised: %q", got)
	}
	if _, err := td.DecideApproval(ctx, td.pendingFor(ctx, ownerKey)[0].ID, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"attentive: something needs you", "pleased: an approved step was carried out"}
	if got := reactionsIn(seen()); !slices.Equal(got, want) {
		t.Fatalf("approved: %q, want %q", got, want)
	}

	withClock(t, 21*time.Second)
	td.owner(t, "email the bank") // #2, turned down
	if _, err := td.DecideApproval(ctx, td.pendingFor(ctx, ownerKey)[0].ID, false); err != nil {
		t.Fatal(err)
	}
	want = append(want, "attentive: something needs you")
	if got := reactionsIn(seen()); !slices.Equal(got, want) {
		t.Fatalf("denied: %q, want %q", got, want)
	}

	// One that fails when it runs: no nod.
	withClock(t, 21*time.Second)
	td.llm.mu.Lock()
	td.llm.brain = func(last string, req llm.Request) llm.Response {
		if last == "post it" {
			return call("p1", "post", `{"to":"council"}`)
		}
		return butler(last, req)
	}
	td.llm.mu.Unlock()
	td.owner(t, "post it")
	if _, err := td.DecideApproval(ctx, td.pendingFor(ctx, ownerKey)[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if got := reactionsIn(seen()); len(got) != 4 || got[3] != "attentive: something needs you" {
		t.Fatalf("a step that failed: %q", got)
	}

	// Bob's request, raised and approved: nothing.
	withClock(t, 21*time.Second)
	td.llm.mu.Lock()
	td.llm.brain = butler
	td.llm.mu.Unlock()
	before := len(reactionsIn(seen()))
	inChat(t, td, "family", "bob", "email the plumber")
	aps, err := td.store.AllPendingApprovals(ctx)
	if err != nil || len(aps) != 1 || aps[0].ChatKey != "telegram:family" {
		t.Fatalf("Bob's request: %+v %v", aps, err)
	}
	td.owner(t, "yes "+strconv.FormatInt(aps[0].ID, 10))
	if !slices.Contains(td.ran(), "plumber") {
		t.Fatalf("Bob's request wasn't carried out: %v", td.ran())
	}
	if got := reactionsIn(seen()); len(got) != before {
		t.Fatalf("someone else's request moved the owner's character: %q", got[before:])
	}
}
