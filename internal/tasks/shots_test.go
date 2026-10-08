package tasks

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// A task that browsed finished with a shortlist, and the chat it reported
// to never heard it had a screenshot: asked later to send it on WhatsApp,
// the twin said the task hadn't used the browser. Now the last screenshots
// go with the finish: said as before, kept in the chat's record, and on the
// task's board.
func TestAFinishedTaskCarriesItsLastScreenshots(t *testing.T) {
	type kept struct{ key, text, record string }
	var mu sync.Mutex
	var got []kept
	var plain []string
	reg := tools.NewRegistry()
	run := func(ctx context.Context, task *Task, _ string) (string, error) {
		switch task.Title {
		case "Flights to Male":
			_, err := reg.Run(ctx, "task_update", tools.Call{ChatKey: task.Key, Input: json.RawMessage(`{"finish":"Malaysia Airlines, A$1,547."}`)})
			return "", err
		case "Find a florist":
			return "Petals on High St opens at 9.", nil // stopped without finish
		}
		_, err := reg.Run(ctx, "task_update", tools.Call{ChatKey: task.Key, Input: json.RawMessage(`{"finish":"Done by phone."}`)})
		return "", err
	}
	m := New(context.Background(), Deps{
		Run: run,
		Notify: func(_ context.Context, _, text string) error {
			mu.Lock()
			plain = append(plain, text)
			mu.Unlock()
			return nil
		},
		NotifyKept: func(_ context.Context, key, text, record string) error {
			mu.Lock()
			got = append(got, kept{key, text, record})
			mu.Unlock()
			return nil
		},
		Shots: func(_ context.Context, key string) []string {
			switch {
			case strings.Contains(key, "#task-") && strings.HasPrefix(key, "voice:local"):
				return []string{"/d/browser-1-1.png", "/d/browser-2-1.png", "/d/browser-3-1.png"}
			case strings.HasPrefix(key, "telegram:me"):
				return []string{"/d/browser-9-1.png"}
			}
			return nil
		},
	})
	reg.Register(m.Tools()...)
	flights, _ := m.Start(context.Background(), "voice:local", "Flights to Male", "a shortlist")
	florist, _ := m.Start(context.Background(), "telegram:me", "Find a florist", "near home")
	phone, _ := m.Start(context.Background(), "whatsapp:me", "Ring the plumber", "book him")
	waitFor(t, "every task to report", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got)+len(plain) == 3
	})

	if f := view(m, flights.ID); !slices.Equal(f.Shots, []string{"/d/browser-2-1.png", "/d/browser-3-1.png"}) {
		t.Fatalf("the task kept %v, want its last two", f.Shots)
	}
	if b := view(m, flights.ID).Board(); !strings.Contains(b, "screenshot: /d/browser-3-1.png") {
		t.Fatalf("list_tasks doesn't show the screenshot:\n%s", b)
	}
	if f := view(m, florist.ID); !slices.Equal(f.Shots, []string{"/d/browser-9-1.png"}) {
		t.Fatalf("a task that stopped without finish kept %v", f.Shots)
	}
	if p := view(m, phone.ID); len(p.Shots) != 0 {
		t.Fatalf("a task with no screenshots kept %v", p.Shots)
	}
	mu.Lock()
	defer mu.Unlock()
	slices.SortFunc(got, func(a, b kept) int { return strings.Compare(a.key, b.key) })
	if len(got) != 2 || len(plain) != 1 || strings.Contains(plain[0], "screenshot") {
		t.Fatalf("kept %+v, plain %q", got, plain)
	}
	if got[0].key != "telegram:me" || got[1].key != "voice:local" {
		t.Fatalf("reported to %+v", got)
	}
	v := got[1]
	if strings.Contains(v.text, "screenshot") || !strings.Contains(v.text, "A$1,547") {
		t.Fatalf("what is said (out loud, here) changed: %q", v.text)
	}
	if v.record != v.text+"\nscreenshot: /d/browser-2-1.png\nscreenshot: /d/browser-3-1.png" {
		t.Fatalf("the chat keeps %q", v.record)
	}
}
