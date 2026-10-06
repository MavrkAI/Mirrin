package heartbeat

import (
	"context"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// "Skip tomorrow's briefing": the next scheduled run is skipped, and only
// that one. A run the owner asks for meanwhile is never skipped.
func TestSkipNextSkipsExactlyOneScheduledRun(t *testing.T) {
	ctx := context.Background()
	h, c, o, _ := timed(t, at(0, 8, 0))
	_ = h.LoadProtocols([]protocols.Protocol{brief})
	next := h.NextRun(brief)
	if !next.Equal(at(1, 7, 0)) {
		t.Fatalf("next run %v, want tomorrow at 07:00", next)
	}
	if err := h.store.Set(ctx, protocols.SkipKey(brief.Name), next.Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	h.RunProtocol(ctx, brief) // asked for: runs
	if n := len(o.messages()); n != 1 {
		t.Fatalf("a run the owner asks for is never skipped, got %d messages", n)
	}
	h.look(ctx)
	c.set(at(1, 7, 0))
	h.look(ctx)
	if n := len(o.messages()); n != 1 {
		t.Fatalf("tomorrow's run should be skipped, got %d messages", n)
	}
	if v, _ := h.store.Get(ctx, protocols.SkipKey(brief.Name)); v != "" {
		t.Fatalf("the skip should be used up, still %q", v)
	}
	c.set(at(2, 7, 0))
	h.look(ctx)
	if n := len(o.messages()); n != 2 {
		t.Fatalf("the run after carries on, got %d messages", n)
	}
	if off := (protocols.Protocol{Name: "x", Schedule: "0 7 * * *", Prompt: "p", Enabled: new(bool)}); !h.NextRun(off).IsZero() {
		t.Fatal("a routine that is off has no next run")
	}
}

// A skip left for a run that never came (the twin was off past it) is
// cleared by the next one, which runs.
func TestStaleSkipIsClearedAndTheRunGoesAhead(t *testing.T) {
	ctx := context.Background()
	h, c, o, _ := timed(t, at(2, 6, 59))
	_ = h.LoadProtocols([]protocols.Protocol{brief})
	_ = h.store.Set(ctx, protocols.SkipKey(brief.Name), at(0, 0, 0).Format(time.DateOnly))
	h.look(ctx)
	c.set(at(2, 7, 0))
	h.look(ctx)
	if n := len(o.messages()); n != 1 {
		t.Fatalf("an old skip must not hold back today's run, got %d messages", n)
	}
	if v, _ := h.store.Get(ctx, protocols.SkipKey(brief.Name)); v != "" {
		t.Fatalf("the old skip should be cleared, still %q", v)
	}
}
