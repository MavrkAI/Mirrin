package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Retention says how long the activity log and bulky tool output are kept.
// Facts, reminders, approvals and what was said in live chats are not
// touched: those are the owner's to keep or forget.
type Retention struct {
	// AuditDays deletes activity-log entries older than this. 0 keeps them.
	AuditDays int
	// TrimAfterDays shortens, after this many days, activity-log entries
	// (which quote up to a few hundred characters of messages and tool
	// inputs) and long tool results kept in conversations. 0 never trims.
	TrimAfterDays int
}

// How much of a trimmed entry is kept.
const (
	trimAuditTo  = 120  // characters of an activity-log entry
	trimResultAt = 2000 // tool results longer than this are cut…
	trimResultTo = 400  // …to this many characters
)

// TidyResult says what Tidy did.
type TidyResult struct {
	AuditDeleted   int64
	AuditTrimmed   int64
	ResultsTrimmed int64
}

// Tidy applies the retention policy. It is safe to run at any time and as
// often as wanted; a forget or backup in progress waits for it.
func (s *Store) Tidy(ctx context.Context, r Retention) (TidyResult, error) {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	var res TidyResult
	if r.AuditDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -r.AuditDays).UTC().Format(time.RFC3339)
		out, err := s.db.ExecContext(ctx, `DELETE FROM audit WHERE ts < ?`, cutoff)
		if err != nil {
			return res, err
		}
		res.AuditDeleted, _ = out.RowsAffected()
	}
	if r.TrimAfterDays <= 0 {
		return res, nil
	}
	cutoff := time.Now().AddDate(0, 0, -r.TrimAfterDays).UTC().Format(time.RFC3339)
	out, err := s.db.ExecContext(ctx, `UPDATE audit SET detail = substr(detail, 1, ?) || '…' WHERE ts < ? AND length(detail) > ?`,
		trimAuditTo, cutoff, trimAuditTo+1)
	if err != nil {
		return res, err
	}
	res.AuditTrimmed, _ = out.RowsAffected()
	n, err := s.trimToolResults(ctx, cutoff, r.TrimAfterDays)
	res.ResultsTrimmed = n
	return res, err
}

// trimResultsKey remembers the last message id trimToolResults looked at, so
// each run only reads messages that have come of age since the last.
const trimResultsKey = "retention.results_through"

// trimToolResults shortens long tool results in messages older than cutoff.
// A tool result is what a tool returned to the model (a web page, a file, a
// search); the model's reply that used it is left as it is. Only rows long
// enough to hold one are read (octet_length needs no look at the content),
// and each batch's changes are written together, so a first run on a big
// memory doesn't hold up a backup or a forget for long.
func (s *Store) trimToolResults(ctx context.Context, cutoff string, days int) (int64, error) {
	var after int64
	if v, _ := s.Get(ctx, trimResultsKey); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	// Everything up to here has come of age since the last run.
	var through sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM messages WHERE id > ? AND created_at < ?`, after, cutoff).Scan(&through); err != nil {
		return 0, err
	}
	if !through.Valid {
		return 0, nil
	}
	var trimmed int64
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT id, blocks FROM messages
			WHERE id > ? AND id <= ? AND created_at < ? AND octet_length(blocks) > ? ORDER BY id LIMIT 100`,
			after, through.Int64, cutoff, trimResultAt)
		if err != nil {
			return trimmed, err
		}
		type fix struct {
			id     int64
			blocks string
		}
		var fixes []fix
		seen := 0
		for rows.Next() {
			var id int64
			var raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return trimmed, err
			}
			after, seen = id, seen+1
			if b, ok := trimBlocks(raw, days); ok {
				fixes = append(fixes, fix{id, b})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return trimmed, err
		}
		if seen == 0 {
			break
		}
		if len(fixes) > 0 {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return trimmed, err
			}
			for _, f := range fixes {
				if _, err := tx.ExecContext(ctx, `UPDATE messages SET blocks=? WHERE id=?`, f.blocks, f.id); err != nil {
					tx.Rollback()
					return trimmed, err
				}
			}
			if err := tx.Commit(); err != nil {
				return trimmed, err
			}
			trimmed += int64(len(fixes))
		}
	}
	return trimmed, s.Set(ctx, trimResultsKey, strconv.FormatInt(through.Int64, 10))
}

// trimBlocks cuts the long tool results in a stored message. ok is false
// when nothing needed cutting (or the row isn't blocks JSON).
func trimBlocks(raw string, days int) (string, bool) {
	var bs []llm.Block
	if json.Unmarshal([]byte(raw), &bs) != nil {
		return "", false
	}
	changed := false
	for i, b := range bs {
		if b.Type != llm.BlockToolResult || len(b.Text) <= trimResultAt {
			continue
		}
		cut := trimResultTo
		for cut > 0 && !utf8Start(b.Text[cut]) {
			cut--
		}
		bs[i].Text = b.Text[:cut] + trimmedNote(days)
		changed = true
	}
	if !changed {
		return "", false
	}
	out, err := json.Marshal(bs)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// utf8Start reports whether b begins a UTF-8 character, so a cut never
// splits one.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// trimmedNote ends a tool result that was cut short.
func trimmedNote(days int) string {
	return "\n[trimmed: tool output is kept in full for " + strconv.Itoa(days) + " days]"
}
