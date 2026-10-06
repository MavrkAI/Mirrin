package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// OnOutcome hears once how each task ended: finished is Done, a problem
// is Failed. A question for the owner or a budget pause isn't an ending.
func TestOnOutcomeHearsHowATaskEnded(t *testing.T) {
	var mu sync.Mutex
	var heard []string
	reg := tools.NewRegistry()
	run := func(ctx context.Context, task *Task, _ string) (string, error) {
		switch task.Title {
		case "Book Ottolenghi":
			_, err := reg.Run(ctx, "task_update", tools.Call{ChatKey: task.Key, Input: json.RawMessage(`{"finish":"Booked. Table for two at 8."}`)})
			return "", err
		case "Move the dentist":
			return "Moved it to Thursday.", nil // stopped without finish: its last words are the result
		case "Chase the refund":
			return "", errors.New("the shop's site is down")
		case "Plan Bali":
			_, err := reg.Run(ctx, "task_update", tools.Call{ChatKey: task.Key, Input: json.RawMessage(`{"ask_user":"Pool or beachfront?"}`)})
			return "", err
		}
		return "", ErrOverBudget
	}
	m := New(context.Background(), Deps{Run: run, Notify: (&notes{}).notify,
		OnOutcome: func(task Task, s Status) {
			mu.Lock()
			heard = append(heard, fmt.Sprintf("%s: %s", task.Title, s))
			mu.Unlock()
		}})
	reg.Register(m.Tools()...)
	for _, title := range []string{"Book Ottolenghi", "Move the dentist", "Chase the refund", "Plan Bali", "Find a plumber"} {
		if _, err := m.Start(context.Background(), "screen:local", title, "do it"); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "every task to settle", func() bool {
		for _, task := range m.List() {
			if task.Status == Running {
				return false
			}
		}
		return true
	})
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(heard)
	want := []string{"Book Ottolenghi: done", "Chase the refund: failed", "Move the dentist: done"}
	if !slices.Equal(heard, want) {
		t.Fatalf("heard %s, want %s", strings.Join(heard, " | "), strings.Join(want, " | "))
	}
}
