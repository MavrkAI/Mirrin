package heartbeat

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/memory"
)

// A reminder goes out with what Related adds, and Fired hears of each one
// once it has gone, whether alone, late with others, or after a pause.
func TestRelatedAndFiredFollowAReminder(t *testing.T) {
	ctx := context.Background()
	h, store, o := setup(t, nil, "telegram:1")
	var fired []int64
	h.Related = func(_ context.Context, r memory.Reminder) string {
		if r.Text == "Mum's birthday is on the 12th (tomorrow)" {
			return "\nYou also told me: Mum loves orchids."
		}
		return ""
	}
	h.Fired = func(_ context.Context, r memory.Reminder) { fired = append(fired, r.ID) }
	a, _ := store.AddReminder(ctx, "telegram:1", time.Now().Add(-time.Second), "Mum's birthday is on the 12th (tomorrow)")
	b, _ := store.AddReminder(ctx, "whatsapp:2", time.Now().Add(-time.Second), "water the plants")
	h.fireReminders(ctx)
	got := o.messages()
	if !slices.Contains(got, "telegram:1 Reminder: Mum's birthday is on the 12th (tomorrow)\nYou also told me: Mum loves orchids.") ||
		!slices.Contains(got, "whatsapp:2 Reminder: water the plants") {
		t.Fatalf("delivered %q", got)
	}
	if len(fired) != 2 || !slices.Contains(fired, a) || !slices.Contains(fired, b) {
		t.Fatalf("Fired heard %v, want %d and %d", fired, a, b)
	}

	// Late ones together, and the backlog after a pause, are followed too.
	fired = nil
	c, _ := store.AddReminder(ctx, "telegram:1", time.Now().Add(-3*time.Hour), "one")
	d, _ := store.AddReminder(ctx, "telegram:1", time.Now().Add(-2*time.Hour), "two")
	h.fireReminders(ctx)
	if !slices.Contains(fired, c) || !slices.Contains(fired, d) {
		t.Fatalf("late ones: Fired heard %v", fired)
	}
	fired = nil
	h.SetPaused(true)
	e, _ := store.AddReminder(ctx, "telegram:1", time.Now().Add(-time.Second), "three")
	h.SetPaused(false)
	h.resume(ctx, time.Now())
	if !slices.Contains(fired, e) {
		t.Fatalf("after a pause: Fired heard %v", fired)
	}
}
