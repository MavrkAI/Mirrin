//go:build !nowhatsapp

package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestOwnChatOnlyCountsTheSelfChat(t *testing.T) {
	selfPN := types.NewJID("61400000001", types.DefaultUserServer)
	selfLID := types.NewJID("123456789", types.HiddenUserServer)
	friend := types.NewJID("61400000002", types.DefaultUserServer)
	friendLID := types.NewJID("987654321", types.HiddenUserServer)
	self := []types.JID{selfPN, selfLID}

	cases := []struct {
		name string
		src  types.MessageSource
		want bool
	}{
		{"message yourself", types.MessageSource{Chat: selfPN, Sender: selfPN, IsFromMe: true}, true},
		{"message yourself by LID", types.MessageSource{Chat: selfLID, Sender: selfLID, IsFromMe: true}, true},
		{"own LID not stored yet", types.MessageSource{Chat: selfLID, RecipientAlt: selfPN, IsFromMe: true}, true},
		{"texting a friend", types.MessageSource{Chat: friend, Sender: selfPN, IsFromMe: true}, false},
		{"texting a friend by LID", types.MessageSource{Chat: friendLID, Sender: selfLID, RecipientAlt: friend, IsFromMe: true}, false},
		{"not paired yet", types.MessageSource{Chat: selfPN, IsFromMe: true}, false},
	}
	for _, c := range cases {
		s := self
		switch c.name {
		case "not paired yet":
			s = nil
		case "own LID not stored yet":
			s = []types.JID{selfPN}
		}
		if got := ownChat(c.src, s); got != c.want {
			t.Errorf("%s: ownChat = %v, want %v", c.name, got, c.want)
		}
	}
}
