package discord

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func TestOnMessageFiltersAndStripsMention(t *testing.T) {
	c := New("t", "owner", false, []string{"chan1"}, nil)
	c.selfID = "self"
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }

	raw := `{"id":"1","channel_id":"dm1","content":"hi there","author":{"id":"u1","username":"owner"}}`
	var m message
	_ = json.Unmarshal([]byte(raw), &m)
	c.onMessage(context.Background(), m, h)
	if len(got) != 1 || !got[0].IsOwner || c.OwnerChatID() != "dm1" {
		t.Fatalf("owner DM not delivered: %+v", got)
	}

	raw = `{"id":"2","channel_id":"chanX","guild_id":"g","content":"<@self> status?","author":{"id":"u1","username":"owner"},"mentions":[{"id":"self"}]}`
	_ = json.Unmarshal([]byte(raw), &m)
	c.onMessage(context.Background(), m, h)
	if len(got) != 2 || got[1].Text != "status?" {
		t.Fatalf("mention not stripped: %+v", got)
	}

	raw = `{"id":"3","channel_id":"chanY","guild_id":"g","content":"chatter","author":{"id":"u2","username":"someone"}}`
	_ = json.Unmarshal([]byte(raw), &m)
	c.onMessage(context.Background(), m, h)
	if len(got) != 2 {
		t.Fatal("unaddressed guild message should be ignored")
	}
}
