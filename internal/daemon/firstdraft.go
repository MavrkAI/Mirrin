package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/memory"
	memskill "github.com/MavrkAI/Mirrin/internal/skills/memory"
)

// A first picture after five minutes, not a week. In the owner's first
// week, once they have told the twin about five things about themselves,
// it writes a first draft of the portrait from those things alone, and
// "How Mirrin sees you" shows it at once, labelled "First draft". Only what
// the owner said in their own chats, or added on the memory page, goes in:
// nothing from mail, the calendar, a routine or another person, and nothing
// private (health, money, relationships, secrets). It is rewritten at most
// once a day as they tell it more, until the end of week one, when the
// regular portrait takes over (weekOnePortrait), or the Sunday job writes
// one first. "Not quite" works on it as on any portrait, and a forget sets
// it aside as it would any other: no draft is written over one set aside.
// "No more tips" (or "stop telling me these") turns the drafts off with the
// rest of the first week.

const (
	// keyPortraitDraft is when the portrait that is a first draft was
	// written, as portraitStamp: a portrait written since isn't one.
	keyPortraitDraft = "portrait.draft"
	// draftMinFacts is how many things the owner has said before there is
	// enough for a first draft.
	draftMinFacts = 5
	// draftEvery is how often a first draft may be rewritten.
	draftEvery = 24 * time.Hour
	// draftFacts caps how many of the owner's facts a draft is built from:
	// the newest.
	draftFacts = 40
	// firstWeek is how long first drafts are written for.
	firstWeek = 7 * 24 * time.Hour
)

// draftState is the twin's first drafts under way.
type draftState struct {
	// busy keeps one first draft at a time, and wg counts the ones under
	// way, so a test can wait for them.
	busy atomic.Bool
	wg   sync.WaitGroup
	// done is set once no first draft will be written again (the first
	// week is over, or a regular portrait has taken over), so a fact kept
	// later doesn't start one.
	done atomic.Bool
	// mu is held while a draft is kept and while a forget sets the
	// portrait aside, and forgets counts the forgets, so a draft the model
	// wrote from a fact forgotten meanwhile is never kept.
	mu      sync.Mutex
	forgets int64
}

// forgot notes a forget, holding off any draft being kept until the
// returned func runs (setPortraitAside defers it).
func (s *draftState) forgot() func() {
	s.mu.Lock()
	s.forgets++
	return s.mu.Unlock
}

// draftOnRemembered hears each fact the twin keeps, and writes a first draft
// in the background when the fact is one the owner said and isn't private.
func (d *Daemon) draftOnRemembered(f memory.Fact) {
	if d.draft.done.Load() || memskill.Sensitive(f.Subject) || !d.ownerStated(f.Source) {
		return
	}
	ctx := d.base(context.Background())
	d.draft.wg.Add(1)
	go func() {
		defer d.draft.wg.Done()
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		d.draftPortrait(ctx)
	}()
}

// ownerStated reports whether a fact kept in the chat source is one the owner
// told the twin: in a chat on this Mac, their own chat in a messaging app,
// or on the memory page. A routine's, a task's or a watcher's run (a
// scratch chat) and another person's chat don't count, nor an import.
func (d *Daemon) ownerStated(source string) bool {
	if source == "" || memory.IsScratch(source) {
		return false
	}
	if notedChats[source] || source == "memory page" {
		return true
	}
	d.chmu.RLock()
	defer d.chmu.RUnlock()
	for name, ch := range d.channels {
		if id := ch.OwnerChatID(); id != "" && name+":"+id == source {
			return true
		}
	}
	return false
}

// portraitIsDraft reports whether p is a first draft.
func (d *Daemon) portraitIsDraft(ctx context.Context, p memory.Portrait) bool {
	if p.Text == "" {
		return false
	}
	v, _ := d.store.Get(ctx, keyPortraitDraft)
	return v != "" && v == portraitStamp(p)
}

// draftPortrait writes or rewrites the first draft when it is due, and
// reports whether it did.
func (d *Daemon) draftPortrait(ctx context.Context) bool {
	if !d.draft.busy.CompareAndSwap(false, true) {
		return false
	}
	defer d.draft.busy.Store(false)
	if off, _ := d.store.Get(ctx, "nudges_off"); off == "1" {
		return false
	}
	installed, _ := d.store.Get(ctx, "installed_at")
	t0, err := time.Parse(time.RFC3339, installed)
	if err != nil {
		return false
	}
	if clock().Sub(t0) >= firstWeek {
		d.draft.done.Store(true)
		return false
	}
	p, err := d.store.GetPortrait(ctx)
	if err != nil {
		return false
	}
	if p.Text != "" && !d.portraitIsDraft(ctx, p) {
		d.draft.done.Store(true) // a regular portrait has taken over
		return false
	}
	if p.Text != "" && clock().Sub(p.UpdatedAt) < draftEvery {
		return false // today's draft is written
	}
	if portraitAside(ctx, d.store) {
		return false
	}
	// Noted before the facts are read: a forget from here on means the
	// draft may say something the owner asked the twin to forget.
	d.draft.mu.Lock()
	forgets := d.draft.forgets
	d.draft.mu.Unlock()
	facts, err := d.ownerFacts(ctx)
	if err != nil || len(facts) < draftMinFacts {
		return false
	}
	if blocked, err := d.backgroundOverBudget(ctx); err != nil || blocked {
		return false
	}
	chatKey := scratchKey(d.proactiveChatKey(), "portrait")
	text, err := d.agent.WriteDraftPortrait(ctx, chatKey, facts)
	if err != nil {
		d.log.Warn("portrait: no first draft", "err", err)
		return false
	}
	if !d.keepDraft(ctx, p, forgets, text) {
		d.log.Info("portrait: a first draft wasn't kept, memory changed while it was written")
		return false
	}
	d.store.Audit(ctx, "memory.portrait.draft", chatKey, firstRunes(text, 200))
	// Screens open now look again, so the panel shows it at once. The
	// event says nothing itself, and a wall screen never hears it.
	d.bus.Flash(events.Event{Kind: "portrait"})
	return true
}

// keepDraft stores text as the first draft, unless, while the model wrote
// it, the owner forgot something (it may be built from that fact), the
// portrait was set aside, or another portrait was written (Refresh, the
// Sunday job or the end of week one): p is the portrait the draft replaces.
// It holds the lock a forget takes, so a forget lands before or after.
func (d *Daemon) keepDraft(ctx context.Context, p memory.Portrait, forgets int64, text string) bool {
	d.draft.mu.Lock()
	defer d.draft.mu.Unlock()
	if d.draft.forgets != forgets || portraitAside(ctx, d.store) {
		return false
	}
	now, err := d.store.GetPortrait(ctx)
	if err != nil || now.Text != p.Text || portraitStamp(now) != portraitStamp(p) {
		return false
	}
	if err := d.store.SetPortrait(ctx, text); err != nil {
		return false
	}
	np, err := d.store.GetPortrait(ctx)
	if err != nil || np.Text == "" {
		return false
	}
	if p.Text != "" {
		_ = d.store.Set(ctx, keyPortraitPrev, p.Text)
	}
	_ = d.store.Unset(ctx, keyPortraitNew) // a draft has nothing new to say
	_ = d.store.Set(ctx, keyPortraitDraft, portraitStamp(np))
	return true
}

// firstRunes is s cut to n runes.
func firstRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ownerFacts are the things the owner said that a first draft may use, as
// "subject: fact", newest last.
func (d *Daemon) ownerFacts(ctx context.Context) ([]string, error) {
	all, err := d.store.AllFacts(ctx, 1000)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range all {
		if memskill.Sensitive(f.Subject) || !d.ownerStated(f.Source) {
			continue
		}
		out = append(out, f.Subject+": "+f.Content)
	}
	if len(out) > draftFacts {
		out = out[len(out)-draftFacts:]
	}
	return out, nil
}
