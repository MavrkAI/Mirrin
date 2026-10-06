package memory

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// The regression: any '#' in a chat key counted as background work, so what
// the owner asked in an IRC room ("irc:#protocols|tony") never reached
// pattern mining.
func TestRecentUserRequestsKeepsIRCRooms(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for key, text := range map[string]string{
		"irc:#protocols|tony":            "what's the weather",
		"telegram:1":                     "check my inbox",
		"telegram:1#protocol-20260101-1": "a protocol's own prompt",
		"irc:#protocols|tony#task-0927":  "a task's own prompt",
		"irc:#task-force|tony":           "book the room",
	} {
		if err := s.AppendMessage(ctx, key, llm.Text(llm.RoleUser, text)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RecentUserRequests(ctx, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"book the room", "check my inbox", "what's the weather"}) {
		t.Fatalf("got %q", got)
	}
	if got, _ := s.RecentUserRequests(ctx, time.Hour, 1); len(got) != 1 {
		t.Fatalf("the limit counts live requests: %q", got)
	}
}
