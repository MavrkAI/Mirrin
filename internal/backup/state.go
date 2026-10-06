package backup

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// stateFile, in the data folder, is this machine's record of its backups.
// It is not part of a snapshot: it describes this machine.
const stateFile = "backup-state.json"

// State is what this machine knows about its backups.
type State struct {
	// Seq is the number of the last snapshot taken (or restored).
	Seq int64 `json:"seq"`
	// Since is when backups were set up here, for the health check before
	// the first snapshot.
	Since time.Time `json:"since,omitzero"`
	// LastAttempt, LastGood and LastError describe the latest runs.
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastGood    time.Time `json:"last_good,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	// LastName is the newest snapshot this machine wrote and read back
	// intact; pruning never removes it.
	LastName string `json:"last_name,omitempty"`
	LastSize int64  `json:"last_size,omitempty"`
	// LastSum is the SHA-256 of LastName as written, which the monthly
	// restore drill checks the target still holds (drill.go).
	LastSum string `json:"last_sum,omitempty"`
	// DrillAt is when the restore drill last ran; DrillError is why it
	// failed ("" when it passed).
	DrillAt    time.Time `json:"drill_at,omitzero"`
	DrillError string    `json:"drill_error,omitempty"`
	// KitShown is when the Recovery Kit was last shown; Nudged is when the
	// owner was last asked whether they still have the words.
	KitShown time.Time `json:"kit_shown,omitzero"`
	Nudged   time.Time `json:"nudged,omitzero"`
	// Standby is set when the twin moved to another machine.
	Standby *Handover `json:"standby,omitempty"`
	// Dismissed lists handover markers the owner answered with
	// `mirrin backup resume`.
	Dismissed []string `json:"dismissed,omitempty"`
	// RestoredAt is when a restore last made this machine the twin. A
	// handover marker from before it is out of date: the twin moved back.
	RestoredAt time.Time `json:"restored_at,omitzero"`
	// DevicesReviewed is when the owner last finished the device review
	// after a restore (the Review devices page); one before RestoredAt
	// means the review is still to do.
	DevicesReviewed time.Time `json:"devices_reviewed,omitzero"`
	// LeftOut names the files the last backup left out because their names
	// couldn't be restored on every system, for the owner to rename.
	LeftOut []string `json:"left_out,omitempty"`
	// From is the machine the last restore moved the twin from, for the
	// welcome page's "Welcome back" (nil when it came from this machine).
	From *MovedFrom `json:"from,omitempty"`
	// Wanted is when a change made while the twin wasn't running (a device
	// revoked with `mirrin devices revoke`) asked for a snapshot soon: the
	// twin takes one as soon as it is running again (Scheduler.Due).
	Wanted time.Time `json:"wanted,omitzero"`
}

// MovedFrom is what a restore knows about the machine the snapshot came
// from, which only that snapshot can say.
type MovedFrom struct {
	// Host names it, as the snapshot does ("" when it doesn't say).
	Host string `json:"host,omitempty"`
	// StandsBy is set when the restore left it the note to stand by.
	StandsBy bool `json:"stands_by,omitempty"`
	// SignIns is set when it had website sign-ins (the twin's browser
	// profile), which a snapshot never takes.
	SignIns bool `json:"sign_ins,omitempty"`
}

// WantSoon records that a change made outside the running twin should be
// in a snapshot soon, as Scheduler.Trigger does inside it.
func WantSoon(dataDir string, now time.Time) error {
	return UpdateState(dataDir, func(s *State) {
		if s.Wanted.Before(now) {
			s.Wanted = now
		}
	})
}

// LoadState reads the state in dataDir (empty when there is none).
func LoadState(dataDir string) (State, error) {
	var s State
	b, err := os.ReadFile(filepath.Join(dataDir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, nil // a damaged record starts over; the backups themselves are fine
	}
	return s, nil
}

// SaveState writes the state atomically.
func SaveState(dataDir string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dataDir, stateFile), b)
}

// UpdateState changes the state in dataDir under the backup lock's
// protection when the caller holds it, or on its own otherwise.
func UpdateState(dataDir string, fn func(*State)) error {
	s, err := LoadState(dataDir)
	if err != nil {
		return err
	}
	fn(&s)
	return SaveState(dataDir, s)
}

// writeFileAtomic writes data to p (0600) by renaming a finished temp file.
func writeFileAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmp.Name(), 0o600)
	return os.Rename(tmp.Name(), p)
}
