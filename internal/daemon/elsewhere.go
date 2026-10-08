package daemon

import (
	"slices"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// localChats are the front ends on this Mac whose chats are the owner's
// own: the microphone, the terminal, the presence screen and the app.
var localChats = []string{"voice", "cli", "screen", "api"}

// sharedChat reports whether key is one of the owner's own conversations,
// which see each other's last few exchanges (agent/elsewhere.go): the
// owner's chat on a messaging app, or a front end on this Mac. Never a
// group, a call, a scratch run, or a channel where anyone could claim to be
// the owner (mail, IRC).
func (d *Daemon) sharedChat(key string) bool {
	name, id := channels.SplitKey(key)
	if id == "" || memory.IsScratch(key) || isCall(key) || forgeable(name) {
		return false
	}
	if !slices.Contains(localChats, name) && !channels.IsMessaging(key) {
		return false
	}
	return d.ownersOwnChat(key)
}
