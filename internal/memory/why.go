package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// "Why did you say that?" Each of the owner's replies keeps a note of the
// facts it drew on: those a recall in the turn brought back, and those
// picked for the prompt because they share words with the message (or,
// when every fact fitted, the closest few). The note is only fact ids and a
// hash of the reply's words, so it never holds what was said; forgetting a
// fact removes it from every note (scrubWhy).

// How a fact came into a reply.
const (
	WhyRecalled = "recalled" // a recall in the turn found it
	WhyMatched  = "matched"  // picked for the prompt as related to the message
)

const (
	whyClosest = 5    // closest facts noted when every fact was in the prompt
	whyMost    = 12   // most facts noted for one reply
	whyKeep    = 5000 // replies a note is kept for, newest first
)

func migrateWhy(ctx context.Context, db Execer) error {
	return execAll(ctx, db,
		`CREATE TABLE IF NOT EXISTS reply_why (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id INTEGER NOT NULL DEFAULT 0,
			chat_key TEXT NOT NULL,
			reply_hash TEXT NOT NULL,
			everything INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS reply_why_hash ON reply_why(reply_hash, id)`,
		`CREATE TABLE IF NOT EXISTS reply_facts (
			reply_id INTEGER NOT NULL,
			fact_id INTEGER NOT NULL,
			how TEXT NOT NULL,
			rank INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (reply_id, fact_id)
		)`,
		`CREATE INDEX IF NOT EXISTS reply_facts_fact ON reply_facts(fact_id)`,
	)
}

// WhyNote is what one reply drew on.
type WhyNote struct {
	MessageID  int64  // the reply's row in messages
	ChatKey    string // the conversation
	Reply      string // the reply's words, kept only as a hash
	Everything bool   // every fact was in the prompt
	Recalled   []int64
	Matched    []int64
}

// NoteWhy keeps a note of what a reply drew on. Only the owner's turns are
// noted (the agent never calls it for anyone else's).
func (s *Store) NoteWhy(ctx context.Context, n WhyNote) error {
	hash := replyHash(n.Reply)
	if hash == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO reply_why(message_id, chat_key, reply_hash, everything, created_at) VALUES(?,?,?,?,?)`,
		n.MessageID, n.ChatKey, hash, n.Everything, now())
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	rank := 0
	seen := map[int64]bool{}
	add := func(ids []int64, how string) error {
		for _, f := range ids {
			if seen[f] || rank >= whyMost {
				continue
			}
			seen[f] = true
			if _, err := tx.ExecContext(ctx, `INSERT INTO reply_facts(reply_id, fact_id, how, rank) VALUES(?,?,?,?)`, id, f, how, rank); err != nil {
				return err
			}
			rank++
		}
		return nil
	}
	if err := add(n.Recalled, WhyRecalled); err != nil {
		return err
	}
	if err := add(n.Matched, WhyMatched); err != nil {
		return err
	}
	if id > whyKeep {
		if _, err := tx.ExecContext(ctx, `DELETE FROM reply_facts WHERE reply_id <= ?`, id-whyKeep); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM reply_why WHERE id <= ?`, id-whyKeep); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WhyFact is a fact a reply drew on, and how.
type WhyFact struct {
	Fact
	How string // WhyRecalled or WhyMatched
}

// Why is what a reply drew on.
type Why struct {
	Everything bool      // every fact was in the prompt
	Facts      []WhyFact // still in memory, most telling first
}

// WhyFor finds the newest note for a reply with these words. ok is false
// when there is none: a reply from before notes were kept, or someone
// else's. Facts forgotten since are left out.
func (s *Store) WhyFor(ctx context.Context, reply string) (w Why, ok bool, err error) {
	hash := replyHash(reply)
	if hash == "" {
		return Why{}, false, nil
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id, everything FROM reply_why WHERE reply_hash=? ORDER BY id DESC LIMIT 1`, hash).Scan(&id, &w.Everything)
	if err == sql.ErrNoRows {
		return Why{}, false, nil
	}
	if err != nil {
		return Why{}, false, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, f.subject, f.content, f.source, f.created_at, rf.how
		FROM reply_facts rf JOIN facts f ON f.id = rf.fact_id WHERE rf.reply_id=? ORDER BY rf.rank`, id)
	if err != nil {
		return Why{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var f WhyFact
		var created string
		if err := rows.Scan(&f.ID, &f.Subject, &f.Content, &f.Source, &created, &f.How); err != nil {
			return Why{}, false, err
		}
		f.CreatedAt, _ = time.Parse(time.RFC3339, created)
		w.Facts = append(w.Facts, f)
	}
	return w, true, rows.Err()
}

// replyHash identifies a reply by its words, however its lines were broken.
func replyHash(reply string) string {
	norm := strings.Join(strings.Fields(reply), " ")
	if norm == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// scrubWhy drops a forgotten fact from every reply's note. A backup made
// before notes were kept has no table for them, and nothing to drop.
func scrubWhy(ctx context.Context, tx *sql.Tx, factID int64) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='reply_facts'`).Scan(&n); err != nil || n == 0 {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM reply_facts WHERE fact_id=?`, factID)
	return err
}

// LastTurn returns the messages of a conversation's latest turn after the
// message it answered (the twin's replies, tool calls and their results),
// oldest first, and the row id of its last reply (0 when there is none).
func (s *Store) LastTurn(ctx context.Context, chatKey string) ([]llm.Message, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, role, blocks FROM messages WHERE chat_key=? ORDER BY id DESC LIMIT 400`, chatKey)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []llm.Message
	var last int64
	for rows.Next() {
		var id int64
		var role, blocks string
		if err := rows.Scan(&id, &role, &blocks); err != nil {
			return nil, 0, err
		}
		m := llm.Message{Role: llm.Role(role)}
		if err := json.Unmarshal([]byte(blocks), &m.Blocks); err != nil {
			return nil, 0, err
		}
		if m.Role == llm.RoleUser && !toolResults(m) {
			break // the message the turn answered
		}
		if m.Role == llm.RoleAssistant && last == 0 {
			last = id
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, last, nil
}

// toolResults reports whether a user-role message only carries tool results.
func toolResults(m llm.Message) bool {
	for _, b := range m.Blocks {
		if b.Type != llm.BlockToolResult {
			return false
		}
	}
	return len(m.Blocks) > 0
}
