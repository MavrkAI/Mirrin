package tasks

import (
	"context"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/events"
)

// maxShots is how many of a task's last screenshots go with its result.
const maxShots = 2

// keepShots notes on t the last screenshots it took (the twin's own, still
// on disk, as Deps.Shots finds them) as it finishes. Its conversation may be
// cleared soon after, so they are kept on the task itself: the chat it
// reports to, list_tasks and every turn's task state can then see them, and
// "send me the screenshot" finds one. Called with m.mu held.
func (m *Manager) keepShots(t *Task) {
	t.Shots = nil
	if m.deps.Shots == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shots := m.deps.Shots(ctx, t.Key)
	if len(shots) > maxShots {
		shots = shots[len(shots)-maxShots:]
	}
	t.Shots = append([]string(nil), shots...)
}

// ShotLines are a task's screenshots as the lines its result carries
// ("\nscreenshot: /path" each), "" when it has none.
func (t Task) ShotLines() string {
	var b strings.Builder
	for _, p := range t.Shots {
		b.WriteString("\nscreenshot: " + p)
	}
	return b.String()
}

// sayDone tells the owner a task finished. What is said is unchanged; the
// conversation it reaches also keeps the screenshot paths, so the twin
// there knows it has them and can pass one on. Called with m.mu held.
func (m *Manager) sayDone(t *Task, text string) {
	lines := t.ShotLines()
	if lines == "" || m.deps.NotifyKept == nil {
		m.say(t.Owner, text, t.source())
		return
	}
	owner, ctx := t.Owner, events.WithSource(context.Background(), t.source())
	go func() {
		if err := m.deps.NotifyKept(ctx, owner, text, text+lines); err != nil {
			m.deps.Log.Warn("tasks: notify", "err", err)
		}
	}()
}
