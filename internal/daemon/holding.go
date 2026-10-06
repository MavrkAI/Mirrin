package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// holdAfter is how long a turn on a messaging channel runs before the twin
// says it's on it, so a long one doesn't read as being ignored.
var holdAfter = 8 * time.Second

type holdKey struct{}

// hold is one turn's holding line: sent at most once, and never once
// anything else has gone out in the chat during the turn.
type hold struct {
	mu   sync.Mutex
	done bool
	key  string // the chat it is for
}

// holdOn arranges the holding line for the turn answering in: after
// holdAfter, unless the twin has said something in the chat by then, "On it
// — this one needs a minute." in the persona's voice. The returned func ends
// it (call it when the turn is over); so does the first message delivered
// to the chat during the turn (spoke). Email isn't a chat: it gets none.
func (d *Daemon) holdOn(ctx context.Context, in channels.Inbound) (context.Context, func()) {
	if in.Channel == "mail" || !channels.IsMessaging(in.Key()) {
		return ctx, func() {}
	}
	h := &hold{key: in.Key()}
	ctx = context.WithValue(ctx, holdKey{}, h)
	t := time.AfterFunc(holdAfter, func() { d.sendHold(ctx, h, in) })
	return ctx, func() {
		t.Stop()
		h.finish()
	}
}

// finish marks the holding line as no longer wanted. If it is being sent
// right now it waits for it, so it can never arrive after the reply.
func (h *hold) finish() {
	h.mu.Lock()
	h.done = true
	h.mu.Unlock()
}

// spoke notes that something is going out to chatKey in the turn behind
// ctx (the reply, or anything said on the way), so the holding line isn't.
func spoke(ctx context.Context, chatKey string) {
	if h, ok := ctx.Value(holdKey{}).(*hold); ok && h.key == chatKey {
		h.finish()
	}
}

// sendHold sends the holding line, unless the turn has ended or spoken.
// It goes out on the chat's own channel only: it is a courtesy, not worth
// reaching the owner elsewhere for.
func (d *Daemon) sendHold(ctx context.Context, h *hold, in channels.Inbound) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done || ctx.Err() != nil || d.paused.Load() {
		return
	}
	h.done = true
	ch, ok := d.channel(in.Channel)
	if !ok {
		return
	}
	line := d.holdingLine(in.IsOwner)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	d.store.Audit(sctx, "message.out", in.Key(), line)
	if err := ch.Send(sctx, in.ChatID, line); err != nil {
		d.log.Warn("holding line", "chat", in.Key(), "err", err)
		return
	}
	// Most apps clear "typing…" when a message arrives: show it again.
	if t, ok := ch.(channels.Typer); ok {
		_, _ = t.Typing(sctx, in.ChatID)
	}
}

// holdingLine is the holding line in the persona's voice: addressed the way
// the twin addresses the owner, plain for anyone else.
func (d *Daemon) holdingLine(owner bool) string {
	if address := d.address(); owner && address != "" { // address.go
		return "On it, " + address + " — this one needs a minute."
	}
	return "On it — this one needs a minute."
}
