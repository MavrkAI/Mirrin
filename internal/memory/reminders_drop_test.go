package memory

import (
	"context"
	"testing"
	"time"
)

// DropReminder removes a reminder that has gone out too, where
// CancelReminder leaves it.
func TestDropReminderRemovesOneThatWentOut(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	id, _ := s.AddReminder(ctx, "screen:local", time.Now().Add(-time.Minute), "Mum's birthday is on the 12th (tomorrow)")
	_ = s.MarkFired(ctx, id)
	_ = s.CancelReminder(ctx, id)
	count := func() int {
		var n int
		_ = s.db.QueryRow(`SELECT count(*) FROM reminders WHERE id=?`, id).Scan(&n)
		return n
	}
	if count() != 1 {
		t.Fatal("cancel removed a delivered reminder")
	}
	if err := s.DropReminder(ctx, id); err != nil || count() != 0 {
		t.Fatalf("drop: %v, %d left", err, count())
	}
}
