package daemon

import (
	"context"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
)

// ownerReplies answers the short things an owner says to the proactive
// moments before the message reaches approvals or the model: turning one
// off, skipping the first look at the inbox, or answering what the twin
// noticed. handled is false when none of them applies.
func (d *Daemon) ownerReplies(ctx context.Context, in channels.Inbound, text string, ev agent.Events) (reply string, handled bool, err error) {
	if reply, ok := d.clashSwitch(ctx, in, text); ok { // watchclash.go
		return reply, true, nil
	}
	if reply, ok, err := d.portraitNoticed(ctx, in, text, ev); ok { // portrait_noticed.go: "I've noticed…" with a hello
		return reply, true, err
	}
	if in.IsOwner && skipsInboxLook(text) && d.inboxFirstPending(ctx) { // inbox_first.go
		return d.skipInboxLook(ctx), true, nil
	}
	if reply, ok := d.weeklySwitch(ctx, in.IsOwner, text); ok { // weekly.go: "no more weekly notes"
		return reply, true, nil
	}
	if reply, ok := d.briefSwitch(in, text); ok { // meetingbrief.go: "no more meeting briefs"
		return reply, true, nil
	}
	if reply, ok := d.billsSwitch(ctx, in); ok { // bills.go: "watch my bills"
		return reply, true, nil
	}
	return "", false, nil
}
