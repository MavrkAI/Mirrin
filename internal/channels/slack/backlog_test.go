package slack

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
	c := New("xoxb", "xapp", "U0OWNER123", false, nil)
	c.selfID = "UBOT"
	c.names["U0OWNER123"] = "akshay"
	start := time.Now().Add(-5 * time.Minute).Truncate(time.Second) // connected five minutes ago
	clock := start
	c.backlog.Now = func() time.Time { return clock }
	c.backlog.Connected()
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	send := func(sent time.Time, text string) {
		var env envelope
		raw := fmt.Sprintf(`{"type":"events_api","payload":{"event":{"type":"message","channel_type":"im","channel":"D1","user":"U0OWNER123","text":%q,"ts":"%d.000100"}}}`, text, sent.Unix())
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatal(err)
		}
		c.onEvent(context.Background(), env, h)
	}

	send(start.Add(-5*time.Minute), "old")
	clock = start.Add(5 * time.Minute)
	send(start, "late")
	if len(got) != 1 || got[0].Text != "late" {
		t.Fatalf("got %+v", got)
	}
}
