package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MavrkAI/Mirrin/internal/health"
)

// Scheduler runs a twin's backups: nightly at 03:30 (or as soon as the
// machine is awake after that), 10 minutes after a security change
// (Trigger), and checks for a handover marker every hour.
type Scheduler struct {
	// Engine builds the engine for a run from the current settings; it
	// returns nil, nil when backups aren't set up.
	Engine func() (*Engine, error)
	// DataDir holds the backup state.
	DataDir string
	// Loc is the owner's time zone, for "03:30". Zone, when set, is asked
	// instead at every look, so the nightly run follows a zone that moves
	// with the machine.
	Loc  *time.Location
	Zone func() *time.Location
	// OnStandby is called once when a handover marker is found.
	OnStandby func(Handover)
	// OnRun is called after every run.
	OnRun func(Manifest, error)
	Log   *slog.Logger

	// Delay is how long Trigger waits (10 minutes); Poll how often the
	// clock is looked at (5 minutes); HandoverEvery how often the target
	// is checked for a marker (an hour).
	Delay, Poll, HandoverEvery time.Duration
	Now                        func() time.Time

	mu      sync.Mutex
	pending *time.Timer
	reasons []string
	runMu   sync.Mutex
	active  atomic.Int32 // runs under way (Running)
	hMu     sync.Mutex   // one handover check at a time, so OnStandby runs once
	ctx     context.Context
}

// NightlyHour and NightlyMinute are when the nightly snapshot is due.
const (
	NightlyHour   = 3
	NightlyMinute = 30
)

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Start runs the schedule until ctx ends.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	poll, every := s.Poll, s.HandoverEvery
	if poll <= 0 {
		poll = 5 * time.Minute
	}
	if every <= 0 {
		every = time.Hour
	}
	go func() {
		// A moment after start: a machine that was off at 03:30 catches up.
		first := time.NewTimer(2 * time.Minute)
		defer first.Stop()
		t := time.NewTicker(poll)
		defer t.Stop()
		var lastHandover time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-first.C:
			case <-t.C:
			}
			s.tick(ctx, &lastHandover, every)
		}
	}()
}

// tick is one look at the clock: the hourly handover check, the monthly
// restore drill (drill.go) and the nightly snapshot, each when due.
func (s *Scheduler) tick(ctx context.Context, lastHandover *time.Time, every time.Duration) {
	if s.now().Sub(*lastHandover) >= every {
		*lastHandover = s.now()
		s.CheckHandover(ctx)
	}
	s.drillIfDue(ctx)
	if s.Due() {
		s.run(ctx, "nightly")
	}
}

// lastNightly is the most recent 03:30 at or before now.
func lastNightly(now time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	n := now.In(loc)
	at := time.Date(n.Year(), n.Month(), n.Day(), NightlyHour, NightlyMinute, 0, 0, loc)
	if at.After(n) {
		at = at.AddDate(0, 0, -1)
	}
	return at
}

// zone is the time zone "03:30" is read in now.
func (s *Scheduler) zone() *time.Location {
	if s.Zone != nil {
		if loc := s.Zone(); loc != nil {
			return loc
		}
	}
	return s.Loc
}

// Due reports whether the nightly snapshot should run now: none has been
// tried since the last 03:30, or the last try failed over an hour ago and
// nothing good has been saved since 03:30, or a change made while the twin
// wasn't running asked for one (WantSoon) and none was tried since.
func (s *Scheduler) Due() bool {
	st, err := LoadState(s.DataDir)
	if err != nil || st.Standby != nil {
		return false
	}
	if !st.Wanted.IsZero() && st.LastAttempt.Before(st.Wanted) {
		return true
	}
	now := s.now()
	due := lastNightly(now, s.zone())
	if st.LastAttempt.Before(due) {
		return true
	}
	return st.LastGood.Before(due) && now.Sub(st.LastAttempt) >= time.Hour
}

// Trigger asks for a snapshot soon, after a pairing, a revoke, a passkey
// enrolment or a reach change: 10 minutes after the first such change, so
// a burst of them makes one snapshot.
func (s *Scheduler) Trigger(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasons = append(s.reasons, reason)
	if s.pending != nil {
		return
	}
	delay := s.Delay
	if delay <= 0 {
		delay = 10 * time.Minute
	}
	s.pending = time.AfterFunc(delay, func() {
		s.mu.Lock()
		why := strings.Join(s.reasons, ", ")
		s.pending, s.reasons = nil, nil
		ctx := s.ctx
		s.mu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
		s.run(ctx, why)
	})
}

// ErrStopped means the scheduler isn't running (before Start, or after
// shutdown).
var ErrStopped = errors.New("backups aren't running")

// RunNow takes a snapshot now, in the background (the Backup page's "Back
// up now"). It refuses with ErrBusy while one is under way, and with
// ErrStopped before Start or after shutdown, so no run outlives the twin.
func (s *Scheduler) RunNow(reason string) error {
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return ErrStopped
	}
	// Counted before it starts, so the page sees it at once, and claimed in
	// one step, so two quick presses start one run.
	if !s.active.CompareAndSwap(0, 1) {
		return ErrBusy
	}
	go func() {
		defer s.active.Add(-1)
		s.run(ctx, reason)
	}()
	return nil
}

// Running reports whether a snapshot is being taken now.
func (s *Scheduler) Running() bool { return s.active.Load() > 0 }

// Pending reports whether a triggered snapshot is waiting.
func (s *Scheduler) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending != nil
}

// run takes one snapshot, if backups are set up and this machine isn't
// standing by. It checks for a handover marker first: a machine the twin
// moved away from must not add snapshots after the move.
func (s *Scheduler) run(ctx context.Context, why string) {
	s.active.Add(1)
	defer s.active.Add(-1)
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if ctx.Err() != nil || s.CheckHandover(ctx) != nil {
		return
	}
	e, err := s.Engine()
	if err != nil || e == nil {
		if err != nil {
			s.log().Warn("backup", "err", err)
			_ = UpdateState(s.DataDir, func(st *State) { st.LastAttempt, st.LastError = s.now(), plainError(err) })
		}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	m, err := e.Run(ctx)
	switch {
	case errors.Is(err, ErrStandingBy), errors.Is(err, ErrBusy):
		return
	case err != nil:
		s.log().Warn("backup failed", "why", why, "err", err)
	default:
		s.log().Info("backed up", "why", why, "seq", m.Seq, "to", e.Target.String())
	}
	if s.OnRun != nil {
		s.OnRun(m, err)
	}
}

// CheckHandover looks for a marker meant for this machine and, the first
// time one turns up, records it and calls OnStandby. It returns the
// handover this machine is standing by for, if any.
func (s *Scheduler) CheckHandover(ctx context.Context) *Handover {
	s.hMu.Lock()
	defer s.hMu.Unlock()
	st, err := LoadState(s.DataDir)
	if err != nil {
		return nil
	}
	if st.Standby != nil {
		return st.Standby
	}
	e, err := s.Engine()
	if err != nil || e == nil {
		return nil
	}
	h, err := CheckHandover(ctx, e.Target, existingStandby(s.DataDir), e.Settings.RecoveryPub, st.Dismissed, st.RestoredAt)
	if err != nil || h == nil {
		return nil
	}
	if err := StandBy(s.DataDir, *h); err != nil {
		s.log().Warn("backup handover", "err", err)
		return nil
	}
	s.log().Warn("the twin moved to another machine; standing by", "to", h.HostLabel)
	if s.OnStandby != nil {
		s.OnStandby(*h)
	}
	return h
}

// Health is the backup self-check: off until set up; a warning after 48
// hours without a good snapshot and a failure after 7 days; and, on a
// machine the twin moved away from, "Standing by: moved to <host>".
func Health(st State, s Settings, now time.Time) (health.State, string, string) {
	if st.Standby != nil {
		return health.Warn, StandbyMessage(st.Standby), "mirrin backup resume makes this machine the twin again"
	}
	if s.Recipient == "" {
		return health.Off, "not set up", "mirrin backup init keeps an encrypted copy in iCloud Drive or a folder you choose"
	}
	where := s.Where
	if st.DrillError != "" {
		what, fix := drillHealth(st)
		return health.Fail, what, fix
	}
	if st.LastGood.IsZero() {
		since := st.Since
		if since.IsZero() {
			return health.Warn, "no backup yet", "mirrin backup now"
		}
		return ageState(now.Sub(since), "no backup yet", st.LastError, where)
	}
	ago := now.Sub(st.LastGood)
	if ago < 48*time.Hour && len(st.LeftOut) > 0 {
		return health.Warn, fmt.Sprintf("last backup %s ago%s, but it left out %s: its name can't be restored everywhere", roughly(ago), where, leftOutNames(st.LeftOut)),
			"rename it without any of " + AwkwardChars + ", and the next backup takes it"
	}
	if ago < 48*time.Hour {
		return health.OK, fmt.Sprintf("last backup %s ago%s", roughly(ago), where), ""
	}
	return ageState(ago, "no good backup for "+roughly(ago), st.LastError, where)
}

func ageState(ago time.Duration, what, lastErr, where string) (health.State, string, string) {
	detail := what + where
	if lastErr != "" {
		detail += " (last try: " + lastErr + ")"
	}
	fix := "mirrin backup now, and check the backup folder is there"
	switch {
	case ago >= 7*24*time.Hour:
		return health.Fail, detail, fix
	case ago >= 48*time.Hour:
		return health.Warn, detail, fix
	}
	return health.OK, detail + "; the first one runs tonight", ""
}

// leftOutNames names the first few files a backup left out.
func leftOutNames(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:3], ", "), len(names)-3)
}

// Settings is what the health check needs to know about the set-up.
type Settings struct {
	Recipient string
	// Where is " (iCloud Drive › Mirrin Backups)" or "", for the detail line.
	Where string
}

// roughly says a duration the way a person would.
func roughly(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return "a minute"
	case d < 2*time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}
