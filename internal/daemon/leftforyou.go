package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// Left for you: what the twin sent on its own (a routine's result, a
// reminder, a watcher's news, a task's result, a tip, an idea) is kept in
// one calm place on the screen, so a briefing that came while you were out
// is still there when you walk past. It holds the last 12, for 18 hours,
// and counts nothing. A question or a request to approve isn't kept: Needs
// you shows those until they are answered.

// leftKey is the kv entry that holds the list, newest last.
const leftKey = "left_for_you"

const (
	leftMax = 12             // items kept
	leftFor = 18 * time.Hour // how long one stays
)

// Left is one message under Left for you.
type Left struct {
	ID     string `json:"id"`
	Source string `json:"source"` // what it came from: protocol, reminder, watch, task, tip, idea
	Title  string `json:"title"`  // "Morning briefing", "Reminder", "Calendar"
	// Text is what the twin said. A screen that only looks gets the title
	// alone (api's screen_private.go).
	Text      string    `json:"text,omitempty"`
	At        time.Time `json:"at"`
	Briefing  bool      `json:"briefing,omitempty"`  // stays open on the screen until noon
	HeldUntil time.Time `json:"held_until,omitzero"` // kept back until then (quiet hours, a meeting)
}

// leftMu keeps two messages arriving at once from losing one another.
var leftMu sync.Mutex

// showMessage puts a message the twin is sending on the screens. One it
// sent on its own (it carries a source) to the owner is a "message", kept
// under Left for you; anything else is a "notice", as before.
func (d *Daemon) showMessage(ctx context.Context, chatKey, text string) {
	src, ok := events.SourceFrom(ctx)
	if !ok || d.someoneElses(homeKey(chatKey)) {
		// Its data (where it went) marks it as the owner's message, not a
		// system line: a wall screen that only looks doesn't show it.
		d.bus.Publish(events.Event{Kind: "notice", Text: text, Data: map[string]string{"channel": channelOf(chatKey)}})
		return
	}
	l := Left{Source: src.Kind, Title: d.leftTitle(src), At: clock(), Briefing: src.Briefing}
	if src.Kind != "question" && src.Kind != "approval" {
		l = d.keepLeft(ctx, l, text)
	}
	d.bus.Publish(events.Event{Kind: "message", Text: text, Data: l})
}

// someoneElses reports whether key is a messaging chat the twin can see is
// not the owner's own (a reminder someone set in a group).
func (d *Daemon) someoneElses(key string) bool {
	if !channels.IsMessaging(key) {
		return false
	}
	name, id := channels.SplitKey(key)
	ch, ok := d.channel(name)
	return ok && id != ch.OwnerChatID()
}

// leftTitle names a message by what it came from: a routine by its name
// ("Morning briefing"), a task by its title, the watcher by what it
// watches.
func (d *Daemon) leftTitle(src events.Source) string {
	switch src.Kind {
	case "reminder":
		return "Reminder"
	case "watch":
		switch src.Name {
		case "calendar":
			return "Calendar"
		case "gmail", "inbox":
			return "Inbox"
		}
	case "tip":
		return "From " + d.Config().Name
	case "idea":
		return "An idea"
	case "task", "question", "approval":
		if src.Name == "" {
			return "Tasks"
		}
		return src.Name
	}
	if src.Name != "" {
		return upperFirst(src.Name)
	}
	return "From " + d.Config().Name
}

// keepLeft adds l, saying text, to Left for you and returns it with its id.
// What is too old, or one too many, is dropped.
func (d *Daemon) keepLeft(ctx context.Context, l Left, text string) Left {
	leftMu.Lock()
	defer leftMu.Unlock()
	list := d.leftForYou(ctx)
	for n := l.At.UnixNano(); ; n++ { // two in the same instant still get an id each
		l.ID = strconv.FormatInt(n, 36)
		if !slices.ContainsFunc(list, func(o Left) bool { return o.ID == l.ID }) {
			break
		}
	}
	kept := l
	kept.Text = text
	list = append(list, kept)
	if len(list) > leftMax {
		list = list[len(list)-leftMax:]
	}
	b, err := json.Marshal(list)
	if err == nil {
		err = d.store.Set(ctx, leftKey, string(b))
	}
	if err != nil {
		d.log.Warn("left for you", "err", err)
	}
	return l
}

// leftForYou is what Left for you holds now, oldest first: never more than
// twelve, none older than 18 hours.
func (d *Daemon) leftForYou(ctx context.Context) []Left {
	var list []Left
	if raw, _ := d.store.Get(ctx, leftKey); raw != "" {
		_ = json.Unmarshal([]byte(raw), &list) // unreadable: start again
	}
	now := clock()
	list = slices.DeleteFunc(list, func(l Left) bool { return now.Sub(l.At) > leftFor })
	if len(list) > leftMax {
		list = list[len(list)-leftMax:]
	}
	if list == nil {
		list = []Left{} // the screen gets a list, never null
	}
	return list
}
