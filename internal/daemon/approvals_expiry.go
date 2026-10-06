package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tasks"
)

// approvalTTL is how long a request waits for the owner's answer before it
// lapses, unless autonomy.approval_ttl says otherwise: long enough to answer
// the next day, or after a weekend away. By then what it was asked about
// has usually moved on (a price, a slot, a page), so the twin asks afresh
// rather than act on a stale yes. A lapsed request reads "expired", screens
// and phones hear it is settled, and a background task waiting on it is set
// aside for the owner to pick up again.
const approvalTTL = config.DefaultApprovalTTL

// ttl is how long a request waits for an answer: autonomy.approval_ttl.
func (d *Daemon) ttl() time.Duration { return d.Config().Autonomy.TTL() }

// lapsed reports whether a request has waited longer than the ttl.
func (d *Daemon) lapsed(ap memory.Approval) bool { return clock().Sub(ap.CreatedAt) > d.ttl() }

// lapseWhy is the audit log's reason for a lapsed request.
func (d *Daemon) lapseWhy() string {
	ttl := d.ttl()
	if days := int(ttl / (24 * time.Hour)); days >= 1 && ttl%(24*time.Hour) == 0 {
		return fmt.Sprintf("no answer in %d days", days)
	}
	return fmt.Sprintf("no answer in %s", config.Duration(ttl))
}

// expireApprovals lets every request that has waited too long lapse. The
// heartbeat runs it every quarter of an hour; a request answered in between
// lapses when it is answered (resolve). Tasks set aside by one sweep (the
// first after an upgrade may find many old requests) are told in one line
// per chat.
func (d *Daemon) expireApprovals(ctx context.Context) {
	aps, err := d.store.LapsedApprovals(ctx, clock().Add(-d.ttl()))
	if err != nil {
		d.log.Warn("approvals: expire", "err", err)
		return
	}
	var ls []tasks.Lapse
	for i := range aps {
		if l, ok := d.expire(ctx, &aps[i]); ok {
			ls = append(ls, l)
		}
	}
	d.tasks.LapsedAll(ls)
}

// lapse marks a request that waited too long as expired, and tells a task
// that was waiting on it.
func (d *Daemon) lapse(ctx context.Context, ap *memory.Approval) {
	if l, ok := d.expire(ctx, ap); ok {
		d.tasks.LapsedAll([]tasks.Lapse{l})
	}
}

// expire marks a request that waited too long as expired, and returns the
// task waiting on it, if any, for the caller to tell.
func (d *Daemon) expire(ctx context.Context, ap *memory.Approval) (tasks.Lapse, bool) {
	if err := d.agent.Settle(ctx, ap, "expired", d.lapseWhy()); err != nil {
		if cur, gerr := d.store.GetApproval(ctx, ap.ID); gerr == nil {
			*ap = *cur // decided meanwhile: that outcome stands
		}
		return tasks.Lapse{}, false
	}
	t, ok := d.tasks.ByKey(ap.ChatKey)
	if !ok {
		return tasks.Lapse{}, false
	}
	return tasks.Lapse{Task: t.ID, Request: label(*ap)}, true
}
