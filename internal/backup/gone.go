package backup

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrGone means the snapshot this machine last wrote isn't in the target
// any more: most often a network drive whose mount point is still there
// but empty because the drive isn't connected. Backing up then would fill
// the empty mount point on this very disk.
var ErrGone = errors.New("the backups are gone")

// checkGone refuses a run when the newest snapshot this machine wrote
// (State.LastName) isn't in the target and no other snapshot is either.
// Other snapshots there mean the drive is connected: another machine the
// twin moved to may have pruned this one's last. `mirrin backup target`,
// `mirrin backup init` and `mirrin backup resume` start over (they clear
// LastName). A target that keeps only some backups whatever it is sent
// (Keeper) is left to itself.
func (e *Engine) checkGone(ctx context.Context, st State) error {
	if st.LastName == "" {
		return nil
	}
	if _, ok := e.Target.(Keeper); ok {
		return nil
	}
	objs, err := e.Target.List(ctx)
	if err != nil {
		return nil // the run itself says what's wrong
	}
	if hasSnapshots(objs) {
		return nil
	}
	return goneError{fmt.Sprintf("the backups in %s are gone; is the drive connected? If you moved them on purpose, run `mirrin backup target` to say where backups go now", e.Target)}
}

type goneError struct{ msg string }

func (g goneError) Error() string        { return g.msg }
func (g goneError) Is(target error) bool { return target == ErrGone }

// hasSnapshots reports whether objs holds a snapshot (any machine's).
func hasSnapshots(objs []Object) bool {
	for _, o := range objs {
		if kind, _, ok := parseName(o.Name); ok && kind == "snap" {
			return true
		}
	}
	return false
}

// forgetLast clears the record of the newest snapshot, and the drill's
// verdict on it, when where backups go changes: the next run doesn't look
// for it there.
func forgetLast(dataDir string) error {
	st, err := LoadState(dataDir)
	if err != nil || (st.LastName == "" && st.DrillError == "") {
		return err
	}
	return UpdateState(dataDir, clearLast)
}

// clearLast forgets the newest snapshot and the drill's verdict on it.
func clearLast(s *State) {
	s.LastName, s.LastSum, s.LastSize = "", "", 0
	s.DrillAt, s.DrillError = time.Time{}, ""
}
