package heartbeat

import (
	"context"

	"github.com/MavrkAI/Mirrin/internal/memory"
)

// A reminder the owner set with a tap from a fact ("Remind me on the
// 11th?", daemon/datereminder.go) brings back, as it goes out, what else
// the twin knows about the same person (a gift idea), and a birthday's
// comes round again next year. The daemon decides both through Related and
// Fired; anything else is delivered and settled as before.

// related is what goes under a reminder as it goes out: "" for most.
func (h *Heartbeat) related(ctx context.Context, r memory.Reminder) string {
	if h.Related == nil {
		return ""
	}
	return h.Related(ctx, r)
}

// markFired records that a reminder went out, then lets Fired follow it.
func (h *Heartbeat) markFired(ctx context.Context, r memory.Reminder) {
	_ = h.store.MarkFired(ctx, r.ID)
	if h.Fired != nil {
		h.Fired(ctx, r)
	}
}
