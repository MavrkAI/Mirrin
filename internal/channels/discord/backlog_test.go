package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Only the replay at connect is dropped for its age: a live message that
// took five minutes to arrive still gets its answer.
func TestStalenessAppliesOnlyToTheBacklog(t *testing.T) {
	c := New("t", "owner", false, nil, nil)
	c.selfID = "self"
	start := time.Now().Add(-5 * time.Minute).Truncate(time.Second) // connected five minutes ago
	clock := start
	c.backlog.Now = func() time.Time { return clock }
	c.backlog.Connected()
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	send := func(id int, sent time.Time, text string) {
		var m message
		raw := fmt.Sprintf(`{"id":"%d","channel_id":"dm1","content":%q,"timestamp":%q,"author":{"id":"u1","username":"owner"}}`, id, text, sent.Format(time.RFC3339))
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		c.onMessage(context.Background(), m, h)
	}

	send(1, start.Add(-5*time.Minute), "old")
	clock = start.Add(5 * time.Minute)
	send(2, start, "late")
	if len(got) != 1 || got[0].Text != "late" {
		t.Fatalf("got %+v", got)
	}
}
