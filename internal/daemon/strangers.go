package daemon

import (
	"context"
	"fmt"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// With reply_to_others on, someone other than the owner can ask the twin
// for something only the owner may allow. The request waits in that
// person's chat. The owner hears about it in their own chat and answers
// there, "yes N" or "no N" (on the presence screen when that chat is IRC or
// email, whose sender can be forged), and the person who asked hears how it
// went.

// answerHint says how the owner, reached at owner, can answer a request
// asked in another conversation.
func answerHint(owner string, id int64) string {
	if forgeable(channelOf(owner)) {
		// A yes by IRC or email can't decide a request made elsewhere (answerable).
		return fmt.Sprintf("Answer #%d on the presence screen: a yes by %s can't be checked for a request made in another conversation.", id, channelLabel(channelOf(owner)))
	}
	return fmt.Sprintf("Reply \"yes %d\" to let me, or \"no %d\".", id, id)
}

// strangerAsked tells the owner that someone is waiting on them.
func (d *Daemon) strangerAsked(ctx context.Context, ap memory.Approval, who string) {
	where, _ := channels.SplitKey(ap.ChatKey)
	if k, ok := config.ConnectorByName(where); ok && k.Label != "" {
		where = k.Label
	}
	owner := d.ownerChatKey()
	// Quoted and marked "not you": anyone can call themselves by the owner's name.
	text := fmt.Sprintf("%q on %s (not you) is asking me to %s\n%s", who, where, ap.Summary, answerHint(owner, ap.ID))
	ctx = context.WithoutCancel(ctx)
	if owner == "" {
		// Data marks it as someone's request, which a wall screen doesn't show.
		d.bus.Publish(events.Event{Kind: "notice", Text: text, Data: map[string]string{"channel": channelOf(ap.ChatKey)}})
		if err := desktopNotify(d.cfg.Name, text); err != nil {
			d.log.Warn("tell owner about a request", "err", err)
		}
		return
	}
	// The request is in their words, up to pages of them: the owner's
	// history keeps only who asked for what, not their text as the twin's.
	record := fmt.Sprintf("(I told you that %q on %s is asking me to use %s, as request #%d, and is waiting on your answer.)", who, where, ap.Tool, ap.ID)
	if err := d.notify(ctx, owner, text, record); err != nil {
		d.log.Warn("tell owner about a request", "chat", owner, "err", err)
	}
}

// strangerDecision carries out the owner's "yes N" or "no N", said in any
// of their chats, for something someone else asked for. It runs in that
// person's conversation and they hear the outcome; the owner gets a line
// saying what they were told. ok is false when N isn't such a request.
func (d *Daemon) strangerDecision(ctx context.Context, ap *memory.Approval, approve bool) (reply string, ok bool) {
	who, ok := d.agent.ApprovalRequester(ctx, ap.ID)
	if !ok {
		return "", false
	}
	if ap.Status != "pending" {
		return fmt.Sprintf("%s's request #%d is already %s.", who, ap.ID, ap.Status), true
	}
	told, err := d.decideElsewhere(ctx, ap, approve)
	if err != nil {
		d.log.Warn("decide for someone else", "id", ap.ID, "err", err)
		return fmt.Sprintf("I couldn't finish %s's request #%d: %s", who, ap.ID, llm.Friendly(d.explain(err))), true
	}
	if approve {
		return fmt.Sprintf("Done. I told %s: %s", who, truncate(told, 300)), true
	}
	return fmt.Sprintf("Understood, I won't. I told %s: %s", who, truncate(told, 300)), true
}
