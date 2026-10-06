package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/tasks"
)

// resumeBudgetPaused picks up the tasks set aside for the monthly model
// budget once it has room again (a new month, or a raised cap), as their
// pause promised, and tells the owner once in each chat. The heartbeat runs
// it every hour.
//
// Only these carry on by themselves: the owner started them and was told
// they would. A task that ran out of steps, or whose request for approval
// lapsed, waits for the owner to try it again.
//
// task.resumed.<id> keeps which pause was picked up, so a pause is resumed
// and told once, while a task the budget stops again later still carries on
// as it says.
func (d *Daemon) resumeBudgetPaused(ctx context.Context) {
	if d.tasks == nil {
		return
	}
	var waiting []tasks.Task
	for _, t := range d.tasks.List() {
		if t.Status == tasks.Paused && t.PausedBy == tasks.PausedBudget {
			waiting = append(waiting, t)
		}
	}
	if len(waiting) == 0 {
		return
	}
	if blocked, err := d.backgroundOverBudget(ctx); err != nil || blocked {
		return
	}
	var owners []string
	titles := map[string][]string{}
	for _, t := range waiting {
		key, pause := "task.resumed."+t.ID, t.Updated.UTC().Format(time.RFC3339Nano)
		if was, err := d.store.Get(ctx, key); err != nil || was == pause {
			continue
		}
		if err := d.tasks.Retry(d.runCtx, t.ID); err != nil {
			continue // cancelled or tried again in the meantime
		}
		if err := d.store.Set(ctx, key, pause); err != nil {
			d.log.Warn("tasks: resume", "err", err)
		}
		if _, ok := titles[t.Owner]; !ok {
			owners = append(owners, t.Owner)
		}
		titles[t.Owner] = append(titles[t.Owner], quoted(t.Title))
	}
	for _, o := range owners {
		text := "The model budget has room again, so I've picked " + andList(titles[o]) + " back up."
		if err := d.Notify(ctx, o, text); err != nil {
			d.log.Warn("tasks: resume notice", "err", err)
		}
	}
}

// andList joins names as a sentence does: "a", "a and b", "a, b and c".
func andList(names []string) string {
	if n := len(names); n > 1 {
		return strings.Join(names[:n-1], ", ") + " and " + names[n-1]
	}
	return strings.Join(names, "")
}
