package mattermost

import (
	"context"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Only the replay at connect is dropped for its age: a live message that
// took five minutes to arrive still gets its answer.
func TestStalenessAppliesOnlyToTheBacklog(t *testing.T) {
	c := New("https://mm.example", "tok", "akshay", false, nil)
	c.selfID, c.selfName = "bot1", "mavrk"
	start := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond) // connected five minutes ago
	clock := start
	c.backlog.Now = func() time.Time { return clock }
	c.backlog.Connected()
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }

	c.onPost(context.Background(), post{ID: "1", UserID: "u1", ChannelID: "d1", Message: "old", CreateAt: start.Add(-5 * time.Minute).UnixMilli()}, "D", "akshay", h)
	clock = start.Add(5 * time.Minute)
	c.onPost(context.Background(), post{ID: "2", UserID: "u1", ChannelID: "d1", Message: "late", CreateAt: start.UnixMilli()}, "D", "akshay", h)
	if len(got) != 1 || got[0].Text != "late" {
		t.Fatalf("got %+v", got)
	}
}
