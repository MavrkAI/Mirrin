package channels

import (
	"testing"
	"time"
)

func TestBacklogDropsOnlyTheReplayAtConnect(t *testing.T) {
	start := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	clock := start
	b := &Backlog{Now: func() time.Time { return clock }}

	// Before any connect, the old age check stands.
	if !b.Stale(start.Add(-5 * time.Minute)) {
		t.Fatal("an old message before connect should be stale")
	}
	b.Connected()
	if !b.Stale(start.Add(-5 * time.Minute)) {
		t.Fatal("a 5-minute-old backlog message should be dropped")
	}
	if b.Stale(start.Add(-time.Minute)) {
		t.Fatal("a message inside the window should be kept")
	}
	if b.Stale(time.Time{}) {
		t.Fatal("a message without a time should be kept")
	}

	// A live message sent at connect but delivered 5 minutes late.
	clock = start.Add(5 * time.Minute)
	if b.Stale(start) {
		t.Fatal("a delayed live message should be kept")
	}
	// Even one sent just before connect, once the backlog is over.
	if b.Stale(start.Add(-time.Second)) {
		t.Fatal("after the backlog settles nothing is dropped")
	}

	// Done ends the backlog early.
	clock = start.Add(10 * time.Minute)
	b.Connected()
	b.Done()
	if b.Stale(start) {
		t.Fatal("after Done nothing is dropped")
	}
	// A reconnect starts a new backlog.
	b.Connected()
	if !b.Stale(start) {
		t.Fatal("a reconnect should drop the new replay")
	}
}

func TestBacklogWindow(t *testing.T) {
	start := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	b := &Backlog{Window: time.Minute, Now: func() time.Time { return start }}
	b.Connected()
	if !b.Stale(start.Add(-90 * time.Second)) {
		t.Fatal("a custom window should apply")
	}
}

// A transport that says when its backlog is over keeps dropping replayed
// history past the 30-second settle (a long offline sync), until Done.
func TestBacklogUntilDoneOutlastsTheSettle(t *testing.T) {
	start := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	clock := start
	b := &Backlog{Now: func() time.Time { return clock }}
	b.ConnectedUntilDone()
	clock = start.Add(2 * time.Minute)
	if !b.Stale(start.Add(-time.Hour)) {
		t.Fatal("history still syncing two minutes in should be dropped")
	}
	if b.Stale(start.Add(time.Second)) {
		t.Fatal("a live message should be kept")
	}
	b.Done()
	if b.Stale(start.Add(-time.Hour)) {
		t.Fatal("after Done nothing is dropped")
	}
	// A plain Connected goes back to the 30-second settle.
	b.Connected()
	clock = clock.Add(time.Minute)
	if b.Stale(start.Add(-time.Hour)) {
		t.Fatal("a plain Connected should settle after 30 seconds")
	}
}
