package daemon

import (
	"context"
	"fmt"

	"github.com/MavrkAI/Mirrin/internal/events"
)

// CancelTask drops a background task from the screen ("Drop" on its card).
func (d *Daemon) CancelTask(ctx context.Context, id string) error {
	t, ok := d.tasks.Get(id)
	if !ok {
		return fmt.Errorf("there's no task %s", id)
	}
	if err := d.tasks.Cancel(id); err != nil {
		return err
	}
	d.store.Audit(ctx, "task.cancelled", t.Key, "from the screen: "+t.Title)
	d.bus.Publish(events.Event{Kind: "notice", Text: "Dropped “" + t.Title + "”."})
	return nil
}
