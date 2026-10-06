package tasks

import (
	"context"
	"fmt"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// cancellable reports why task id (t, found) can't be cancelled, or nil.
func cancellable(id string, t *Task, found bool) error {
	if !found {
		return fmt.Errorf("no task %s", id)
	}
	if !t.open() && t.Status != Paused {
		return fmt.Errorf("task %s is already %s", id, t.Status)
	}
	return nil
}

// cancelCheck refuses, before the owner is asked, to cancel a task that has
// already finished: the owner said yes to stopping a task twice over, only
// to hear it had been done all along.
func (m *Manager) cancelCheck(_ context.Context, call tools.Call) error {
	var in struct{ ID string }
	if err := tools.Decode(call, &in); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[in.ID]
	return cancellable(in.ID, t, ok)
}

// cancelSummary names the task in the approval ("Cancel the task "Book Bali
// flights"") instead of showing cancel_task(id=10011).
func (m *Manager) cancelSummary(call tools.Call) string {
	var in struct{ ID string }
	if err := tools.Decode(call, &in); err != nil {
		return "Cancel a task"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[in.ID]; ok && t.Title != "" {
		return fmt.Sprintf("Cancel the task %q", t.Title)
	}
	return "Cancel task " + in.ID
}

// openLine heads the task list with what is still open, so a finished task
// whose board ends "Present options" isn't read back as waiting on the owner.
func openLine(list []Task) string {
	var open []string
	for _, t := range list {
		if t.open() {
			open = append(open, fmt.Sprintf("%s %q (%s)", t.ID, t.Title, statusWords[t.Status]))
		}
	}
	if len(open) == 0 {
		return "Nothing is open: every task below has finished, failed or been cancelled.\n\n"
	}
	return "Still open: " + strings.Join(open, "; ") + ". Everything else below is over.\n\n"
}

var statusWords = map[Status]string{
	Running:         "running",
	WaitingApproval: "waiting for an approval",
	WaitingUser:     "waiting on the user",
}
