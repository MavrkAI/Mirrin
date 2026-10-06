package tasks

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// An approved step handed to a task that is cancelled while the step waits
// for its turn never runs; whoever handed it over hears so, and can say the
// approval wasn't carried out rather than leave it reading "approved".
func TestAStepWhoseLegNeverStartsIsReportedDropped(t *testing.T) {
	release := make(chan struct{})
	m := New(context.Background(), Deps{
		Run: func(ctx context.Context, _ *Task, _ string) (string, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return "ok", nil
		},
	})
	task, _ := m.Start(context.Background(), "w:1", "Chase refund", "get the money back")
	waitFor(t, "first leg", func() bool { return view(m, task.ID).Runs == 1 })
	ran := make(chan struct{}, 1)
	dropped := make(chan struct{}, 1)
	m.ResumeOr(context.Background(), task, func(context.Context) (string, error) {
		ran <- struct{}{}
		return "sent", nil
	}, func() { dropped <- struct{}{} })
	if err := m.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-dropped:
	case <-ran:
		t.Fatal("the step ran on a cancelled task")
	case <-time.After(2 * time.Second):
		t.Fatal("nobody heard that the step never ran")
	}
}

// board is a fake model for a task with three steps on its board.
type board struct {
	t      *testing.T
	reg    *tools.Registry
	mu     sync.Mutex
	inputs []string
	finish bool // finish when told to carry on
}

func (b *board) run(ctx context.Context, task *Task, input string) (string, error) {
	b.mu.Lock()
	b.inputs = append(b.inputs, input)
	finish := b.finish
	b.mu.Unlock()
	update := func(args string) {
		if _, err := b.reg.Run(ctx, "task_update", tools.Call{ChatKey: task.Key, Input: json.RawMessage(args)}); err != nil {
			b.t.Errorf("task_update: %v", err)
		}
	}
	switch {
	case strings.Contains(input, "started by the user"):
		update(`{"add_steps":"[\"ask the shop\",\"chase the bank\",\"confirm\"]"}`)
		update(`{"ask_user":"Shall I email the shop?"}`)
		return "asked", nil
	case strings.Contains(input, "board still has open steps") && finish:
		update(`{"step_done":2}`)
		update(`{"step_done":3,"finish":"Refund on its way."}`)
		return "done", nil
	}
	// An approval's result or an answer: it reports and stops, as a model
	// told only "tell the user the outcome" would.
	update(`{"step_done":1}`)
	return "Emailed the shop.", nil
}

func (b *board) got() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.inputs...)
}

// Approving one step of a task, or answering its question, is not the task
// done: a leg that settles one step and stops with the board still open is
// sent on from the board, once.
func TestAMidTaskStepThatStopsShortCarriesOn(t *testing.T) {
	for _, finishes := range []bool{true, false} {
		n := &notes{}
		reg := tools.NewRegistry()
		b := &board{t: t, reg: reg, finish: finishes}
		m := New(context.Background(), Deps{Run: b.run, Notify: n.notify})
		reg.Register(m.Tools()...)
		task, _ := m.Start(context.Background(), "w:1", "Chase refund", "get the money back")
		waitFor(t, "the question", func() bool { return view(m, task.ID).Status == WaitingUser })
		m.Resume(context.Background(), task, func(c context.Context) (string, error) {
			return b.run(c, task, "[System: the user approved #1. send was executed and succeeded.]")
		})
		waitFor(t, "the task to settle", func() bool { return view(m, task.ID).Status == Done })
		inputs := b.got()
		if len(inputs) != 3 || !strings.Contains(inputs[2], "board still has open steps") || !strings.Contains(inputs[2], "Goal: get the money back") {
			t.Fatalf("finishes=%v: legs %q", finishes, inputs)
		}
		got := view(m, task.ID)
		if finishes && got.Result != "Refund on its way." {
			t.Fatalf("result %q", got.Result)
		}
		if !finishes && got.Result != "Emailed the shop." { // nudged once, then its last words stand
			t.Fatalf("result %q", got.Result)
		}
	}
}

// A task's question can be answered from any chat, however long ago it was
// asked: the answer is taken once, and passed on in the owner's words.
func TestAnswerReachesATaskHoweverLate(t *testing.T) {
	var mu sync.Mutex
	var inputs []string
	m := New(context.Background(), Deps{Run: func(_ context.Context, _ *Task, input string) (string, error) {
		mu.Lock()
		inputs = append(inputs, input)
		mu.Unlock()
		return "ok", nil
	}})
	m.tasks["1"] = &Task{ID: "1", Owner: "cli:terminal", Key: "cli:terminal#task-1", Title: "Dinner", Status: WaitingUser, Question: "7pm or 8pm?", AskedAt: time.Now().Add(-3 * time.Hour)}
	if m.WaitingOn("cli:terminal", 30*time.Minute) != nil {
		t.Fatal("a three-hour-old question takes a bare reply")
	}
	if _, err := m.Answer(context.Background(), "1", "8pm, and a table outside"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Answer(context.Background(), "1", "7pm"); err == nil || !strings.Contains(err.Error(), "isn't waiting") {
		t.Fatalf("a second answer: %v", err)
	}
	if _, err := m.Answer(context.Background(), "2", "x"); err == nil {
		t.Fatal("answered a task that doesn't exist")
	}
	waitFor(t, "the leg", func() bool { mu.Lock(); defer mu.Unlock(); return len(inputs) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(inputs[0], `"7pm or 8pm?"`) || !strings.Contains(inputs[0], "8pm, and a table outside") {
		t.Fatalf("leg input %q", inputs[0])
	}
}

func TestWaitingForMatchesTheChatsThatStandIn(t *testing.T) {
	m := New(context.Background(), Deps{})
	m.tasks["1"] = &Task{ID: "1", Owner: "cli:terminal", Status: WaitingUser, AskedAt: time.Now()}
	phone := func(owner string) bool { return owner == "telegram:1" || owner == "cli:terminal" }
	if got := m.WaitingFor(phone, time.Minute); got == nil || got.ID != "1" {
		t.Fatalf("got %+v", got)
	}
	if m.WaitingFor(func(o string) bool { return o == "telegram:1" }, time.Minute) != nil {
		t.Fatal("matched a chat that doesn't stand in")
	}
	m.tasks["2"] = &Task{ID: "2", Owner: "telegram:1", Status: WaitingUser, AskedAt: time.Now()}
	if m.WaitingFor(phone, time.Minute) != nil {
		t.Fatal("two tasks waiting: a reply can't say which")
	}
}

// A task whose request for approval lapsed unanswered is set aside, not left
// waiting forever; the owner hears once, and retry tells the model why.
func TestALapsedRequestSetsTheTaskAside(t *testing.T) {
	n := &notes{}
	var mu sync.Mutex
	var inputs []string
	pending := 0
	m := New(context.Background(), Deps{
		Run: func(_ context.Context, _ *Task, input string) (string, error) {
			mu.Lock()
			inputs = append(inputs, input)
			mu.Unlock()
			return "ok", nil
		},
		Notify:  n.notify,
		Pending: func(context.Context, string) (int, string) { return pending, "" },
	})
	m.tasks["7"] = &Task{ID: "7", Owner: "w:1", Key: "w:1#task-7", Title: "Chase refund", Goal: "get the money back", Status: WaitingApproval}
	pending = 1
	m.Lapsed("7", "send: to shop")
	if got := view(m, "7").Status; got != WaitingApproval {
		t.Fatalf("still waiting on another request, but %s", got)
	}
	pending = 0
	m.Lapsed("7", "send: to shop")
	if got := view(m, "7"); got.Status != Paused || !strings.Contains(got.PausedFor, "send: to shop") {
		t.Fatalf("task %+v", got)
	}
	waitFor(t, "notice", func() bool { return len(n.all()) == 1 })
	if said := n.all()[0]; !strings.Contains(said, "didn't hear back") || !strings.Contains(said, `"retry task 7"`) {
		t.Fatalf("owner told %q", said)
	}
	m.Lapsed("7", "send: to shop") // already set aside: said once
	time.Sleep(20 * time.Millisecond)
	if len(n.all()) != 1 {
		t.Fatalf("the owner was told more than once: %q", n.all())
	}
	if err := m.Retry(context.Background(), "7"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "retry leg", func() bool { mu.Lock(); defer mu.Unlock(); return len(inputs) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(inputs[0], "didn't answer your request to approve send: to shop") || !strings.Contains(inputs[0], "Goal: get the money back") {
		t.Fatalf("retry input %q", inputs[0])
	}
	if view(m, "7").PausedFor != "" {
		t.Fatal("the reason outlived the retry")
	}
}

// Requests that lapse together (the first sweep after an upgrade finds
// every old one) set their tasks aside with one line per chat, not a burst.
func TestTasksSetAsideTogetherAreToldOnce(t *testing.T) {
	n := &notes{}
	m := New(context.Background(), Deps{Notify: n.notify})
	m.tasks["7"] = &Task{ID: "7", Owner: "w:1", Key: "w:1#task-7", Title: "Chase refund", Status: WaitingApproval}
	m.tasks["8"] = &Task{ID: "8", Owner: "w:1", Key: "w:1#task-8", Title: "Book dinner", Status: WaitingApproval}
	m.tasks["9"] = &Task{ID: "9", Owner: "w:2", Key: "w:2#task-9", Title: "Renew permit", Status: WaitingApproval}
	m.LapsedAll([]Lapse{{"7", "send: to shop"}, {"8", "book: 8pm"}, {"9", "pay: council"}})
	waitFor(t, "notices", func() bool { return len(n.all()) == 2 })
	time.Sleep(20 * time.Millisecond)
	said := strings.Join(n.all(), "\n")
	if len(n.all()) != 2 || !strings.Contains(said, "2 tasks were waiting for your OK") || !strings.Contains(said, "Chase refund (7), Book dinner (8)") || !strings.Contains(said, `"retry task 9"`) {
		t.Fatalf("owner told %q", n.all())
	}
	for _, id := range []string{"7", "8", "9"} {
		if got := view(m, id).Status; got != Paused {
			t.Fatalf("task %s is %s", id, got)
		}
	}
}
