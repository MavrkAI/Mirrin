package memory

import (
	"context"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

func TestFirstConversationAt(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, ok := s.FirstConversationAt(ctx); ok {
		t.Fatal("a new store has no conversations")
	}
	// The twin's own scratch work (a first look, a protocol run) doesn't count.
	_ = s.AppendMessage(ctx, "cli:terminal#firstlook", llm.Text(llm.RoleUser, "introduce yourself"))
	s.Audit(ctx, "message.in", "whatsapp:1#protocol", "run")
	if _, ok := s.FirstConversationAt(ctx); ok {
		t.Fatal("scratch conversations counted as someone talking to the twin")
	}
	_ = s.AppendMessage(ctx, "whatsapp:1", llm.Text(llm.RoleUser, "hi"))
	old := time.Now().AddDate(0, -2, 0).UTC().Truncate(time.Second)
	if _, err := s.db.Exec(`INSERT INTO audit(ts, kind, chat_key, detail) VALUES(?, 'message.in', 'telegram:1', 'hello')`, old.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	// A cleared conversation still leaves the audit log behind.
	if got, ok := s.FirstConversationAt(ctx); !ok || !got.Equal(old) {
		t.Fatalf("first = %v %v, want %v", got, ok, old)
	}
}

// A twin used only in IRC or Matrix rooms isn't new: a '#' that starts the
// room's name is not a scratch run's suffix.
func TestFirstConversationInARoom(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	_ = s.AppendMessage(ctx, "irc:#golang|tony#protocol-20260927-070000-1", llm.Text(llm.RoleUser, "run"))
	if _, ok := s.FirstConversationAt(ctx); ok {
		t.Fatal("a protocol run in a room's conversation counted")
	}
	_ = s.AppendMessage(ctx, "irc:#golang|tony", llm.Text(llm.RoleUser, "hi"))
	if _, ok := s.FirstConversationAt(ctx); !ok {
		t.Fatal("an IRC room's conversation didn't count")
	}
	s2, _ := Open(t.TempDir())
	defer s2.Close()
	s2.Audit(ctx, "message.in", "matrix:#house:example.org", "hello")
	if _, ok := s2.FirstConversationAt(ctx); !ok {
		t.Fatal("a Matrix room's conversation didn't count")
	}
}
