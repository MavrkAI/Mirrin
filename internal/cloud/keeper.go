package cloud

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Refresh timing. A linked daemon asks for a new entitlement about once a
// day, at a random point in an eight-hour window so that daemons started
// together do not stay together. After a failure it waits a random time
// under a ceiling that doubles from five minutes to six hours. After the
// control plane has answered, it never asks again within refreshFloor,
// whatever the clocks say.
const (
	refreshEvery  = 20 * time.Hour
	refreshWindow = 8 * time.Hour
	refreshFloor  = time.Hour
	retryMin      = 5 * time.Minute
	retryMax      = 6 * time.Hour
)

// Keep refreshes the entitlement about once a day until ctx ends. It is the
// only thing in this package that contacts the control plane unprompted,
// and the daemon starts it only on a machine that has linked. It stops for
// good when the control plane says this machine is revoked or superseded,
// or when the link is removed.
func (c *Client) Keep(ctx context.Context, log *slog.Logger) {
	c.keep(ctx, log, time.After, rand.N[time.Duration])
}

// keep is Keep with the timer and the randomness passed in, for tests.
func (c *Client) keep(ctx context.Context, log *slog.Logger, after func(time.Duration) <-chan time.Time, random func(time.Duration) time.Duration) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	failures, answered := 0, false
	for {
		wait, err := c.untilRefresh(failures, random)
		if err != nil {
			log.Info("cloud: not refreshing", "err", err)
			return
		}
		if answered {
			wait = max(wait, refreshFloor)
		}
		select {
		case <-ctx.Done():
			return
		case <-after(wait):
		}
		_, err = c.Refresh(ctx)
		answered = err == nil || errors.Is(err, ErrLapsed)
		var sup *SupersededError
		switch {
		case err == nil:
			failures = 0
		case errors.Is(err, ErrLapsed):
			failures = 0
			log.Info("cloud: payment has lapsed; the entitlement lasts until it expires")
		case errors.As(err, &sup):
			log.Warn("cloud: another machine took over this handle; standing by", "gen", sup.Gen, "at", sup.At)
			return
		case errors.Is(err, ErrRevoked), errors.Is(err, ErrNotLinked):
			log.Warn("cloud: this machine's link has ended", "err", err)
			return
		case ctx.Err() != nil:
			return
		default:
			failures++
			log.Warn("cloud: entitlement refresh failed", "err", err, "failures", failures)
		}
	}
}

// untilRefresh is how long to wait before the next refresh: after a failure
// a jittered backoff, otherwise until about a day after the control plane
// last answered, by this machine's clock (never the token's iat, which is
// the server's). The wait is never longer than the refresh window's end, so
// a clock set back does not stall refreshing. A link that was removed,
// revoked or superseded ends the loop with an error.
func (c *Client) untilRefresh(failures int, random func(time.Duration) time.Duration) (time.Duration, error) {
	rec, err := c.linked()
	if err != nil {
		return 0, err
	}
	if !rec.RevokedAt.IsZero() {
		return 0, ErrRevoked
	}
	if rec.Superseded != nil {
		return 0, &SupersededError{Gen: rec.Superseded.Gen, At: rec.Superseded.At}
	}
	if failures > 0 {
		ceil := retryMin << min(failures-1, 16)
		return random(min(ceil, retryMax)) + time.Second, nil
	}
	last := rec.CheckedAt
	if last.IsZero() {
		last = rec.LinkedAt
	}
	if last.IsZero() {
		return 0, nil
	}
	wait := last.Add(refreshEvery + random(refreshWindow)).Sub(c.Now())
	return min(max(wait, 0), refreshEvery+refreshWindow), nil
}
