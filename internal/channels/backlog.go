package channels

import (
	"sync"
	"time"
)

// Backlog tells the messages a service replays when the twin connects from
// live ones, so a restart doesn't answer an hour-old queue while a live
// message that was merely slow to arrive still gets its answer.
//
// A message is stale only when all of these hold: it was sent before the
// connection opened, it is older than Window, and it arrived while the
// backlog was still coming in (before Done, and within Settle of the
// connect). Call Connected each time the transport (re)connects, or
// ConnectedUntilDone when it always says when the backlog has finished;
// Settle is then only a safety net (default 10 minutes). The zero
// value is ready to use; until Connected is called, every message older
// than Window is stale, as before.
type Backlog struct {
	// Window is how old a replayed message may be and still be answered
	// (default 2 minutes).
	Window time.Duration
	// Settle is how long after connecting the backlog may keep arriving
	// when the transport can't say it has finished (default 30 seconds,
	// or 10 minutes after ConnectedUntilDone).
	Settle time.Duration
	// Now is the clock, for tests (default time.Now).
	Now func() time.Time

	mu        sync.Mutex
	connected time.Time
	done      bool
	awaitDone bool
}

func (b *Backlog) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Connected records that the transport has just (re)connected: what arrives
// next is the backlog.
func (b *Backlog) Connected() {
	at := b.now()
	b.mu.Lock()
	b.connected, b.done, b.awaitDone = at, false, false
	b.mu.Unlock()
}

// ConnectedUntilDone is Connected for a transport that calls Done once its
// backlog has arrived, however long that takes (a large offline sync).
func (b *Backlog) ConnectedUntilDone() {
	at := b.now()
	b.mu.Lock()
	b.connected, b.done, b.awaitDone = at, false, true
	b.mu.Unlock()
}

// Done records that the transport has delivered its whole backlog.
func (b *Backlog) Done() {
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
}

// Stale reports whether a message sent at sent, arriving now, is replayed
// history to drop. A zero sent time is never stale.
func (b *Backlog) Stale(sent time.Time) bool {
	if sent.IsZero() {
		return false
	}
	window := b.Window
	if window <= 0 {
		window = 2 * time.Minute
	}
	now := b.now()
	if now.Sub(sent) <= window {
		return false
	}
	b.mu.Lock()
	connected, done, awaitDone := b.connected, b.done, b.awaitDone
	b.mu.Unlock()
	settle := b.Settle
	if settle <= 0 {
		settle = 30 * time.Second
		if awaitDone {
			settle = 10 * time.Minute
		}
	}
	if connected.IsZero() {
		return true
	}
	if done || now.Sub(connected) > settle {
		return false
	}
	return sent.Before(connected)
}
