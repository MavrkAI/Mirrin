package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/push"
)

// What the twin does unprompted (a routine's result, a reminder, a tip)
// reaches an owner with no messaging app too. It goes to the presence
// screen, with at most one desktop notification, and it is read out loud
// only to someone in the room.

// screenChat is the presence screen's conversation.
const screenChat = "screen:local"

// spokeWithin is how recently the owner must have talked to the twin out
// loud for something other than a reminder to be read out to them.
const spokeWithin = 10 * time.Minute

// defaultQuietHours are when nothing is read out loud and what can wait is
// held (held.go), unless the owner set quiet hours of their own.
const defaultQuietHours = "22:00-07:00"

// proactiveChatKey is where the twin's own messages go: the owner's first
// messaging app with an owner chat, otherwise the screen. Voice and the
// terminal don't count: a 7am briefing isn't read to an empty room, and a
// terminal may not be open at all.
func (d *Daemon) proactiveChatKey() string {
	d.chmu.RLock()
	defer d.chmu.RUnlock()
	for _, name := range channels.MessagingOrder {
		if name == "voice" || name == "cli" {
			continue
		}
		if ch, ok := d.channels[name]; ok && ch.OwnerChatID() != "" {
			return name + ":" + ch.OwnerChatID()
		}
	}
	return screenChat
}

// toScreen delivers a message for the screen that no messaging app took.
// It is on the screen already (notify published it). This adds one desktop
// notification, and speech for someone in the room. It returns chatKey, so
// the message is kept in the screen's conversation and a reply there ("yes,
// set it up") has context.
func (d *Daemon) toScreen(ctx context.Context, chatKey, text string) (string, error) {
	src, hasSrc := events.SourceFrom(ctx)
	ping := screenPing(src, text)
	var pingErr error
	if ping != "" {
		d.store.Audit(ctx, "message.out.notification", chatKey, truncate(ping, 300))
		pingErr = desktopNotify(d.Config().Name, ping)
	}
	spoken := d.sayInRoom(ctx, src, text)
	if pingErr != nil && ping != "" && !spoken && !keptOnScreen(src, hasSrc) {
		// The notification was what had to tell the owner, and nothing
		// else carried it: say it didn't go, so a reminder is tried again.
		return chatKey, pingErr
	}
	if pingErr != nil {
		d.log.Warn("desktop notification", "err", pingErr)
	}
	return chatKey, nil
}

// keptOnScreen reports whether a message stays on the screen whether or not
// its notification showed: anything the twin sent on its own stays under
// Left for you (a question, under Needs you). A reminder or a follow-up has
// to be seen on time, and a notice with no source only passes by, so for
// those a notification that failed means the message didn't go.
func keptOnScreen(src events.Source, hasSrc bool) bool {
	return hasSrc && src.Kind != "reminder" && src.Kind != "followup"
}

// screenPing is the desktop notification for a message on the screen: for
// a routine's result, that it is ready; for what was held, that it is ready,
// or nothing when it is only tips and ideas; for a reminder, the reminder;
// for a follow-up, what it was about (what it found can be private, and
// stays on the screen); for a tip or an idea, nothing. Anything else is
// shown as written.
func screenPing(src events.Source, text string) string {
	if src.Quiet {
		return ""
	}
	switch src.Kind {
	case "protocol":
		if src.Name != "" {
			return upperFirst(src.Name) + " is ready."
		}
	case "held":
		return heldPing
	case "followup":
		if src.Name != "" {
			return "Following up on " + src.Name + "."
		}
		return "A follow-up is on the screen."
	case "tip", "idea":
		return ""
	}
	return text // reminders, and notices with no source
}

// sayInRoom reads text out loud on the voice channel, but only to someone
// in the room: voice is on, it isn't quiet hours, and it is a reminder or
// the owner talked to the twin out loud in the last ten minutes. A
// follow-up is not a reminder here: what it found can be private. It
// reports whether it was said.
func (d *Daemon) sayInRoom(ctx context.Context, src events.Source, text string) bool {
	ch, ok := d.channel("voice")
	if !ok {
		return false
	}
	now := clock()
	if d.quietNow(now) {
		return false
	}
	if src.Kind != "reminder" && now.Sub(time.Unix(0, d.heardAloud.Load())) > spokeWithin {
		return false
	}
	if err := d.deliver(ctx, ch, voiceChat, ch.OwnerChatID(), text); err != nil {
		d.log.Warn("say on voice", "err", err)
		return false
	}
	return true
}

// quietNow reports whether now is in quiet hours (held.go's quietHours), in
// the twin's time zone.
func (d *Daemon) quietNow(now time.Time) bool {
	return push.InQuiet(d.quietHours(), now.In(d.location()))
}

// noteHeard remembers when the owner last talked to the twin out loud, so
// what it has to say then can be said to them.
func (d *Daemon) noteHeard(in channels.Inbound) {
	if in.IsOwner && in.Key() == voiceChat {
		d.heardAloud.Store(clock().UnixNano())
	}
}

// upperFirst capitalises the first letter: "morning briefing" becomes
// "Morning briefing".
func upperFirst(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
