package reach

import (
	"context"
	"log/slog"
	"time"
)

// StayAwake holds a sleep inhibitor only while on external power. It never
// prevents display sleep. Cancellation releases the inhibitor.
func StayAwake(ctx context.Context) error {
	tick := time.NewTicker(awakeEvery)
	defer tick.Stop()
	return stayAwake(ctx, onPower, inhibit, tick.C)
}

// awakeEvery is how often the power source is checked.
var awakeEvery = 15 * time.Second

// KeepAwake holds the sleep inhibitor in the background: only while on
// external power when onACOnly, else always. stop releases it and waits
// until the inhibitor has exited; ending ctx does the same.
func KeepAwake(ctx context.Context, onACOnly bool) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	power := onPower
	if !onACOnly {
		power = func() bool { return true }
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(awakeEvery)
		defer tick.Stop()
		if err := stayAwake(ctx, power, inhibit, tick.C); err != nil {
			slog.Warn("keep awake stopped", "err", err)
		}
	}()
	return func() { cancel(); <-done }
}

func stayAwake(ctx context.Context, power func() bool, hold func(context.Context) error, ticks <-chan time.Time) error {
	var stop context.CancelFunc
	var done chan error
	defer func() {
		if stop != nil {
			stop()
			<-done
		}
	}()
	for {
		if power() {
			if stop == nil {
				stop, done = startInhibitor(ctx, hold)
			}
		} else if stop != nil {
			stop()
			<-done
			stop = nil
			done = nil
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			stop()
			stop = nil
			done = nil
			return err
		case <-ticks:
		}
	}
}

func startInhibitor(ctx context.Context, hold func(context.Context) error) (context.CancelFunc, chan error) {
	child, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- hold(child) }()
	return cancel, done
}
