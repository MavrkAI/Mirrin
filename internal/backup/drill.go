package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// The monthly restore drill: once a month the schedule reads the newest
// snapshot back from where it is kept and checks it is still, byte for
// byte, what this machine wrote. A disk or bucket that has quietly damaged
// it turns the backup health check red, long before a restore needs it.
// This machine has no words, so it can't decrypt the snapshot; `mirrin
// backup verify` does that full check with them.

// DrillEvery is how often the drill runs; after a failed one it runs again
// a day later (DrillRetry), so fixing the disk turns the check green.
const (
	DrillEvery = 30 * 24 * time.Hour
	DrillRetry = 24 * time.Hour
)

// DrillDue reports whether the drill should run now.
func DrillDue(st State, now time.Time) bool {
	if st.Standby != nil || st.LastName == "" {
		return false
	}
	if st.DrillError != "" {
		return now.Sub(st.DrillAt) >= DrillRetry
	}
	since := st.DrillAt
	if since.IsZero() {
		since = st.Since
	}
	if since.IsZero() {
		since = st.LastGood
	}
	return now.Sub(since) >= DrillEvery
}

// Drill reads the newest snapshot this machine wrote (st.LastName) back
// from t and checks its size and SHA-256 against what was written.
func Drill(ctx context.Context, t Target, st State) error {
	if st.LastName == "" {
		return nil
	}
	rc, err := t.Get(ctx, st.LastName)
	if errors.Is(err, ErrNotFound) {
		// Pruned by another machine the twin moved to, which writes here
		// too: nothing of this machine's to check until its next run.
		if objs, lerr := t.List(ctx); lerr == nil && hasSnapshots(objs) {
			return errDrillSkipped
		}
		return fmt.Errorf("%s isn't in %s any more", st.LastName, t)
	}
	if err != nil {
		return fmt.Errorf("couldn't read %s from %s (%v)", st.LastName, t, err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return fmt.Errorf("couldn't read %s from %s (%v)", st.LastName, t, err)
	}
	if (st.LastSize > 0 && n != st.LastSize) || (st.LastSum != "" && hex.EncodeToString(h.Sum(nil)) != st.LastSum) {
		return fmt.Errorf("%s in %s no longer matches what was saved: the disk or service damaged it", st.LastName, t)
	}
	return nil
}

// errDrillSkipped means the snapshot to check was pruned, but others are
// there: the drill has nothing of this machine's to check.
var errDrillSkipped = errors.New("the newest snapshot this machine wrote was pruned")

// drillIfDue runs the drill when it is due, and records the result for the
// health check.
func (s *Scheduler) drillIfDue(ctx context.Context) {
	st, err := LoadState(s.DataDir)
	if err != nil || !DrillDue(st, s.now()) {
		return
	}
	s.runMu.Lock() // not while a snapshot is being written
	defer s.runMu.Unlock()
	// A run may have finished while this waited: check what it wrote.
	if st, err = LoadState(s.DataDir); err != nil || !DrillDue(st, s.now()) {
		return
	}
	e, err := s.Engine()
	if err != nil || e == nil {
		return
	}
	derr := Drill(ctx, e.Target, st)
	if ctx.Err() != nil {
		return
	}
	skipped := errors.Is(derr, errDrillSkipped)
	if skipped {
		derr = nil
	}
	at := s.now()
	_ = UpdateState(s.DataDir, func(st *State) {
		st.DrillAt, st.DrillError = at, ""
		if derr != nil {
			st.DrillError = plainError(derr)
		}
	})
	switch {
	case derr != nil:
		s.log().Warn("backup restore drill failed", "err", derr)
	case skipped:
		s.log().Info("backup restore drill skipped: " + errDrillSkipped.Error())
	default:
		s.log().Info("backup restore drill passed", "snapshot", st.LastName)
	}
}

// drillHealth is the health check's line for a failed drill.
func drillHealth(st State) (string, string) {
	return "the monthly check of the newest backup failed: " + st.DrillError,
		"check the disk or service the backups are on; the check runs again a day after it failed, or choose where backups go again with mirrin backup target. mirrin backup verify checks a backup with your words"
}
