package memory

import (
	"context"
	"encoding/json"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Said is one message's words in some conversation, and when it was kept.
type Said struct {
	ChatKey string
	Role    llm.Role
	Text    string
	At      time.Time
}

// recentScanLimit bounds how many messages RecentElsewhere looks through,
// however busy the last hours were.
const recentScanLimit = 400

// RecentElsewhere returns what was said in conversations other than
// chatKey since the given time, oldest first. Only the words are kept:
// tool calls, tool results and empty messages are left out.
func (s *Store) RecentElsewhere(ctx context.Context, chatKey string, since time.Time) ([]Said, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_key, role, blocks, created_at FROM messages WHERE chat_key<>? ORDER BY id DESC LIMIT ?`,
		chatKey, recentScanLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Said
	for rows.Next() {
		var key, role, blocks, ts string
		if err := rows.Scan(&key, &role, &blocks, &ts); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		if at.Before(since) {
			break // ids follow time: everything further back is older still
		}
		m := llm.Message{Role: llm.Role(role)}
		if json.Unmarshal([]byte(blocks), &m.Blocks) != nil {
			continue
		}
		if t := m.PlainText(); t != "" {
			out = append(out, Said{ChatKey: key, Role: m.Role, Text: t, At: at})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
