package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/skills/phone"
)

// phoneChat is the pseudo-chat live calls run under. It is nobody's chat:
// what a call leaves for the owner (a reminder, a request to approve) goes
// to the owner's chat instead (reach in approvals.go, remind below).
const phoneChat = "voice:phone"

// phoneKey is the conversation for one live call. Twilio waits about 12 s for
// each answer, so the key starts with "voice:": turns get the voice model and
// effort and the spoken delivery style. The "#" makes it a scratch
// conversation, pruned with the others.
func phoneKey(callID string) string { return phoneChat + "#call-" + callID }

// remind delivers what the heartbeat sends (reminders, protocol output) with
// Notify. A reminder set during a call belongs to the call's pseudo-chat
// (memory.LiveKey keeps "voice:phone"), which is nobody's: it goes to the
// owner's chat, not out loud in the room.
func (d *Daemon) remind(ctx context.Context, chatKey, text string) error {
	if memory.LiveKey(chatKey) == phoneChat {
		if owner := d.ownerChatKey(); owner != "" {
			chatKey = owner
		}
	}
	return d.Notify(ctx, chatKey, text)
}

// callAsked tells the owner that the call the twin is making for them needs
// their yes: the person on the line can't give it (agent.ask tells the model
// so), and the request is answered from the owner's chat (reach).
func (d *Daemon) callAsked(ap memory.Approval) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	owner := d.ownerChatKey()
	text := fmt.Sprintf("On the phone call I'm making for you, I'd need your OK to %s\n%s", ap.Summary, answerHint(owner, ap.ID))
	if owner == "" {
		if err := desktopNotify(d.cfg.Name, text); err != nil {
			d.log.Warn("tell owner about a call's request", "err", err)
		}
		return
	}
	if err := d.Notify(ctx, owner, text); err != nil {
		d.log.Warn("tell owner about a call's request", "chat", owner, "err", err)
	}
}

// phoneTurn runs one exchange of a live two-way call.
func (d *Daemon) phoneTurn(ctx context.Context, callID, purpose, heard string) (string, error) {
	prompt := "[You are on a live phone call, speaking on the user's behalf to someone else; the person on the line is not the user. Purpose and facts: " + purpose + ". Reply with only what you say next, one or two short spoken sentences, no markdown. When the matter is settled or they want to end, say goodbye and add [HANGUP].]"
	if heard == "" {
		return d.agent.RunTask(ctx, phoneKey(callID), prompt+"\n(They answered but said nothing yet.)")
	}
	return d.agent.RunTask(ctx, phoneKey(callID), prompt+"\nThey said: \""+heard+"\"")
}

// phoneEnded tells the owner how a two-way call went, in the conversation it
// was placed from, so "did they book it?" never needs asking.
func (d *Daemon) phoneEnded(c phone.Ended) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	key := c.ChatKey
	if key == "" || memory.IsScratch(key) {
		// Placed from a background run: tell the owner where they are. (An
		// IRC room's '#' is its name, not a run: the room is told.)
		key = d.ownerChatKey()
	}
	text := callSummary(c)
	if text == "" && d.agent != nil {
		task := fmt.Sprintf("A phone call you placed for the user to %s has ended. What it was for: %s\nTranscript:\n%s\n\nTell the user in one or two lines how it went: what was agreed and anything they still need to do.",
			c.To, c.Purpose, strings.Join(c.Transcript, "\n"))
		out, err := d.agent.RunTask(ctx, scratchKey(key, "call"), task)
		if err != nil {
			d.log.Warn("call summary", "err", err)
		} else if !strings.Contains(out, "NOTHING_TO_REPORT") {
			text = strings.TrimSpace(out)
		}
	}
	if text == "" {
		text = fmt.Sprintf("The call to %s has ended. What was said:\n%s", c.To, strings.Join(c.Transcript, "\n"))
	}
	if key == "" {
		key = "phone:" + c.To // nobody to tell on a channel: Send falls back to a notification
	}
	if err := d.Notify(ctx, key, text); err != nil {
		d.log.Warn("call ended: tell owner", "err", err)
	}
}

// callSummary words a call that didn't connect ("" when there is a
// conversation to summarise).
func callSummary(c phone.Ended) string {
	switch c.Status {
	case "busy":
		return fmt.Sprintf("I rang %s but the line was busy. Shall I try again later?", c.To)
	case "no-answer":
		return fmt.Sprintf("I rang %s but nobody picked up. Shall I try again later?", c.To)
	case "failed":
		return fmt.Sprintf("The call to %s didn't go through; the number may be wrong or unreachable.", c.To)
	case "canceled":
		return fmt.Sprintf("The call to %s was cancelled before anyone answered.", c.To)
	}
	switch len(c.Transcript) {
	case 0:
		return fmt.Sprintf("I rang %s but the call ended before I could say anything.", c.To)
	case 1:
		return fmt.Sprintf("I rang %s and gave my opening line, but they hung up without replying.", c.To)
	}
	return ""
}
