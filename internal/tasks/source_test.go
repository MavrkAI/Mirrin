package tasks

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// A task's news says what it is about, so the screen keeps its result under
// Left for you by the task's title, and shows its question as one (Needs you
// holds that until it is answered).
func TestTaskNewsSaysWhatItIsAbout(t *testing.T) {
	var mu sync.Mutex
	got := map[string]events.Source{}
	reg := tools.NewRegistry()
	run := func(ctx context.Context, task *Task, input string) (string, error) {
		call := func(args string) {
			if _, err := reg.Run(ctx, "task_update", tools.Call{ChatKey: task.Key, Input: json.RawMessage(args)}); err != nil {
				t.Errorf("task_update: %v", err)
			}
		}
		if strings.Contains(input, "started by the user") {
			call(`{"ask_user":"Window or aisle?"}`)
			return "asked", nil
		}
		call(`{"finish":"Booked a window seat."}`)
		return "done", nil
	}
	m := New(context.Background(), Deps{Run: run, Notify: func(ctx context.Context, _, text string) error {
		src, _ := events.SourceFrom(ctx)
		mu.Lock()
		got[text] = src
		mu.Unlock()
		return nil
	}})
	reg.Register(m.Tools()...)
	if _, err := reg.Run(context.Background(), "start_task", tools.Call{ChatKey: "screen:local", Input: json.RawMessage(`{"title":"Book the flight","goal":"a seat to Perth"}`)}); err != nil {
		t.Fatal(err)
	}
	var task *Task
	for deadline := time.Now().Add(5 * time.Second); task == nil && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		task = m.WaitingOn("screen:local", time.Minute)
	}
	if task == nil {
		t.Fatal("the task never asked")
	}
	m.drive(context.Background(), task, "[The user replied: window]")
	for deadline := time.Now().Add(5 * time.Second); view(m, task.ID).Status != Done && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // say delivers in the background
	mu.Lock()
	defer mu.Unlock()
	if src := got["Book the flight: Window or aisle?"]; src != (events.Source{Kind: "question", Name: "Book the flight"}) {
		t.Errorf("the question carries %+v (all: %v)", src, got)
	}
	if src := got["Book the flight: done. Booked a window seat."]; src != (events.Source{Kind: "task", Name: "Book the flight"}) {
		t.Errorf("the result carries %+v (all: %v)", src, got)
	}
}
