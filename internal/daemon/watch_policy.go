package daemon

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

type watchNoticeKey struct{}

// runWatchTask keeps external changes behind the stranger gate, including
// approval continuations. The approval notice is the only message when a
// model calls a tool despite the task's instruction to offer a follow-up.
func (d *Daemon) runWatchTask(ctx context.Context, key, task string) (string, error) {
	var asked atomic.Bool
	ctx = context.WithValue(ctx, watchNoticeKey{}, &asked)
	out, err := d.runTask(agent.ForStranger(ctx, "the calendar or inbox watcher"), key, task)
	if asked.Load() && err == nil {
		return "NOTHING_TO_REPORT", nil
	}
	return out, err
}

// backgroundAsked uses the agent's existing approval callback without
// changing the stranger gate or the handling of other people's messages.
// Identify watchers by their internal conversation key, never a sender name.
func (d *Daemon) backgroundAsked(ctx context.Context, ap memory.Approval, who string) {
	if !memory.IsWatchRun(ap.ChatKey) {
		d.strangerAsked(ctx, ap, who)
		return
	}
	if asked, ok := ctx.Value(watchNoticeKey{}).(*atomic.Bool); ok {
		asked.Store(true)
	}
	owner := d.ownerChatKey()
	text := fmt.Sprintf("To follow up on a calendar or inbox change, I'd like to %s\n%s", ap.Summary, answerHint(owner, ap.ID))
	ctx = context.WithoutCancel(ctx)
	if owner == "" {
		// Data marks it as the owner's business, which a wall screen doesn't show.
		d.bus.Publish(events.Event{Kind: "notice", Text: text, Data: map[string]string{"channel": channelOf(ap.ChatKey)}})
		if err := desktopNotify(d.cfg.Name, text); err != nil {
			d.log.Warn("tell owner about a watcher request", "err", err)
		}
		return
	}
	record := fmt.Sprintf("(I asked you to approve %s to follow up on a calendar or inbox change, as request #%d.)", ap.Tool, ap.ID)
	if err := d.notify(ctx, owner, text, record); err != nil {
		d.log.Warn("tell owner about a watcher request", "chat", owner, "err", err)
	}
}
