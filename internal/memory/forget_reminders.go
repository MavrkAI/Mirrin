package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// FactRemindersKey is the kv record of which reminders were set from which
// fact (daemon/datereminder.go): a JSON list of {"fact": id, "reminders":
// [ids], …}.
const FactRemindersKey = "factreminders.v1"

// dropFactReminders removes, inside a forget's transaction, the reminders set
// from a fact that is no longer in this database, wording and all, and their
// links. It runs on the live database before the WAL is checkpointed, and on
// every backup and recorded copy, so restoring one doesn't bring a forgotten
// fact's reminder back.
func dropFactReminders(ctx context.Context, tx *sql.Tx) error {
	var tables int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('kv', 'reminders')`).Scan(&tables); err != nil || tables < 2 {
		return err // an older copy without them has nothing to drop
	}
	var v string
	err := tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, FactRemindersKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && v == "") {
		return nil
	}
	if err != nil {
		return err
	}
	var raws []json.RawMessage
	if json.Unmarshal([]byte(v), &raws) != nil {
		return nil // not ours to judge; the daemon reads it the same lenient way
	}
	kept := raws[:0:0]
	for _, raw := range raws {
		var l struct {
			Fact      int64   `json:"fact"`
			Reminders []int64 `json:"reminders"`
		}
		if json.Unmarshal(raw, &l) != nil {
			kept = append(kept, raw)
			continue
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM facts WHERE id=?`, l.Fact).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			kept = append(kept, raw)
			continue
		}
		for _, rid := range l.Reminders {
			if _, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE id=?`, rid); err != nil {
				return err
			}
		}
	}
	if len(kept) == len(raws) {
		return nil
	}
	if len(kept) == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE key=?`, FactRemindersKey)
		return err
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE kv SET value=? WHERE key=?`, string(b), FactRemindersKey)
	return err
}
