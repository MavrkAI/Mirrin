package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/heartbeat"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// The owner's answer to the travel welcome's offer (heartbeat's
// travelpeople.go: "Want a nudge tomorrow to see if Dan's free for
// coffee?"). A yes sets a reminder for the owner, at ten tomorrow where
// they are; it never messages the person. A no lets it go, and "stop
// telling me these" turns the offers off ("start telling me these again"
// turns them back on).

// travelOfferFor is how long after the notice a yes still answers it.
const travelOfferFor = 24 * time.Hour

var (
	// A bare "ok" acknowledges the notice; it isn't a yes to the offer.
	travelYes = map[string]bool{"yes": true, "yes please": true, "yeah": true, "yep": true,
		"please": true, "please do": true, "go on": true, "go ahead": true,
		"sounds good": true, "yes thanks": true, "yes thank you": true, "good idea": true}
	travelNo = map[string]bool{"no": true, "no thanks": true, "no thank you": true, "nope": true, "not now": true,
		"nah": true, "no need": true}
	travelStop = map[string]bool{"stop telling me these": true, "dont tell me these": true, "don't tell me these": true,
		"no more of these": true, "stop these": true, "stop doing that": true, "dont do that again": true,
		"don't do that again": true}
	travelResume = map[string]bool{"start telling me these again": true, "tell me these again": true,
		"start telling me these": true, "turn these back on": true}
)

// answerTravelOffer settles the travel welcome's offer when the owner's
// whole message answers it and it was the twin's last word in this chat.
func (d *Daemon) answerTravelOffer(ctx context.Context, in channels.Inbound) (string, bool) {
	key := in.Key()
	if !in.IsOwner || forgeable(in.Channel) || memory.IsScratch(key) || !d.ownersOwnChat(key) {
		return "", false
	}
	words := strings.ToLower(strings.Join(strings.Fields(strings.TrimFunc(in.Text, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) && r != '\'' || unicode.IsSymbol(r)
	})), " "))
	words = strings.NewReplacer(",", "", ".", "", "!", "").Replace(words)
	if travelResume[words] {
		return d.resumeTravelOffers(ctx, key, in.Text)
	}
	if !travelYes[words] && !travelNo[words] && !travelStop[words] {
		return "", false
	}
	o, ok := d.travelOfferOpen(ctx, key)
	if !ok {
		return "", false
	}
	kind := "yes"
	switch {
	case travelStop[words]:
		kind = "stop"
	case travelNo[words]:
		kind = "no"
	}
	return d.settleTravelOffer(ctx, key, in.Text, kind, o)
}

// travelOfferOpen is the travel welcome's offer while it still waits on an
// answer in key: made under a day ago, and the twin's last word there.
func (d *Daemon) travelOfferOpen(ctx context.Context, key string) (heartbeat.TravelOffer, bool) {
	raw, _ := d.store.Get(ctx, heartbeat.TravelOfferKey)
	var o heartbeat.TravelOffer
	if raw == "" || json.Unmarshal([]byte(raw), &o) != nil || o.Ask == "" {
		return o, false
	}
	if clock().Sub(o.At) > travelOfferFor || !d.offeredLast(ctx, key, o.Ask) {
		return o, false
	}
	return o, true
}

// settleTravelOffer answers the offer o as kind says: "yes" sets the owner
// a reminder (it never messages the person), "no" lets it go, and "stop"
// turns the offers off. said, the owner's words, is kept in the chat with
// the reply.
func (d *Daemon) settleTravelOffer(ctx context.Context, key, said, kind string, o heartbeat.TravelOffer) (string, bool) {
	now := clock()
	_ = d.store.Unset(ctx, heartbeat.TravelOfferKey) // answered, whichever way
	var reply string
	switch kind {
	case "stop":
		_ = d.store.Set(ctx, heartbeat.TravelPeopleOffKey, "1")
		reply = `Understood. I won't bring up people or plans when you travel. Say "start telling me these again" if you'd like them back.`
	case "no":
		reply = "No problem. Enjoy the trip."
	default:
		loc := d.location()
		due := nudgeAt(now, loc)
		if _, err := d.store.AddReminder(ctx, key, due, o.Remind); err != nil {
			d.log.Warn("travel offer: reminder", "err", err)
			return "", false
		}
		d.store.Audit(ctx, "travel.offer", key, "reminder set: "+o.Remind)
		reply = fmt.Sprintf("Done. I'll nudge you at %s.", backAt(due, now, loc))
		if o.Who != "" {
			reply += fmt.Sprintf(" I won't message %s myself.", o.Who)
		}
	}
	d.recordTurn(ctx, key, said, reply)
	return reply, true
}

// resumeTravelOffers turns the offers back on after "stop telling me
// these". While they are on, the words are left to the conversation.
func (d *Daemon) resumeTravelOffers(ctx context.Context, key, said string) (string, bool) {
	if off, _ := d.store.Get(ctx, heartbeat.TravelPeopleOffKey); off != "1" {
		return "", false
	}
	if err := d.store.Unset(ctx, heartbeat.TravelPeopleOffKey); err != nil {
		d.log.Warn("travel offer: resume", "err", err)
		return "", false
	}
	reply := "Happy to. Next time you travel, I'll mention anyone you've told me about there."
	d.recordTurn(ctx, key, said, reply)
	return reply, true
}

// recordTurn keeps the owner's words and the reply to them in the chat,
// for a message answered without the model.
func (d *Daemon) recordTurn(ctx context.Context, key, said, reply string) {
	for _, m := range []llm.Message{llm.Text(llm.RoleUser, said), llm.Text(llm.RoleAssistant, reply)} {
		if err := d.store.AppendMessage(context.WithoutCancel(ctx), key, m); err != nil {
			d.log.Warn("answer: record", "chat", key, "err", err)
		}
	}
}

// nudgeAt is ten in the morning tomorrow where the owner is; in the small
// hours (a late landing), "tomorrow" is the coming morning.
func nudgeAt(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	day := n.Day() + 1
	if n.Hour() < 5 {
		day = n.Day()
	}
	return time.Date(n.Year(), n.Month(), day, 10, 0, 0, 0, loc)
}

// offeredLast reports whether the twin's latest message in key ends with
// the offer's question.
func (d *Daemon) offeredLast(ctx context.Context, key, ask string) bool {
	h, err := d.store.History(ctx, key, 8)
	if err != nil {
		return false
	}
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == llm.RoleAssistant {
			return strings.HasSuffix(strings.TrimSpace(h[i].PlainText()), strings.TrimSpace(ask))
		}
	}
	return false
}
