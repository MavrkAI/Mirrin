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

func TestTaskLifecycle(t *testing.T) {
	var mu sync.Mutex
	var said []string
	saved := ""
	reg := tools.NewRegistry()
	var m *Manager
	// A fake model: plans, does a step, asks the user, then (on resume) finishes.
	run := func(ctx context.Context, task *Task, input string) (string, error) {
		call := func(args string) {
			_, err := reg.Run(ctx, "task_update", tools.Call{ChatKey: task.Key, Input: json.RawMessage(args)})
			if err != nil {
				t.Errorf("task_update: %v", err)
			}
		}
		if strings.Contains(input, "started by the user") {
			call(`{"add_steps":"[\"find flights\",\"pick hotel\"]"}`)
			call(`{"step_done":1,"note":"Jetstar Tue 6am, $240"}`)
			call(`{"ask_user":"Pool or beachfront?"}`)
			return "asked", nil
		}
		call(`{"step_done":2,"finish":"Booked the beachfront place. Flights Tue 6am."}`)
		return "done", nil
	}
	m = New(context.Background(), Deps{
		Run: run,
		Notify: func(_ context.Context, key, text string) error {
			mu.Lock()
			said = append(said, key+": "+text)
			mu.Unlock()
			return nil
		},
		Load: func(context.Context) (string, error) { return saved, nil },
		Save: func(_ context.Context, d string) error { saved = d; return nil },
	})
	reg.Register(m.Tools()...)

	out, err := reg.Run(context.Background(), "start_task", tools.Call{ChatKey: "whatsapp:me", Input: json.RawMessage(`{"title":"Bali trip","goal":"book it"}`)})
	if err != nil || !strings.Contains(out, "started task") {
		t.Fatalf("start: %v %q", err, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	var task *Task
	for time.Now().Before(deadline) {
		if w := m.WaitingOn("whatsapp:me", time.Minute); w != nil {
			task = w
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if task == nil || task.Question != "Pool or beachfront?" || !task.Steps[0].Done || task.Steps[1].Done {
		t.Fatalf("expected waiting_user with board: %+v", task)
	}
	// Persisted, and the board survives a reload.
	m2 := New(context.Background(), Deps{Load: func(context.Context) (string, error) { return saved, nil }})
	if got, ok := m2.Get(task.ID); !ok || got.Status != WaitingUser || len(got.Steps) != 2 {
		t.Fatalf("reload: %+v", got)
	}
	m.drive(context.Background(), task, "[The user replied: beachfront]")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if view(m, task.ID).Status == Done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := view(m, task.ID)
	if got.Status != Done || !strings.Contains(got.Result, "beachfront") {
		t.Fatalf("finish: %+v", got)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(said) != 2 || !strings.Contains(said[0], "Pool or beachfront?") || !strings.Contains(said[1], "done.") {
		t.Fatalf("notifications: %v", said)
	}
}
