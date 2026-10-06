package channels

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"
)

// State is where a channel's connection stands.
type State string

const (
	Connecting State = "connecting"
	Connected  State = "connected"
	Failed     State = "failed"
)

// Status is a channel's connection as the owner should see it.
type Status struct {
	State State
	// Err is the last thing that went wrong ("" once healthy again).
	Err   string
	Since time.Time
}

// Reporter is a Channel that knows whether it is really connected, not just
// running. Channels without it are taken to be connected while Start runs.
type Reporter interface {
	Status() Status
}

// Tracker records a channel's connection state. Embed it to implement Reporter.
type Tracker struct {
	smu sync.Mutex
	st  Status
}

func (t *Tracker) set(s State, err error) {
	t.smu.Lock()
	defer t.smu.Unlock()
	msg := ""
	if err != nil {
		msg = Plain(err)
	}
	if t.st.State != s {
		t.st.Since = time.Now()
	}
	t.st.State, t.st.Err = s, msg
}

// Up marks the channel connected.
func (t *Tracker) Up() { t.set(Connected, nil) }

// Retrying marks the channel as reconnecting after err.
func (t *Tracker) Retrying(err error) { t.set(Connecting, err) }

// Down marks the channel as failed with err; it stays down until restarted.
func (t *Tracker) Down(err error) { t.set(Failed, err) }

// Status implements Reporter. Until something is recorded it reads Connecting.
func (t *Tracker) Status() Status {
	t.smu.Lock()
	defer t.smu.Unlock()
	st := t.st
	if st.State == "" {
		st.State = Connecting
	}
	return st
}

type fatalError struct{ err error }

func (e fatalError) Error() string { return e.err.Error() }
func (e fatalError) Unwrap() error { return e.err }

// Fatal marks an error that retrying won't fix, such as a rejected token or a
// setting only the owner can change. The daemon stops the channel and shows
// the error instead of reconnecting.
func Fatal(err error) error {
	if err == nil {
		return nil
	}
	return fatalError{err}
}

// IsFatal reports whether err, or anything it wraps, was marked Fatal.
func IsFatal(err error) bool {
	var f fatalError
	return errors.As(err, &f)
}

// Plain turns a connection error into a short line for the Channels page:
// network trouble reads as network trouble, not as a Go error chain.
func Plain(err error) string {
	if err == nil {
		return ""
	}
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case IsFatal(err):
	case errors.Is(err, context.DeadlineExceeded):
		return "the server took too long to answer"
	case errors.As(err, &dns):
		return "can't reach " + dns.Name + "; is this computer online?"
	case errors.As(err, &op) && op.Op == "dial":
		return "can't connect to the server; is this computer online?"
	}
	return strings.TrimSpace(err.Error())
}

// StableAfter is how long a connection must stay up before the next drop is
// treated as a fresh one and retried quickly.
const StableAfter = time.Minute

// Backoff spaces out reconnect attempts. Delays double from Min to Max with
// jitter, so many clients don't come back in lockstep, and start over once a
// connection has stayed up for StableAfter: a routine server-side disconnect
// after hours online is healed in about a second, not half a minute.
type Backoff struct {
	Min, Max time.Duration
	next     time.Duration
}

// Next returns the delay before the next attempt (within 20% of the nominal one).
func (b *Backoff) Next() time.Duration {
	if b.Min <= 0 {
		b.Min = time.Second
	}
	if b.Max < b.Min {
		b.Max = b.Min
	}
	if b.next < b.Min {
		b.next = b.Min
	}
	d := b.next
	b.next *= 2
	if b.next > b.Max {
		b.next = b.Max
	}
	spread := int64(d) / 5
	return d - time.Duration(spread) + time.Duration(rand.Int64N(2*spread+1))
}

// Reset starts the delays over from Min.
func (b *Backoff) Reset() { b.next = 0 }

// Wait sleeps before the next attempt. If the attempt that began at since
// stayed up for StableAfter the delays start over first. It returns false if
// ctx ends while waiting.
func (b *Backoff) Wait(ctx context.Context, since time.Time) bool {
	if !since.IsZero() && time.Since(since) >= StableAfter {
		b.Reset()
	}
	t := time.NewTimer(b.Next())
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
