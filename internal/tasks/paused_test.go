package tasks

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// Each way a task is set aside is recorded, so only one paused for the
// budget carries on by itself and the rest wait for Try again.
func TestPausedBySaysWhy(t *testing.T) {
	var mu sync.Mutex
	var inputs []string
	errs := []error{ErrOverBudget, stepLimit{"Found three plumbers."}}
	m := New(context.Background(), Deps{Run: func(_ context.Context, _ *Task, input string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		inputs = append(inputs, input)
		if len(errs) == 0 {
			return "carried on", nil
		}
		err := errs[0]
		errs = errs[1:]
		return "", err
	}})
	bali, _ := m.Start(context.Background(), "w:1", "Plan the Bali trip", "ten days in March")
	waitFor(t, "budget pause", func() bool { return view(m, bali.ID).Status == Paused })
	if got := view(m, bali.ID); got.PausedBy != PausedBudget || !strings.Contains(got.PausedFor, "budget") {
		t.Fatalf("%+v", got)
	}
	plumber, _ := m.Start(context.Background(), "w:1", "Find a plumber", "fix the leak")
	waitFor(t, "steps pause", func() bool { return view(m, plumber.ID).Status == Paused })
	if got := view(m, plumber.ID).PausedBy; got != PausedSteps {
		t.Fatalf("out of steps, paused by %q", got)
	}
	m.tasks["7"] = &Task{ID: "7", Owner: "w:1", Key: "w:1#task-7", Title: "Chase refund", Status: WaitingApproval}
	m.Lapsed("7", "send: to shop")
	if got := view(m, "7").PausedBy; got != PausedLapsed {
		t.Fatalf("lapsed, paused by %q", got)
	}

	if err := m.Retry(context.Background(), bali.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "retry leg", func() bool { mu.Lock(); defer mu.Unlock(); return len(inputs) == 3 })
	mu.Lock()
	in := inputs[2]
	mu.Unlock()
	if !strings.Contains(in, "monthly model budget was used up, and it has room again") || strings.Contains(in, "ask again") {
		t.Fatalf("retry told the model %q", in)
	}
	waitFor(t, "done", func() bool { return view(m, bali.ID).Status == Done })
	if got := view(m, bali.ID); got.PausedBy != "" || got.PausedFor != "" {
		t.Fatalf("the reason outlived the retry: %+v", got)
	}
}

// Tasks saved before PausedBy get one when loaded, so a task paused for the
// budget before this update still carries on by itself.
func TestPausedByForTasksSavedBefore(t *testing.T) {
	saved, _ := json.Marshal([]Task{
		{ID: "1", Status: Paused, PausedFor: budgetPausedFor},
		{ID: "2", Status: Paused, PausedFor: "the user didn't answer your request to approve send: to shop in time"},
		{ID: "3", Status: Paused},
		{ID: "4", Status: Done},
	})
	m := New(context.Background(), Deps{Load: func(context.Context) (string, error) { return string(saved), nil }})
	for id, want := range map[string]string{"1": PausedBudget, "2": PausedLapsed, "3": PausedSteps, "4": ""} {
		if got := view(m, id).PausedBy; got != want {
			t.Errorf("task %s paused by %q, want %q", id, got, want)
		}
	}
}

// A task paused for the budget isn't pruned with the finished ones: it can
// wait weeks for the month to turn.
func TestPruneKeepsATaskWaitingForTheBudget(t *testing.T) {
	m := New(context.Background(), Deps{})
	old := time.Now().Add(-10 * 24 * time.Hour)
	m.tasks["1"] = &Task{ID: "1", Status: Paused, PausedBy: PausedBudget, Updated: old}
	m.tasks["2"] = &Task{ID: "2", Status: Paused, PausedBy: PausedSteps, Updated: old}
	m.tasks["3"] = &Task{ID: "3", Status: Cancelled, PausedBy: PausedBudget, Updated: old}
	m.Prune(7 * 24 * time.Hour)
	if _, ok := m.Get("1"); !ok {
		t.Fatal("the budget-paused task was pruned")
	}
	for _, id := range []string{"2", "3"} {
		if _, ok := m.Get(id); ok {
			t.Fatalf("task %s outlived the prune", id)
		}
	}
}
