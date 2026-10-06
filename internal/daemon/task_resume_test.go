package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tasks"
)

// seedTasks gives the daemon a task manager holding list, as if it had been
// saved before a restart, wired to the budget check as the real one is.
func seedTasks(t *testing.T, td *testDaemon, list []tasks.Task) {
	t.Helper()
	data, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	td.tasks = tasks.New(context.Background(), tasks.Deps{
		Run: func(ctx context.Context, tk *tasks.Task, input string) (string, error) {
			return td.budgetTask(ctx, tk.Key, input)
		},
		Notify: td.Notify,
		Load:   func(context.Context) (string, error) { return string(data), nil },
	})
}

// taskStatus reads a task's status under the manager's lock.
func taskStatus(td *testDaemon, id string) tasks.Status {
	for _, v := range td.tasks.List() {
		if v.ID == id {
			return v.Status
		}
	}
	return ""
}

// pickedUp is what the owner was told about tasks picked back up.
func pickedUp(td *testDaemon) []string {
	var out []string
	for _, m := range td.ch.messages() {
		if strings.Contains(m, "room again") {
			out = append(out, m)
		}
	}
	return out
}

// "Plan the Bali trip" ran out of monthly budget. When the budget has room
// again it carries on by itself, and the owner hears so once. A task that
// ran out of steps, or whose approval lapsed, waits for Try again.
func TestBudgetPausedTasksCarryOnOnce(t *testing.T) {
	td := budgetTestDaemon(t)
	old := desktopNotify
	desktopNotify = func(string, string) error { return nil }
	t.Cleanup(func() { desktopNotify = old })
	ctx := context.Background()
	exhaustBudget(t, td)
	bali, err := td.tasks.Start(ctx, ownerKey, "Plan the Bali trip", "Ten days in March")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the budget pause", func() bool {
		for _, v := range td.tasks.List() {
			if v.ID == bali.ID && v.Status == tasks.Paused {
				if v.PausedBy != tasks.PausedBudget {
					t.Fatalf("paused by %q", v.PausedBy)
				}
				return true
			}
		}
		return false
	})
	saved := td.tasks.List()
	set := time.Now().Add(-time.Hour)
	seedTasks(t, td, append(saved,
		tasks.Task{ID: "7", Owner: ownerKey, Key: ownerKey + "#task-7", Title: "Chase the refund", Status: tasks.Paused, PausedBy: tasks.PausedLapsed, PausedFor: "the user didn't answer your request to approve send: to shop in time", Updated: set},
		tasks.Task{ID: "8", Owner: ownerKey, Key: ownerKey + "#task-8", Title: "Find a plumber", Status: tasks.Paused, PausedBy: tasks.PausedSteps, Updated: set},
	))

	td.resumeBudgetPaused(ctx) // still over budget
	time.Sleep(50 * time.Millisecond)
	if got := taskStatus(td, bali.ID); got != tasks.Paused {
		t.Fatalf("picked up over budget: %s", got)
	}
	if len(pickedUp(td)) != 0 || len(td.llm.heard()) != 0 {
		t.Fatalf("over budget, but told %q, model heard %q", pickedUp(td), td.llm.heard())
	}

	if err := td.UpdateConfig(func(c *config.Config) { c.Usage.MonthlyBudget = 2 }); err != nil {
		t.Fatal(err)
	}
	td.resumeBudgetPaused(ctx)
	td.resumeBudgetPaused(ctx) // the next hour: nothing more
	eventually(t, "the task carrying on", func() bool { return taskStatus(td, bali.ID) == tasks.Done })
	time.Sleep(50 * time.Millisecond)
	said := pickedUp(td)
	if len(said) != 1 || said[0] != "owner: The model budget has room again, so I've picked “Plan the Bali trip” back up." {
		t.Fatalf("owner told %q", said)
	}
	for _, id := range []string{"7", "8"} {
		if got := taskStatus(td, id); got != tasks.Paused {
			t.Fatalf("task %s carried on by itself: %s", id, got)
		}
	}
	if v, _ := td.store.Get(ctx, "task.resumed."+bali.ID); v == "" {
		t.Fatal("the pick-up wasn't recorded")
	}
}

// Several tasks picked up in one chat are told in one line.
func TestBudgetPausedTasksAreToldTogether(t *testing.T) {
	td := budgetTestDaemon(t)
	now := time.Now()
	seedTasks(t, td, []tasks.Task{
		{ID: "1", Owner: ownerKey, Key: ownerKey + "#task-1", Title: "Plan the Bali trip", Status: tasks.Paused, PausedBy: tasks.PausedBudget, Updated: now.Add(-time.Hour)},
		{ID: "2", Owner: ownerKey, Key: ownerKey + "#task-2", Title: "Renew the permit", Status: tasks.Paused, PausedBy: tasks.PausedBudget, Updated: now.Add(-2 * time.Hour)},
	})
	td.resumeBudgetPaused(context.Background())
	eventually(t, "the notice", func() bool { return len(pickedUp(td)) > 0 })
	time.Sleep(50 * time.Millisecond)
	if said := pickedUp(td); len(said) != 1 || said[0] != "owner: The model budget has room again, so I've picked “Plan the Bali trip” and “Renew the permit” back up." {
		t.Fatalf("owner told %q", said)
	}
}

// A task set aside stays on the screen for a week, not two hours.
func TestScreenKeepsATaskSetAside(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	off := false
	if err := td.UpdateConfig(func(c *config.Config) { c.UI.Weather = &off }); err != nil { // no call to the weather service
		t.Fatal(err)
	}
	now := time.Now()
	seedTasks(t, td, []tasks.Task{
		{ID: "1", Title: "Plan the Bali trip", Status: tasks.Paused, PausedBy: tasks.PausedBudget, Updated: now.Add(-3 * 24 * time.Hour)},
		{ID: "2", Title: "Book the dentist", Status: tasks.Done, Updated: now.Add(-3 * 24 * time.Hour)},
		{ID: "3", Title: "Last month's errand", Status: tasks.Paused, PausedBy: tasks.PausedSteps, Updated: now.Add(-8 * 24 * time.Hour)},
	})
	var titles []string
	for _, tk := range td.screenData(context.Background()).Tasks {
		titles = append(titles, tk.Title)
	}
	if strings.Join(titles, ",") != "Plan the Bali trip" {
		t.Fatalf("screen shows %q", titles)
	}
}
