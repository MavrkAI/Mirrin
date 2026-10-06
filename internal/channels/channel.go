// Package channels defines how people reach the twin: WhatsApp, Telegram,
// the terminal, and anything the community adds.
package channels

import (
	"context"
	"strings"
)

// Inbound is a message arriving from a person.
type Inbound struct {
	Channel string
	// ChatID is channel-specific (a WhatsApp JID, "terminal").
	ChatID string
	Sender string
	Text   string
	// Media names an attachment (Voice, Photo, Video, File). Text is then
	// its caption, if any.
	Media string
	// Attachment is the file behind Media when the transport can hand it
	// over (media.go); without one the twin says it can't open it here.
	Attachment *Attachment `json:"-"`
	// IsOwner is true when the sender is the configured principal.
	IsOwner bool
}

// Key returns the conversation key used for memory and approvals.
func (i Inbound) Key() string { return i.Channel + ":" + i.ChatID }

// Handler receives inbound messages.
type Handler func(ctx context.Context, in Inbound)

// Channel is a two-way transport.
type Channel interface {
	Name() string
	// Start connects and delivers messages to handler until ctx ends. It
	// returns early only when it can't connect or stay connected; the daemon
	// then starts a fresh instance after a pause, unless the error is Fatal.
	// A transport that reconnects by itself should say so through Reporter.
	// One that holds something beyond Start (a database) implements
	// io.Closer; the daemon closes each instance once it is done with it.
	Start(ctx context.Context, handler Handler) error
	// Send delivers text to a chat on this channel.
	Send(ctx context.Context, chatID, text string) error
	// OwnerChatID is where proactive messages for the principal go ("" if unknown).
	OwnerChatID() string
}

// SplitKey reverses Inbound.Key.
func SplitKey(key string) (channel, chatID string) {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

// Stream receives a reply as it is generated.
type Stream interface {
	// Write delivers a fragment of text.
	Write(delta string)
	// Close ends the reply and blocks until it has been delivered (spoken/printed).
	Close()
}

// Noter is a Stream that can show a brief status note ("Checking your calendar.")
// while the reply is still being worked on.
type Noter interface {
	Note(text string)
}

// Streamer is a Channel that can deliver replies incrementally.
type Streamer interface {
	Channel
	// OpenStream starts an incremental reply to a chat.
	OpenStream(ctx context.Context, chatID string) Stream
}

// Linker is a Channel that knows where the owner opens the conversation.
type Linker interface {
	ChatLink() (label, url string)
}

// ImageSender is a Channel that can deliver a picture (e.g. a screenshot before an approval).
type ImageSender interface {
	SendImage(ctx context.Context, chatID, path, caption string) error
}

// Warner is a Channel with something the owner should fix, such as a setting
// that keeps them from being recognised. It is shown on the Channels page
// ("" when all is well).
type Warner interface {
	Warning() string
}

// MessagingOrder ranks channels for proactive messages: the ones people
// carry on a phone first, then desktop chat, then slow transports.
var MessagingOrder = []string{"whatsapp", "telegram", "imessage", "signal", "discord", "slack", "matrix", "mattermost", "zulip", "irc", "mail", "voice", "cli"}

// IsMessaging reports whether a chat key belongs to a text-messaging channel.
func IsMessaging(chatKey string) bool {
	name, _ := SplitKey(chatKey)
	switch name {
	case "voice", "cli", "":
		return false
	}
	for _, n := range MessagingOrder {
		if n == name {
			return true
		}
	}
	return false
}

// SplitText breaks s into chunks of at most n bytes, preferring newline boundaries.
func SplitText(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := -1
		for i := n; i > n/2; i-- {
			if s[i] == '\n' {
				cut = i
				break
			}
		}
		if cut < 0 {
			cut = n
			for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
				cut-- // don't split a UTF-8 sequence
			}
		}
		out = append(out, s[:cut])
		s = s[cut:]
		for len(s) > 0 && s[0] == '\n' {
			s = s[1:]
		}
	}
	return append(out, s)
}

// MatchOwner reports whether any of the sender's identifiers equals owner
// (case-insensitive, ignoring a leading @).
func MatchOwner(owner string, ids ...string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(strings.ToLower(s))
		return strings.TrimPrefix(s, "@")
	}
	o := norm(owner)
	if o == "" {
		return false
	}
	for _, id := range ids {
		if id != "" && norm(id) == o {
			return true
		}
	}
	return false
}
