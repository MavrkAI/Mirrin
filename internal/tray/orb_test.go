package tray

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/events"
)

type orbTwin struct {
	bus     *events.Bus
	pending atomic.Int32
}

func (o *orbTwin) UIURL() string       { return "http://127.0.0.1:7742/ui?token=x" }
func (o *orbTwin) Events() *events.Bus { return o.bus }
func (o *orbTwin) Status(context.Context) api.Status {
	return api.Status{Pending: int(o.pending.Load())}
}

// orbCalls records what the driver asked the native orb to do.
type orbCalls struct {
	mu          sync.Mutex
	shows, hide int
}

func watchOrb(t *testing.T) *orbCalls {
	t.Helper()
	c := &orbCalls{}
	oldDelay, oldPre, oldShow, oldHide := orbStartDelay, orbPreload, orbShow, orbHide
	orbStartDelay = 0
	orbPreload = func(string, int, int, int) {}
	orbShow = func(string, int, int, int) { c.mu.Lock(); c.shows++; c.mu.Unlock() }
	orbHide = func() { c.mu.Lock(); c.hide++; c.mu.Unlock() }
	t.Cleanup(func() { orbStartDelay, orbPreload, orbShow, orbHide = oldDelay, oldPre, oldShow, oldHide })
	return c
}

func (c *orbCalls) counts() (int, int) { c.mu.Lock(); defer c.mu.Unlock(); return c.shows, c.hide }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A new request shows the orb at once, with its card beside it; it used to
// wait for the next status refresh.
func TestAnApprovalShowsTheOrbAtOnce(t *testing.T) {
	calls := watchOrb(t)
	twin := &orbTwin{bus: events.New()}
	var on atomic.Bool
	on.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orbDriver(ctx, twin, &on)
	time.Sleep(50 * time.Millisecond) // subscribed
	twin.pending.Store(1)
	twin.bus.Publish(events.Event{Kind: "approval", Text: "Text Sam the invoice?"})
	eventually(t, "the orb shown for the request", func() bool { s, _ := calls.counts(); return s == 1 })

	// Decided: the orb goes quiet a moment later.
	twin.pending.Store(0)
	twin.bus.Publish(events.Event{Kind: "approval", Text: "approved"})
	eventually(t, "the orb hidden after the decision", func() bool { _, h := calls.counts(); return h == 1 })
}

// A page handed over shows the orb, so the owner can click through to it.
func TestAHandOverShowsTheOrb(t *testing.T) {
	calls := watchOrb(t)
	twin := &orbTwin{bus: events.New()}
	var on atomic.Bool
	on.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orbDriver(ctx, twin, &on)
	time.Sleep(50 * time.Millisecond) // subscribed
	twin.bus.Publish(events.Event{Kind: "browser", Text: "handover", Data: map[string]string{"ask": "Solve the check"}})
	eventually(t, "the orb shown for the hand-over", func() bool { s, _ := calls.counts(); return s == 1 })
}

// Opening the laptop no longer pops the orb to say a channel is
// reconnecting: a system notice stays on the screen. What the twin did on
// its own (a briefing, a reminder) does pop it.
func TestOnlyAMessagePopsTheOrb(t *testing.T) {
	calls := watchOrb(t)
	twin := &orbTwin{bus: events.New()}
	var on atomic.Bool
	on.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orbDriver(ctx, twin, &on)
	time.Sleep(50 * time.Millisecond) // subscribed
	twin.bus.Publish(events.Event{Kind: "notice", Text: "Telegram is reconnecting: can't connect to the server"})
	time.Sleep(100 * time.Millisecond)
	if s, _ := calls.counts(); s != 0 {
		t.Fatalf("a system notice showed the orb (%d times)", s)
	}
	twin.bus.Publish(events.Event{Kind: "message", Text: "Good morning. Dentist at 3.", Data: map[string]any{"title": "Morning briefing"}})
	eventually(t, "the orb shown for the briefing", func() bool { s, _ := calls.counts(); return s == 1 })
}
