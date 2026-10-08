package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// The first look at the inbox waits its turn on a first day. Google is
// often connected from the welcome page before the twin's first hello, and
// the hello ends with the briefing offer, which a bare "yes" on the screen
// answers. So the look says nothing until the hello is said and the offer
// is answered, for at most inboxFirstHoldMax; and it doesn't offer to come
// every morning when a morning briefing, which reads mail too, runs or was
// offered.

// inboxFirstHoldMax is the longest the first look waits for the hello and
// the offer; inboxFirstPoll is how often it checks. Variables so tests can
// stand in.
var (
	inboxFirstHoldMax = 30 * time.Minute
	inboxFirstPoll    = 2 * time.Second
)

// holdInboxFirst waits while the first hello or its briefing offer is
// still to come. It reports false when the twin stopped, or the owner said
// "skip the inbox" meanwhile. A wait restarts the claim's clock, so "skip
// the inbox" still counts for the minute after the heads-up.
func (d *Daemon) holdInboxFirst(ctx context.Context) bool {
	until := time.Now().Add(inboxFirstHoldMax)
	waited := false
	for d.inboxFirstHeld(ctx) && time.Now().Before(until) {
		waited = true
		select {
		case <-ctx.Done():
			return false
		case <-time.After(inboxFirstPoll):
		}
	}
	if !waited {
		return true
	}
	if off, _ := d.store.Get(ctx, inboxFirstOffKey); off == "1" {
		return false
	}
	_ = d.store.Set(ctx, inboxFirstKey, time.Now().UTC().Format(time.RFC3339))
	return true
}

// inboxFirstHeld reports whether the first hello is still to come on a new
// install, or its briefing offer still waits for an answer.
func (d *Daemon) inboxFirstHeld(ctx context.Context) bool {
	if d.helloToCome(ctx) {
		return true
	}
	if state, _ := d.store.Get(ctx, briefingOfferKey); state == "offered" {
		_, waiting := d.pendingBriefing(ctx)
		return waiting
	}
	return false
}

// helloToCome reports whether this install, under a day old, has yet to
// say its first hello. Unlike NeedsFirstLook it starts no clock: a twin
// with no install time isn't on its first day.
func (d *Daemon) helloToCome(ctx context.Context) bool {
	if v, _ := d.store.Get(ctx, "first_look_at"); v != "" {
		return false
	}
	v, _ := d.store.Get(ctx, "installed_at")
	t, err := time.Parse(time.RFC3339, v)
	return err == nil && time.Since(t) < 24*time.Hour
}

// morningOffered reports whether the owner has, or has been offered, a
// morning briefing. It reads the inbox too, so the first look doesn't
// offer a second thing every morning.
func (d *Daemon) morningOffered(ctx context.Context) bool {
	if state, _ := d.store.Get(ctx, briefingOfferKey); state != "" {
		return true // offered, yes or later: asked once is enough
	}
	p, ok := protocols.Find(d.Protocols(), briefingName)
	return ok && p.IsEnabled() && strings.TrimSpace(p.Schedule) != ""
}
