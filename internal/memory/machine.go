package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

// Some of what memory.db keeps belongs to the machine the twin runs on, not
// to the twin: when this machine's scheduler last looked at the clock, and
// whether this machine is paused, or paused because the twin moved away.
// A restore (`mirrin restore`, `mirrin memory restore`) or an identity
// import brings back the twin's memory, but not these: the machine keeps
// its own pause, and runs that fell due before the restore aren't made up
// again (the machine the copy came from, or this one before it, ran them).
const (
	// KeyAlive is when the scheduler last looked (internal/heartbeat).
	KeyAlive = "heartbeat.alive.v1"
	// KeyPaused is a pause kept over restarts (daemon/pause.go).
	KeyPaused = "paused.v1"
	// KeyStandbyPause marks a pause a backup standby put on (daemon/backup.go).
	KeyStandbyPause = "paused.standby"
)

// Pause is what KeyPaused keeps: when the pause began and, for a pause for
// a while ("for an hour", "until tomorrow"), when it ends by itself. A zero
// Until is a pause until the owner resumes.
type Pause struct {
	Since time.Time
	Until time.Time
}

// String is the pause as KeyPaused keeps it. A pause without an end is kept
// as its start alone, as versions before timed pauses wrote it.
func (p Pause) String() string {
	if p.Until.IsZero() {
		return p.Since.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(pauseRecord{p.Since.UTC().Format(time.RFC3339), p.Until.UTC().Format(time.RFC3339)})
	return string(b)
}

type pauseRecord struct {
	Since string `json:"since"`
	Until string `json:"until,omitempty"`
}

// Expired reports whether a timed pause has run out at now.
func (p Pause) Expired(now time.Time) bool { return !p.Until.IsZero() && !now.Before(p.Until) }

// ParsePause reads a KeyPaused value: {since, until}, or the plain RFC 3339
// start older versions wrote. ok is false for an empty value. A value it
// can't read is still a pause, since an unknown time (a zero Since), and
// never lost.
func ParsePause(v string) (p Pause, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Pause{}, false
	}
	if strings.HasPrefix(v, "{") {
		var r pauseRecord
		if json.Unmarshal([]byte(v), &r) == nil {
			p.Since, _ = time.Parse(time.RFC3339, r.Since)
			p.Until, _ = time.Parse(time.RFC3339, r.Until)
		}
		return p, true
	}
	p.Since, _ = time.Parse(time.RFC3339, v)
	return p, true
}

// machineKeys are the keys a restore keeps from the machine.
var machineKeys = []string{KeyAlive, KeyPaused, KeyStandbyPause}

// MachineState reads the machine-kept keys from the database at path. It is
// a best effort: a missing or damaged file has none.
func MachineState(path string) map[string]string {
	out := map[string]string{}
	if _, err := os.Stat(path); err != nil {
		return out
	}
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return out
	}
	defer db.Close()
	for _, k := range machineKeys {
		var v string
		if db.QueryRow(`SELECT value FROM kv WHERE key=?`, k).Scan(&v) == nil && v != "" {
			out[k] = v
		}
	}
	return out
}

// SettleRestored makes the database at path, just restored from a copy,
// this machine's again: its machine-kept keys are carried (what this machine
// had, from MachineState) and nothing else, and the scheduler's last look is
// now. It reports what the copy itself had, so the caller can say so (a twin
// that was paused when the copy was taken).
func SettleRestored(path string, carried map[string]string, now time.Time) (had map[string]string, err error) {
	had = MachineState(path)
	db, err := sql.Open("sqlite", fileURI(path)+"?_pragma=busy_timeout(5000)&_pragma=secure_delete(1)")
	if err != nil {
		return had, err
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := settleKV(ctx, db, "", carried, now); err != nil {
		db.Close()
		return had, err
	}
	// Everything in the file itself: it is about to be moved into place,
	// and a journal left beside it wouldn't follow.
	_, _ = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if err := db.Close(); err != nil {
		return had, err
	}
	if st, err := os.Stat(path + "-wal"); err == nil && st.Size() > 0 {
		return had, errors.New("the restored copy's journal couldn't be written into it")
	}
	for _, j := range journal {
		_ = os.Remove(path + j)
	}
	return had, nil
}

// settleKV sets the machine-kept keys in schema's kv (main when "") to
// carried, with the scheduler's last look at now.
func settleKV(ctx context.Context, x interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, schema string, carried map[string]string, now time.Time) error {
	table := "kv"
	if schema != "" {
		table = schema + ".kv"
	}
	for _, k := range machineKeys {
		v := carried[k]
		if k == KeyAlive {
			v = now.Round(0).UTC().Format(time.RFC3339Nano)
		}
		if _, err := x.ExecContext(ctx, `DELETE FROM `+table+` WHERE key=?`, k); err != nil {
			return err
		}
		if v == "" {
			continue
		}
		if _, err := x.ExecContext(ctx, `INSERT INTO `+table+`(key, value) VALUES(?,?)`, k, v); err != nil {
			return err
		}
	}
	return nil
}

// SettleKV is SettleRestored inside a transaction the caller holds (an
// identity import restoring memory table by table).
func SettleKV(ctx context.Context, tx *sql.Tx, carried map[string]string, now time.Time) error {
	return settleKV(ctx, tx, "", carried, now)
}
