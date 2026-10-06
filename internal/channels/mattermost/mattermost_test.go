package mattermost

import (
	"context"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func TestOnPost(t *testing.T) {
	c := New("https://mm.example", "tok", "akshay", false, nil)
	c.selfID, c.selfName = "bot1", "mavrk"
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	now := time.Now().UnixMilli()
	c.onPost(context.Background(), post{ID: "1", UserID: "u1", ChannelID: "d1", Message: "hello", CreateAt: now}, "D", "akshay", h)
	c.onPost(context.Background(), post{ID: "2", UserID: "u1", ChannelID: "c1", Message: "@mavrk status", CreateAt: now}, "O", "akshay", h)
	c.onPost(context.Background(), post{ID: "3", UserID: "u1", ChannelID: "c1", Message: "no mention", CreateAt: now}, "O", "akshay", h)
	c.onPost(context.Background(), post{ID: "4", UserID: "u2", ChannelID: "d2", Message: "hi", CreateAt: now}, "D", "bob", h)
	if len(got) != 2 || !got[0].IsOwner || c.OwnerChatID() != "d1" || got[1].Text != "status" {
		t.Fatalf("got %+v", got)
	}
}
