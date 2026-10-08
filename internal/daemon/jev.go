package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/jev"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// Optional quick judgments with TypeSafe's Jev (internal/jev). A call site
// asks d.judge only after its own exact rule didn't settle something, and
// does exactly what it did before whenever judge says no: Jev off, no key,
// an error, a timeout or a malformed answer. Nothing here sends memories;
// each call site sends only the few words its question needs.

// jevQuietFor is how long a failure of one kind stays quiet in the log
// after it was logged once.
const jevQuietFor = 10 * time.Minute

// jevState is Jev's part of the daemon.
type jevState struct {
	mu      sync.Mutex
	url     string                // tests point it at a jevtest server; "" is TypeSafe
	lastLog map[string]time.Time  // failure kind → when it was last logged
	lastErr string                // "key" (refused) or "unreachable", for the Accounts card; "" when the last call worked
	notices map[string]lastNotice // home chat → the weekly note or brief last delivered there (notice_replies.go)
}

// jevClient is a client for key with the current settings. It is made for
// each call, so a key saved while the twin runs is used at once.
func (d *Daemon) jevClient(c *config.Config, key string, timeout time.Duration) *jev.Client {
	d.jev.mu.Lock()
	url := d.jev.url
	d.jev.mu.Unlock()
	opts := []jev.Option{jev.WithModel(c.JevModel()), jev.WithUserAgent("mirrin/" + d.version)}
	if url != "" {
		opts = append(opts, jev.WithURL(url))
	}
	if timeout > 0 {
		opts = append(opts, jev.WithTimeout(timeout))
	}
	return jev.New(key, opts...)
}

// judge asks Jev qs about state for use (a short name such as
// "reminder-replies", which also names its line in the usage ledger). It
// reports false, having sent nothing, unless Jev is switched on with a key;
// and false on any failure, which it logs quietly and without content.
func (d *Daemon) judge(ctx context.Context, use string, state any, qs map[string]jev.Question) (*jev.Result, bool) {
	c := d.Config()
	if !c.JevOn() {
		return nil, false
	}
	res, err := d.jevClient(&c, c.JevKey(), 0).Ask(ctx, state, qs)
	if err != nil {
		d.jevFailed(use, err)
		return nil, false
	}
	d.jevWorked()
	d.jevUsage(ctx, use, c.JevModel(), res)
	return res, true
}

// jevUsage adds a call's tokens to the usage ledger as "jev.<use>", under
// the model name asked for (not the exact version that answered), so a
// price set for it under usage.prices keeps applying.
func (d *Daemon) jevUsage(ctx context.Context, use, model string, res *jev.Result) {
	t := llm.Tokens{Input: int64(res.Usage.InputTokens), Output: int64(res.Usage.OutputTokens)}
	day := clock().In(d.location()).Format(memory.DayFormat)
	if err := d.store.RecordUsage(context.WithoutCancel(ctx), day, model, "jev."+use, t); err != nil {
		d.log.Warn("jev usage not recorded", "err", err)
	}
}

// jevKind sorts a failure for the log, and for the card: "key" when the
// key was refused, "" when it says nothing about TypeSafe (the caller gave
// up, or asked something it shouldn't), else how TypeSafe failed.
func jevKind(err error) (kind, card string) {
	var se *jev.StatusError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, jev.ErrInvalid), errors.Is(err, jev.ErrNoKey):
		return "local", ""
	case errors.As(err, &se) && (se.Code == 401 || se.Code == 403):
		return "key", "key"
	case errors.As(err, &se) && se.Retryable:
		return "busy", "unreachable"
	case errors.As(err, &se):
		return "refused", "unreachable"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "unreachable"
	case errors.Is(err, jev.ErrBadAnswer):
		return "answer", "unreachable"
	}
	return "unreachable", "unreachable"
}

// jevFailed notes a failure: one Info line per kind per 10 minutes, the
// rest at Debug. The line names the use and the kind, never what was asked.
func (d *Daemon) jevFailed(use string, err error) {
	kind, card := jevKind(err)
	status := 0
	var se *jev.StatusError
	if errors.As(err, &se) {
		status = se.Code
	}
	now := clock()
	d.jev.mu.Lock()
	if card != "" {
		d.jev.lastErr = card
	}
	loud := now.Sub(d.jev.lastLog[kind]) >= jevQuietFor
	if loud {
		if d.jev.lastLog == nil {
			d.jev.lastLog = map[string]time.Time{}
		}
		d.jev.lastLog[kind] = now
	}
	d.jev.mu.Unlock()
	if kind == "local" {
		if !errors.Is(err, context.Canceled) {
			d.log.Warn("jev question not sent", "use", use, "why", err)
		}
		return
	}
	if loud {
		d.log.Info("jev unavailable, using the usual rules", "use", use, "status", kind, "code", status)
		return
	}
	d.log.Debug("jev unavailable, using the usual rules", "use", use, "status", kind, "code", status)
}

// jevWorked clears the card's problem after a call that worked.
func (d *Daemon) jevWorked() {
	d.jev.mu.Lock()
	d.jev.lastErr = ""
	d.jev.mu.Unlock()
}

// jevProblem is the card's problem: "key", "unreachable" or "".
func (d *Daemon) jevProblem() string {
	d.jev.mu.Lock()
	defer d.jev.mu.Unlock()
	return d.jev.lastErr
}
