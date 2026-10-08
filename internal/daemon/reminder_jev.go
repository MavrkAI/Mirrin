package daemon

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/jev"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// A reply to a reminder just sent that isn't one of the exact words
// ("paid it", "yep did that", "✅", "not yet", "can't right now") is asked of
// Jev, when it is switched on: done ticks it off, later puts it back an
// hour, and anything else, or any doubt, is conversation as before. Jev
// sees the reminder as the twin said it and the owner's reply, nothing more.

// reminderReplyMax is the longest reply, in characters, read as being
// about a reminder at all; the exact words stop at 40.
const reminderReplyMax = 200

// How sure Jev must be before a reply settles a reminder.
const (
	reminderDoneP     = 0.9
	reminderDoneConf  = 0.7
	reminderLaterP    = 0.85
	reminderLaterConf = 0.6
)

// reminderReplyFits reports whether text is short enough, and not empty,
// to be read as an answer to a reminder.
func reminderReplyFits(text string) bool {
	t := strings.TrimSpace(text)
	return t != "" && utf8.RuneCountInString(t) <= reminderReplyMax
}

// reminderReplyTyped reports whether text is one line the owner typed: not
// a photo, which only the model can look at, nor a voice note, a guess at
// what was said that the exact words never settle either, nor a message
// with a note about an attachment or more lines than an answer needs.
func reminderReplyTyped(text string) bool {
	t := strings.TrimSpace(text)
	return !channels.IsPhotoNote(t) && !channels.IsVoiceNote(t) && !strings.Contains(t, "\n")
}

// reminderReplyQuestion asks what the owner's reply says about the
// reminder. The options are sent in this order.
var reminderReplyQuestion = jev.Choice{
	Instructions: "The twin just sent the owner the reminder in `twin_said`. What does `owner_replied` say about that reminder?",
	Options: []jev.Opt{
		{Name: "done", Rubric: "The thing is done or dealt with ('paid it', 'yep did that', 'finished', 'all sorted thanks', '✅')."},
		{Name: "later", Rubric: "Remind again later without saying when ('not yet', 'remind me later', 'in a bit', 'can't right now')."},
		{Name: "drop", Rubric: "No longer wants it but doesn't say it is done ('forget it', 'doesn't matter now')."},
		{Name: "other", Rubric: "A question, a time or date, a partial or unclear answer, a message about something else, or one that also asks the twin something or asks it to do something else."},
	},
}

// reminderReplyState is all Jev is shown: the owner's own words and the
// reminder as the twin sent it to them.
type reminderReplyState struct {
	TwinSaid     string `json:"twin_said"`
	OwnerReplied string `json:"owner_replied"`
}

// judgeReminderReply settles r from the owner's reply in, when Jev is sure
// it says done or later; otherwise it reports false and the message is
// conversation. answerReminder has already checked that r went out in the
// owner's own chat in the last half hour and is the twin's last word there.
func (d *Daemon) judgeReminderReply(ctx context.Context, in channels.Inbound, r memory.Reminder, now time.Time) (string, bool) {
	if !reminderReplyFits(in.Text) || !reminderReplyTyped(in.Text) {
		return "", false
	}
	state := reminderReplyState{
		TwinSaid:     "Reminder: " + strings.TrimSpace(r.Text),
		OwnerReplied: strings.TrimSpace(in.Text),
	}
	res, ok := d.judge(ctx, "reminder-replies", state, map[string]jev.Question{"reply": reminderReplyQuestion})
	if !ok {
		return "", false
	}
	a, ok := res.Answers["reply"]
	if !ok || a.Type != "choice" {
		return "", false
	}
	key := in.Key()
	var reply string
	switch {
	case a.P("done") >= reminderDoneP && a.Confidence >= reminderDoneConf:
		reply, ok = d.tickReminder(ctx, key, r, " (jev)")
	case a.P("later") >= reminderLaterP && a.Confidence >= reminderLaterConf:
		reply, ok = d.snoozeReminder(ctx, key, r, now.Add(time.Hour), now)
	default:
		return "", false // drop, other, or not sure: the model answers
	}
	if !ok {
		return "", false
	}
	d.recordReminderReply(ctx, key, in.Text, reply)
	return reply, true
}
