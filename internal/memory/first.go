package memory

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// FirstConversationAt is when someone first talked to the twin (scratch
// conversations such as protocol runs aside), and whether anyone has. The
// audit log counts too, so a cleared conversation still shows the twin isn't
// new.
func (s *Store) FirstConversationAt(ctx context.Context) (time.Time, bool) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat_key, MIN(created_at) FROM messages WHERE role='user' AND instr(chat_key, '#')=0
		UNION ALL SELECT chat_key, MIN(created_at) FROM messages WHERE role='user' AND instr(chat_key, '#')>0 GROUP BY chat_key
		UNION ALL SELECT chat_key, MIN(ts) FROM audit WHERE kind='message.in' AND instr(chat_key, '#')=0
		UNION ALL SELECT chat_key, MIN(ts) FROM audit WHERE kind='message.in' AND instr(chat_key, '#')>0 GROUP BY chat_key`)
	if err != nil {
		return time.Time{}, false
	}
	defer rows.Close()
	var first time.Time
	for rows.Next() {
		var key, at sql.NullString
		if rows.Scan(&key, &at) != nil || !at.Valid || !liveChat(key.String) {
			continue
		}
		if t, err := time.Parse(time.RFC3339, at.String); err == nil && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	return first, !first.IsZero()
}

// liveChat reports whether a chat key is someone talking to the twin rather
// than its own scratch work. A '#' marks scratch work ("whatsapp:1#protocol-…")
// except where it starts the chat's own name: an IRC or Matrix room
// ("irc:#golang|tony"), unless a run's suffix follows that.
func liveChat(key string) bool {
	i := strings.IndexByte(key, '#')
	if i < 0 {
		return true
	}
	if IsScratch(key) {
		return false
	}
	return i > 0 && key[i-1] == ':' && !strings.Contains(key[i+1:], "#")
}
