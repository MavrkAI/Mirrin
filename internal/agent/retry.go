package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// RetryQueue carries approval events to somewhere slow or unreliable (a
// push to the owner's phone) without holding up the turn that raised them.
// An OnApproval hook hands each event to Add, which never blocks; Run
// delivers them, one at a time and in order, and tries a failed one again
// after a pause that grows each time. A newer event for the same approval
// replaces one still waiting (a "resolved" push makes an unsent "needs you"
// pointless). It lives in memory: after a restart the screen and "/pending"
// still show what is waiting.
type RetryQueue struct {
	deliver func(ctx context.Context, e ApprovalEvent) error
	backoff []time.Duration
	limit   int
	log     *slog.Logger

	mu    sync.Mutex
	items []*retryItem
	wake  chan struct{}
}

type retryItem struct {
	e        ApprovalEvent
	attempts int
	due      time.Time
}

// DefaultBackoff is how long a RetryQueue waits before each new try: a
// phone that is briefly offline still hears within the hour, and then the
// event is given up (the screen still shows it).
var DefaultBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 45 * time.Minute}

// NewRetryQueue builds a queue that delivers with deliver. backoff is the
// wait before each retry (nil for DefaultBackoff); an event that fails once
// more than it has waits is dropped. limit caps what is held (0 for 256):
// past it the oldest event is dropped.
func NewRetryQueue(deliver func(ctx context.Context, e ApprovalEvent) error, backoff []time.Duration, limit int, log *slog.Logger) *RetryQueue {
	if backoff == nil {
		backoff = DefaultBackoff
	}
	if limit <= 0 {
		limit = 256
	}
	if log == nil {
		log = slog.Default()
	}
	return &RetryQueue{deliver: deliver, backoff: backoff, limit: limit, log: log, wake: make(chan struct{}, 1)}
}

// Add queues e for delivery now. It never blocks.
func (q *RetryQueue) Add(e ApprovalEvent) {
	q.mu.Lock()
	for i, it := range q.items {
		if it.e.ID == e.ID {
			q.items = append(q.items[:i], q.items[i+1:]...)
			break
		}
	}
	if len(q.items) >= q.limit {
		q.log.Warn("approval delivery: queue full, dropping the oldest", "id", q.items[0].e.ID, "status", q.items[0].e.Status)
		q.items = q.items[1:]
	}
	q.items = append(q.items, &retryItem{e: e, due: time.Now()})
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Len is how many events wait to be delivered.
func (q *RetryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Run delivers events until ctx ends.
func (q *RetryQueue) Run(ctx context.Context) {
	for {
		it, wait := q.next()
		if it == nil {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-q.wake:
			case <-timer.C:
			}
			timer.Stop()
			continue
		}
		err := q.deliver(ctx, it.e)
		if ctx.Err() != nil {
			return
		}
		q.done(it, err)
	}
}

// next takes the first event that is due, or says how long until one is.
func (q *RetryQueue) next() (*retryItem, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	wait := time.Hour
	now := time.Now()
	for i, it := range q.items {
		if !it.due.After(now) {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return it, 0
		}
		wait = min(wait, it.due.Sub(now))
	}
	return nil, wait
}

// done settles a delivery: dropped when it worked, can never work, or has
// failed too often; otherwise back in the queue for later, unless a newer
// event for the same approval has arrived meanwhile.
func (q *RetryQueue) done(it *retryItem, err error) {
	if err == nil {
		return
	}
	it.attempts++
	var perm *permanent
	if errors.As(err, &perm) || it.attempts > len(q.backoff) {
		q.log.Warn("approval delivery: giving up", "id", it.e.ID, "status", it.e.Status, "attempts", it.attempts, "err", err)
		return
	}
	wait := q.backoff[it.attempts-1]
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) && ra.RetryAfter() > wait {
		wait = ra.RetryAfter()
	}
	it.due = time.Now().Add(wait)
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, other := range q.items {
		if other.e.ID == it.e.ID {
			return // superseded while it was being tried
		}
	}
	if len(q.items) >= q.limit {
		return
	}
	q.items = append(q.items, it)
}

type permanent struct{ err error }

func (p *permanent) Error() string { return p.err.Error() }
func (p *permanent) Unwrap() error { return p.err }

// Permanent marks a delivery error that trying again can't fix (the phone's
// push subscription is gone): the event is dropped at once.
func Permanent(err error) error { return &permanent{err} }
