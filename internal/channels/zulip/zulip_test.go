package zulip

import (
	"context"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func TestOnMessage(t *testing.T) {
	c := New("https://x.zulipchat.com", "bot@x", "k", "me@x", false, nil)
	c.selfID = 7
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	now := time.Now().Unix()
	c.onMessage(context.Background(), zmessage{ID: 1, SenderEmail: "me@x", SenderID: 2, Type: "private", Content: "hi", Timestamp: now}, false, h)
	c.onMessage(context.Background(), zmessage{ID: 2, SenderEmail: "me@x", SenderID: 2, Type: "stream", StreamID: 9, Subject: "ops", Content: "@**Mirrin** status", Timestamp: now}, true, h)
	c.onMessage(context.Background(), zmessage{ID: 3, SenderEmail: "me@x", SenderID: 2, Type: "stream", StreamID: 9, Subject: "ops", Content: "not for you", Timestamp: now}, false, h)
	c.onMessage(context.Background(), zmessage{ID: 4, SenderEmail: "bob@x", SenderID: 3, Type: "private", Content: "hey", Timestamp: now}, false, h)
	if len(got) != 2 || got[0].ChatID != "pm:me@x" || got[1].ChatID != "stream:9:ops" || got[1].Text != "status" {
		t.Fatalf("got %+v", got)
	}
}
