package slack

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func TestOnEvent(t *testing.T) {
	c := New("xoxb", "xapp", "U0OWNER123", false, nil)
	c.selfID = "UBOT"
	c.names["U0OWNER123"] = "akshay"
	c.names["USTRANGER"] = "bob"
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }

	var env envelope
	_ = json.Unmarshal([]byte(`{"envelope_id":"e1","type":"events_api","payload":{"event":{"type":"message","channel_type":"im","channel":"D1","user":"U0OWNER123","text":"what &amp; when?","ts":"9999999999.0001"}}}`), &env)
	c.onEvent(context.Background(), env, h)
	if len(got) != 1 || got[0].Text != "what & when?" || !got[0].IsOwner || c.OwnerChatID() != "D1" {
		t.Fatalf("owner DM: %+v", got)
	}
	_ = json.Unmarshal([]byte(`{"envelope_id":"e2","type":"events_api","payload":{"event":{"type":"message","channel_type":"channel","channel":"C1","user":"U0OWNER123","text":"<@UBOT> status","ts":"9999999999.0002"}}}`), &env)
	c.onEvent(context.Background(), env, h)
	_ = json.Unmarshal([]byte(`{"envelope_id":"e3","type":"events_api","payload":{"event":{"type":"app_mention","channel":"C1","user":"U0OWNER123","text":"<@UBOT> status","ts":"9999999999.0002"}}}`), &env)
	c.onEvent(context.Background(), env, h)
	if len(got) != 2 || got[1].Text != "status" {
		t.Fatalf("mention should be delivered once: %+v", got)
	}
	_ = json.Unmarshal([]byte(`{"envelope_id":"e4","type":"events_api","payload":{"event":{"type":"message","channel_type":"im","channel":"D2","user":"USTRANGER","text":"hey","ts":"9999999999.0003"}}}`), &env)
	c.onEvent(context.Background(), env, h)
	if len(got) != 2 {
		t.Fatal("stranger should be ignored")
	}
}
