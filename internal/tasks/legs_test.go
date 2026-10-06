package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// notes collects what the manager tells the owner.
type notes struct {
	mu   sync.Mutex
	said []string
}

func (n *notes) notify(_ context.Context, _, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.said = append(n.said, text)
	return nil
}

func (n *notes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.said...)
}

// view copies a task under the manager's lock, as the daemon's List does.
func view(m *Manager, id string) Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.tasks[id]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCancelStopsTheWorkInFlight(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan error, 1)
	n := &notes{}
	m := New(context.Background(), Deps{
		Run: func(ctx context.Context, _ *Task, _ string) (string, error) {
			close(started)
			<-ctx.Done() // a long browse that only a cancel can end
			stopped <- ctx.Err()
			return "", ctx.Err()
		},
		Notify: n.notify,
	})
	task, err := m.Start(context.Background(), "w:1", "Chase the refund", "get the money back")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := m.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if err == nil {
			t.Fatal("leg should see its context cancelled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel_task left the running leg going")
	}
	time.Sleep(50 * time.Millisecond)
	if got := view(m, task.ID); got.Status != Cancelled {
		t.Fatalf("status: %s", got.Status)
	}
	if said := n.all(); len(said) != 0 {
		t.Fatalf("a cancelled task shouldn't report a problem: %v", said)
	}
}

func TestShutdownLeavesTheTaskToResume(t *testing.T) {
	started := make(chan struct{})
	n := &notes{}
	m := New(context.Background(), Deps{
		Run: func(ctx context.Context, _ *Task, _ string) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
		Notify: n.notify,
	})
	ctx, stop := context.WithCancel(context.Background())
	task, _ := m.Start(ctx, "w:1", "Chase the refund", "get the money back")
	<-started
	stop() // the daemon is quitting
	waitFor(t, "leg to end", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.legs[task.ID].cancel == nil && m.legs[task.ID].queued == 0
	})
	if got := view(m, task.ID); got.Status != Running {
		t.Fatalf("want it left running for ResumeAll, got %s", got.Status)
	}
	if said := n.all(); len(said) != 0 {
		t.Fatalf("a shutdown isn't a problem to report: %v", said)
	}
}

func TestLegsOfOneTaskNeverOverlap(t *testing.T) {
	var running, most atomic.Int32
	release := make(chan struct{})
	m := New(context.Background(), Deps{
		Parallel: 4,
		Run: func(ctx context.Context, _ *Task, input string) (string, error) {
			now := running.Add(1)
			defer running.Add(-1)
			if now > most.Load() {
				most.Store(now)
			}
			if strings.Contains(input, "started by the user") {
				<-release
			}
			return "ok", nil
		},
	})
	task, _ := m.Start(context.Background(), "w:1", "Plan the trip", "Bali in March")
	waitFor(t, "first leg", func() bool { return running.Load() == 1 })
	// The user answers while the first leg is still going.
	m.Resume(context.Background(), task, func(ctx context.Context) (string, error) { return m.deps.Run(ctx, task, "answer") })
	time.Sleep(50 * time.Millisecond)
	if most.Load() != 1 {
		t.Fatal("second leg started while the first was running")
	}
	close(release)
	waitFor(t, "both legs", func() bool { return view(m, task.ID).Runs == 2 && running.Load() == 0 })
	if most.Load() != 1 {
		t.Fatalf("legs overlapped: %d at once", most.Load())
	}
}

// stepLimit mimics the agent's StepLimitError.
type stepLimit struct{ reply string }

func (e stepLimit) Error() string    { return e.reply }
func (e stepLimit) OutOfSteps() bool { return true }

func TestRunningOutOfStepsPausesInsteadOfFinishing(t *testing.T) {
	n := &notes{}
	var inputs []string
	var mu sync.Mutex
	m := New(context.Background(), Deps{
		Run: func(ctx context.Context, _ *Task, input string) (string, error) {
			mu.Lock()
			inputs = append(inputs, input)
			mu.Unlock()
			return "", stepLimit{"Found three plumbers; still need quotes from two."}
		},
		Notify: n.notify,
	})
	task, _ := m.Start(context.Background(), "w:1", "Find a plumber", "fix the leak under the sink")
	waitFor(t, "pause", func() bool { return view(m, task.ID).Status == Paused })
	waitFor(t, "notice", func() bool { return len(n.all()) == 1 })
	said := n.all()[0]
	if !strings.Contains(said, "ran out of steps") || !strings.Contains(said, "still need quotes") || !strings.Contains(said, "retry task "+task.ID) {
		t.Fatalf("owner should hear it stopped short and how to carry on: %q", said)
	}
	if strings.Contains(said, "done") {
		t.Fatalf("must not claim it is done: %q", said)
	}
	if err := m.Retry(context.Background(), task.ID); err != nil {
		t.Fatalf("a paused task should be retryable: %v", err)
	}
	waitFor(t, "retry leg", func() bool { mu.Lock(); defer mu.Unlock(); return len(inputs) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(inputs[1], "ran out of steps") || !strings.Contains(inputs[1], "Goal: fix the leak under the sink") {
		t.Fatalf("retry should restate the goal and why: %q", inputs[1])
	}
}

func TestKeepKeysCoversTasksThatCanCarryOn(t *testing.T) {
	m := New(context.Background(), Deps{})
	for id, st := range map[string]Status{"1": Running, "2": WaitingApproval, "3": Paused, "4": Failed, "5": Done} {
		m.tasks[id] = &Task{ID: id, Key: "w:1#task-" + id, Status: st}
	}
	got := strings.Join(m.KeepKeys(), ",")
	if got != "w:1#task-1,w:1#task-2,w:1#task-3,w:1#task-4" {
		t.Fatalf("got %s", got)
	}
}

func TestAFailedTaskSaysHowToCarryOn(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ran past the 30-minute limit", context.DeadlineExceeded, "I worked on this for 30 minutes without finishing, so I stopped. Say \"retry task %s\" and I'll carry on from the board."},
		{"a tool broke", errors.New("browser crashed"), "I hit a problem and stopped (browser crashed). Say \"retry task %s\" to try again."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := &notes{}
			m := New(context.Background(), Deps{
				Run:    func(context.Context, *Task, string) (string, error) { return "", c.err },
				Notify: n.notify,
			})
			task, _ := m.Start(context.Background(), "w:1", "Book the dentist", "a check-up next week")
			waitFor(t, "notice", func() bool { return len(n.all()) == 1 })
			if got, want := n.all()[0], "Book the dentist: "+fmt.Sprintf(c.want, task.ID); got != want {
				t.Fatalf("\n got %q\nwant %q", got, want)
			}
			if err := m.Retry(context.Background(), task.ID); err != nil {
				t.Fatalf("the notice offers a retry, so it should work: %v", err)
			}
		})
	}
}
