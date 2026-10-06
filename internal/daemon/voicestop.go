package daemon

import (
	"context"
	"slices"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// A "stop" can come as a voice note. Its words are known only once it is
// transcribed, and a typed stop is caught as it arrives (stopPosted), before
// it would wait behind the turn it stops. So while the owner's turn runs in
// a chat, a short voice note from them is heard at once, beside that turn,
// and if it says stop, it stops the turn there and then. Either way it is
// transcribed once: its own turn uses what was heard. A voice note heard in
// its own turn that says stop is taken like a typed one (spokenStop).

// stopNoteSeconds is the longest voice note heard ahead of its turn: a stop
// is a word or two, and anything longer waits its turn as before.
const stopNoteSeconds = 20

// preheard is a voice note being heard ahead of its turn.
type preheard struct {
	done  chan struct{} // closed once heard; words, why and stop are set before
	words string
	why   *unopened
	stop  bool     // it said stop and stopped what it waited behind: nothing more to answer
	run   *running // its own turn, once a mailbox worker took it (guarded by the conversation's mu)
}

type preheardKey struct{}

// mayBeSpokenStop reports whether in is a voice note from the owner short
// enough to be a stop, and worth hearing ahead of its turn.
func (d *Daemon) mayBeSpokenStop(in channels.Inbound) bool {
	a := in.Attachment
	if !in.IsOwner || a == nil || d.paused.Load() {
		return false
	}
	if in.Media != channels.Voice && !(in.Media == channels.File && channels.MediaKind(a.Mime) == channels.Voice) {
		return false
	}
	return a.Seconds <= stopNoteSeconds && strings.TrimSpace(in.Text) == ""
}

// hearAhead transcribes an owner's voice note while their turn runs in c,
// and if it says stop, stops that turn (and lets go of the owner's messages
// that were waiting behind it, as a typed stop does).
func (d *Daemon) hearAhead(ctx context.Context, c *conversation, in channels.Inbound, pre *preheard) {
	words, why := d.hear(ctx, in)
	var cut []stoppedWork
	if why == nil {
		if stop, whole, _ := d.isStop(words); stop && whole {
			cut = c.stopAhead(pre)
		}
	}
	pre.words, pre.why, pre.stop = words, why, len(cut) > 0
	close(pre.done)
	if len(cut) > 0 {
		d.store.Audit(context.WithoutCancel(ctx), "turn.stopped", in.Key(), truncate("(voice note) "+strings.Join(labels(cut), "; "), 300))
	}
}

// stopAhead is stop for a voice note that said it: it cuts what runs here
// but the voice note's own turn, and lets go of the owner's messages queued
// before it, and the voice note itself, which needs no answer of its own
// (the stopped turn says what stopped). Messages sent after it stay.
func (c *conversation) stopAhead(pre *preheard) []stoppedWork {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cut []*running
	for _, r := range c.runs {
		if r == pre.run || !r.stopped.CompareAndSwap(false, true) {
			continue
		}
		r.cancel()
		cut = append(cut, r)
	}
	if len(cut) == 0 {
		return nil // it had finished: the voice note is answered in its turn
	}
	at := slices.IndexFunc(c.inbox, func(q queued) bool { return q.pre == pre })
	dropped := int32(0)
	if at >= 0 {
		kept := make([]queued, 0, len(c.inbox))
		for i, q := range c.inbox {
			switch {
			case i == at:
			case i < at && q.in.IsOwner:
				dropped++
			default:
				kept = append(kept, q)
			}
		}
		c.inbox = kept
	}
	for _, r := range cut {
		r.dropped.Store(dropped)
	}
	out := make([]stoppedWork, len(cut))
	for i, r := range cut {
		out[i] = stoppedWork{r.at, r.label(), r}
	}
	return out
}

// heardStop reports whether the voice note being answered said stop while
// it waited, and has been dealt with.
func heardStop(ctx context.Context) bool {
	pre, ok := ctx.Value(preheardKey{}).(*preheard)
	if !ok {
		return false
	}
	select {
	case <-pre.done:
		return pre.stop
	default:
		return false
	}
}

// spokenStop takes a voice note that says stop, heard in its own turn, as a
// typed stop: it reaches work of the owner's elsewhere, or messages still
// waiting, instead of being answered by the model.
func (d *Daemon) spokenStop(ctx context.Context, in channels.Inbound) bool {
	if !in.IsOwner || !channels.IsVoiceNote(in.Text) {
		return false
	}
	said := in
	said.Text = channels.VoiceNoteWords(in.Text)
	if stop, whole, _ := d.isStop(said.Text); !stop || !whole {
		return false
	}
	if run, ok := ctx.Value(queuedRunKey{}).(*running); ok {
		untrack(run) // its own turn is not what it stops
	}
	return d.stopPosted(ctx, said)
}
