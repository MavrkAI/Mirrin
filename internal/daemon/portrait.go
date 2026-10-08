package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// The portrait is the twin's own summary of its owner, and it goes into
// every turn, onto the screens and into the export. Forgetting a fact
// scrubs the fact's wording, but a portrait can say the same thing in other
// words ("you're quietly looking for a new role"), so a forget sets the
// whole portrait aside. It isn't rewritten there and then: the owner's own
// messages keep their words, and a rewrite could bring the forgotten thing
// straight back. The Sunday job, or Refresh on the memory page, writes the
// next one, and that clears the mark.

// setPortraitAside removes the portrait after a forget and notes when, so
// the memory page can say why it's gone. With no portrait there is nothing
// to set aside, and the page says nothing about it.
func (d *Daemon) setPortraitAside(ctx context.Context) {
	defer d.draft.forgot()() // firstdraft.go: a draft being written isn't kept
	had, _ := d.store.Get(ctx, "portrait")
	if err := d.store.DeletePortrait(ctx); err != nil {
		d.log.Warn("portrait: couldn't set it aside after a forget", "err", err)
		return
	}
	if had != "" {
		_ = d.store.Set(ctx, memory.KeyPortraitAside, time.Now().UTC().Format(time.RFC3339))
	}
}

// portraitAside reports whether the portrait was set aside after a forget
// and no new one has been written since.
func portraitAside(ctx context.Context, store *memory.Store) bool {
	if p, err := store.GetPortrait(ctx); err != nil || p.Text != "" {
		return false
	}
	v, _ := store.Get(ctx, memory.KeyPortraitAside)
	return v != ""
}

// Each portrait comes with a line on what's new since the last one ("you've
// been guarding Friday afternoons"). The screen's "How Mirrin sees you" shows
// the portrait in full, the line for a week, and asks whether it's right:
// "That's you" puts the question away until the next portrait, and "Not
// quite" opens the text box. The line is also said once with the owner's
// next hello on this computer (portrait_noticed.go); none of it is sent
// anywhere else, and a wall screen that only looks never gets it
// (api/screen_private.go). The
// first portrait is written at the end of the first week, so the last tip
// can point to it.

// Kept beside the portrait. DeletePortrait removes the first two with it.
const (
	keyPortraitPrev = "portrait.prev" // the portrait before this one
	keyPortraitNew  = "portrait.new"  // this portrait's line on what's new, as portraitNews
	keyPortraitAck  = "portrait.ack"  // when the portrait the owner said "That's you" to was written
)

// weekOneTip is the last first-week tip once there's a portrait to see.
const weekOneTip = "I've written down how I see you so far. It's on the screen; tell me what I got wrong."

// portraitNews is a portrait's line on what's new, and when that portrait
// was written: a line kept from an earlier portrait is never shown under a
// newer one.
type portraitNews struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// writePortrait has the model write a fresh portrait, and keeps the one
// before it and its line on what's new. Every portrait is written here:
// the Sunday job and the end of week one (within the background budget,
// through budgetPortrait), Refresh on the memory page and the portrait job
// run by hand.
func (d *Daemon) writePortrait(ctx context.Context, key string) (string, error) {
	old, _ := d.store.GetPortrait(ctx)
	text, news, err := d.agent.WritePortrait(ctx, key)
	if err != nil {
		return "", err
	}
	if old.Text != "" {
		_ = d.store.Set(ctx, keyPortraitPrev, old.Text)
	}
	if news == "" {
		_ = d.store.Unset(ctx, keyPortraitNew)
		return text, nil
	}
	p, _ := d.store.GetPortrait(ctx)
	b, _ := json.Marshal(portraitNews{Text: news, At: p.UpdatedAt})
	_ = d.store.Set(ctx, keyPortraitNew, string(b))
	return text, nil
}

// sundayPortrait is the weekly portrait, on Sunday morning. It is written
// in the conversation of wherever the owner hears from the twin, the
// screen's when there's no messaging app.
func (d *Daemon) sundayPortrait(ctx context.Context) {
	if _, err := d.budgetPortrait(ctx, scratchKey(d.proactiveChatKey(), "portrait")); err != nil {
		d.log.Warn("portrait", "err", err)
	}
}

// weekOnePortrait is for the last day of the first week: it writes the
// first portrait when there isn't one, and reports whether there is one to
// point to. One set aside after a forget isn't rewritten here (see above).
func (d *Daemon) weekOnePortrait(ctx context.Context) bool {
	p, err := d.store.GetPortrait(ctx)
	draft := err == nil && d.portraitIsDraft(ctx, p) // firstdraft.go: the regular one takes over
	if err != nil || (p.Text != "" && !draft) {
		return err == nil
	}
	if portraitAside(ctx, d.store) {
		return false
	}
	text, err := d.budgetPortrait(ctx, scratchKey(d.proactiveChatKey(), "portrait"))
	if err != nil {
		d.log.Warn("portrait: the first one wasn't written", "err", err)
	}
	// When the rewrite fails, the draft stays up, still labelled "First
	// draft", until the Sunday job replaces it: there is still one to
	// point to, and no draft is rewritten after the first week.
	return text != "" || draft
}

// screenPortrait puts the portrait on the screen, with its line on what's
// new for a week after it was written, and whether the owner has said it's
// right.
func (d *Daemon) screenPortrait(ctx context.Context, sd *ScreenData) {
	p, err := d.store.GetPortrait(ctx)
	if err != nil || p.Text == "" {
		return
	}
	sd.Portrait, sd.PortraitAt = p.Text, portraitStamp(p)
	sd.PortraitDraft = d.portraitIsDraft(ctx, p) // firstdraft.go
	var n portraitNews
	if raw, _ := d.store.Get(ctx, keyPortraitNew); raw != "" && json.Unmarshal([]byte(raw), &n) == nil &&
		n.At.Equal(p.UpdatedAt) && time.Since(n.At) < 7*24*time.Hour {
		sd.PortraitNew = n.Text
	}
	ack, _ := d.store.Get(ctx, keyPortraitAck)
	sd.PortraitAck = ack == sd.PortraitAt
}

// portraitStamp is when p was written, as the screen and "That's you" name
// it.
func portraitStamp(p memory.Portrait) string { return p.UpdatedAt.UTC().Format(time.RFC3339Nano) }

// AckPortrait records the owner's "That's you" for the portrait on the
// screen now, and the screen stops asking until the next one.
func (d *Daemon) AckPortrait(ctx context.Context) error {
	p, err := d.store.GetPortrait(ctx)
	if err != nil || p.Text == "" {
		return err
	}
	return d.store.Set(ctx, keyPortraitAck, portraitStamp(p))
}

var _ api.PortraitAcker = (*Daemon)(nil)
