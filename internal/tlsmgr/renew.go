package tlsmgr

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Warning is optional health information from a Source. A renewal failure is
// recoverable while the previously loaded certificate is still valid.
func Warning(src Source) error {
	if s, ok := src.(interface{ Warning() error }); ok {
		return s.Warning()
	}
	return nil
}
func (f *files) Warning() error       { f.mu.RLock(); defer f.mu.RUnlock(); return f.warning }
func (f *files) setWarning(err error) { f.mu.Lock(); f.warning = err; f.mu.Unlock() }
func (f *files) time() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (f *files) sleep(ctx context.Context, d time.Duration) bool {
	if f.wait != nil {
		return f.wait(ctx, d)
	}
	return wait(ctx, d)
}

// maintain owns renewal scheduling. Both clocks and waits can be replaced in
// tests, so Run's retry/expiry behaviour is tested without real time passing.
func (f *files) maintain(ctx context.Context, interval time.Duration, renew func(context.Context) error) error {
	backoff := time.Second
	for ctx.Err() == nil {
		err := renew(ctx)
		if ctx.Err() != nil {
			return nil
		}
		delay := interval
		if err != nil {
			slog.Warn("HTTPS certificate renewal failed; keeping the last valid certificate", "err", err)
			f.setWarning(errors.New("could not renew HTTPS yet; keeping the current certificate and retrying"))
			delay = backoff
			backoff = min(backoff*2, time.Minute)
		} else {
			f.setWarning(nil)
			backoff = time.Second
		}
		f.mu.RLock()
		p := f.pair
		f.mu.RUnlock()
		if p == nil {
			return errors.New("no HTTPS certificate is available; check the certificate and key files")
		}
		remaining := p.Leaf.NotAfter.Sub(f.time())
		if remaining <= 0 {
			return errors.New("the HTTPS certificate has expired; renew it to restore remote access")
		}
		delay = min(delay, remaining)
		if !f.sleep(ctx, delay) {
			return nil
		}
	}
	return nil
}
