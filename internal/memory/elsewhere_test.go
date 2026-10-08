package memory

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

func TestRecentElsewhereKeepsOtherChatsWordsInTheWindowOldestFirst(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	add := func(key string, role llm.Role, text string, ago time.Duration) {
		t.Helper()
		if err := s.AppendMessage(ctx, key, llm.Text(role, text)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE messages SET created_at=? WHERE id=(SELECT MAX(id) FROM messages)`,
			time.Now().Add(-ago).UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	add("voice:local", llm.RoleUser, "too old", 3*time.Hour)
	add("voice:local", llm.RoleUser, "two flights, which?", 30*time.Minute)
	add("voice:local", llm.RoleAssistant, "the 9am or the 2pm", 29*time.Minute)
	// A tool call carries no words and is left out.
	b, _ := json.Marshal([]llm.Block{{Type: llm.BlockToolUse, ToolUseID: "t1", ToolName: "x"}})
	if _, err := s.db.ExecContext(ctx, `INSERT INTO messages(chat_key, role, blocks, created_at) VALUES(?,?,?,?)`,
		"voice:local", "assistant", string(b), time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	add("telegram:owner", llm.RoleUser, "this chat", time.Minute)

	got, err := s.RecentElsewhere(ctx, "telegram:owner", time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "two flights, which?" || got[1].Text != "the 9am or the 2pm" || got[0].ChatKey != "voice:local" {
		t.Fatalf("want the voice exchange in the window, oldest first, got %+v", got)
	}
}
