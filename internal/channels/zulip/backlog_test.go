package zulip

import (
	"context"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Only the replay at connect is dropped for its age: a live message that
// took five minutes to arrive still gets its answer.
func TestStalenessAppliesOnlyToTheBacklog(t *testing.T) {
	c := New("https://zulip.example", "bot@zulip.example", "key", "me@example.com", false, nil)
	start := time.Now().Add(-5 * time.Minute).Truncate(time.Second) // connected five minutes ago
	clock := start
	c.backlog.Now = func() time.Time { return clock }
	c.backlog.Connected()
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }

	c.onMessage(context.Background(), zmessage{SenderEmail: "me@example.com", SenderID: 8, Type: "private", Content: "old", Timestamp: start.Add(-5 * time.Minute).Unix()}, false, h)
	clock = start.Add(5 * time.Minute)
	c.onMessage(context.Background(), zmessage{SenderEmail: "me@example.com", SenderID: 8, Type: "private", Content: "late", Timestamp: start.Unix()}, false, h)
	if len(got) != 1 || got[0].Text != "late" {
		t.Fatalf("got %+v", got)
	}
}
