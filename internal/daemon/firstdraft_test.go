package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

const (
	draftOne   = "You're new here, you like quiet mornings and you cycle to work in all weathers."
	draftTwo   = "You like quiet mornings, cycle to work and you've just taken up the cello."
	weeklyText = "You keep Friday afternoons free, and you'd rather be asked than guessed about."
)

// draftBrain writes first drafts (the first-draft task), regular portraits
// (the scheduled one) and says back anything else. It keeps every
// first-draft request it was sent.
type draftBrain struct {
	mu    sync.Mutex
	draft string
	reqs  []llm.Request
	// When started is set, the next first draft says so on it and then
	// waits for release before it answers.
	started, release chan struct{}
}

func (b *draftBrain) brain(last string, req llm.Request) llm.Response {
	if req.System == agent.DraftPortraitSystem {
		b.mu.Lock()
		b.reqs = append(b.reqs, req)
		draft, started, release := b.draft, b.started, b.release
		b.started, b.release = nil, nil
		b.mu.Unlock()
		if started != nil {
			close(started)
			<-release
		}
		return say(draft)
	}
	if strings.Contains(last, "refresh your portrait") {
		return say(weeklyText + "\nNEW: NONE")
	}
	return echo(last, req)
}

func (b *draftBrain) set(text string) {
	b.mu.Lock()
	b.draft = text
	b.mu.Unlock()
}

// hold makes the next first draft wait: it returns a channel closed once
// the model is writing it, and a func that lets it answer.
func (b *draftBrain) hold() (started <-chan struct{}, release func()) {
	s, r := make(chan struct{}), make(chan struct{})
	b.mu.Lock()
	b.started, b.release = s, r
	b.mu.Unlock()
	return s, func() { close(r) }
}

func (b *draftBrain) asked() []llm.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]llm.Request(nil), b.reqs...)
}

// newDraftDaemon is a twin on its first day with a first-draft writer.
func newDraftDaemon(t *testing.T) (*testDaemon, *draftBrain) {
	t.Helper()
	b := &draftBrain{draft: draftOne}
	td := newTestDaemon(t, b.brain)
	_ = td.store.Set(context.Background(), "installed_at", time.Now().Add(-time.Hour).Format(time.RFC3339))
	return td, b
}

// tell keeps facts as the twin would, and waits for any draft they set off.
func tell(t *testing.T, td *testDaemon, source string, facts ...string) {
	t.Helper()
	for _, f := range facts {
		subject, content, _ := strings.Cut(f, ": ")
		if _, err := td.store.Remember(context.Background(), subject, content, source); err != nil {
			t.Fatal(err)
		}
	}
	td.draft.wg.Wait()
}

// Five things the owner said, and the screen shows a first draft built
// from those alone: nothing private, nothing a routine found in the inbox,
// nothing another person said, and none of the rest of memory.
func TestAFirstDraftAfterFiveThingsTheOwnerSaid(t *testing.T) {
	td, b := newDraftDaemon(t)
	ctx := context.Background()
	tell(t, td, "screen:local", "user: likes quiet mornings", "work: cycles to work", "user: is called Sam")
	tell(t, td, "telegram:owner#protocol-20261007-080000", "work: has a dentist invoice from Brightsmile in the inbox")
	tell(t, td, "telegram:stranger", "user: owes Bob a favour")
	tell(t, td, "imported", "user: an imported line")
	tell(t, td, "voice:local", "health: has a bad back")
	tell(t, td, ownerKey, "hobbies: has taken up the cello")
	if p, _ := td.store.GetPortrait(ctx); p.Text != "" || len(b.asked()) != 0 {
		t.Fatalf("a draft from four things: %q", p.Text)
	}

	tell(t, td, "memory page", "user: lives in Leeds")
	p, _ := td.store.GetPortrait(ctx)
	if p.Text != draftOne {
		t.Fatalf("after five things the portrait is %q", p.Text)
	}
	sd := td.screenData(ctx)
	if sd.Portrait != draftOne || !sd.PortraitDraft || sd.PortraitNew != "" {
		t.Fatalf("the screen gets %q, draft %v, new %q", sd.Portrait, sd.PortraitDraft, sd.PortraitNew)
	}
	reqs := b.asked()
	if len(reqs) != 1 {
		t.Fatalf("%d drafts asked for", len(reqs))
	}
	req := reqs[0]
	if len(req.Tools) != 0 || req.SystemVolatile != "" || len(req.Messages) != 1 {
		t.Fatalf("the draft saw more than the facts: %d tools, volatile %q, %d messages", len(req.Tools), req.SystemVolatile, len(req.Messages))
	}
	got := req.Messages[0].PlainText()
	for _, want := range []string{"quiet mornings", "cycles to work", "Sam", "cello", "Leeds"} {
		if !strings.Contains(got, want) {
			t.Errorf("the draft wasn't told %q: %q", want, got)
		}
	}
	for _, not := range []string{"Brightsmile", "Bob", "imported", "back"} {
		if strings.Contains(got, not) {
			t.Errorf("the draft was told %q: %q", not, got)
		}
	}
}

// The draft is rewritten as the owner says more, but at most once a day.
func TestTheFirstDraftIsRewrittenAtMostOnceADay(t *testing.T) {
	td, b := newDraftDaemon(t)
	ctx := context.Background()
	tell(t, td, "screen:local", "user: a", "user: b", "user: c", "user: d", "user: e")
	if p, _ := td.store.GetPortrait(ctx); p.Text != draftOne {
		t.Fatalf("no first draft: %q", p.Text)
	}
	b.set(draftTwo)
	tell(t, td, "screen:local", "user: f")
	if p, _ := td.store.GetPortrait(ctx); p.Text != draftOne || len(b.asked()) != 1 {
		t.Fatalf("rewritten within the day: %q, %d asks", p.Text, len(b.asked()))
	}
	withClock(t, 25*time.Hour)
	if !td.draftPortrait(ctx) {
		t.Fatal("not rewritten the next day")
	}
	sd := td.screenData(ctx)
	if sd.Portrait != draftTwo || !sd.PortraitDraft {
		t.Fatalf("the next day the screen shows %q, draft %v", sd.Portrait, sd.PortraitDraft)
	}
	if prev, _ := td.store.Get(ctx, keyPortraitPrev); prev != draftOne {
		t.Fatalf("portrait.prev is %q", prev)
	}
}

// The regular portrait takes over: the end of week one rewrites a draft
// (where it would leave a real portrait be), and no draft is written over
// a real one.
func TestTheRegularPortraitTakesOverFromTheDraft(t *testing.T) {
	td, _ := newDraftDaemon(t)
	ctx := context.Background()
	tell(t, td, "screen:local", "user: a", "user: b", "user: c", "user: d", "user: e")
	if !td.weekOnePortrait(ctx) {
		t.Fatal("the end of week one has no portrait to point to")
	}
	sd := td.screenData(ctx)
	if sd.Portrait != weeklyText || sd.PortraitDraft {
		t.Fatalf("after week one the screen shows %q, draft %v", sd.Portrait, sd.PortraitDraft)
	}
	withClock(t, 25*time.Hour)
	if td.draftPortrait(ctx) {
		t.Fatal("a draft was written over the regular portrait")
	}
	if !td.draft.done.Load() {
		t.Fatal("a fact kept from now on would still start a draft")
	}
	if p, _ := td.store.GetPortrait(ctx); p.Text != weeklyText {
		t.Fatalf("the portrait is %q", p.Text)
	}
}

// No draft after the first week, after "No more tips", or over a portrait
// set aside after a forget; and a forget sets a draft aside as it would any
// portrait.
func TestNoFirstDraftWhenItShouldStayQuiet(t *testing.T) {
	ctx := context.Background()
	t.Run("after the first week", func(t *testing.T) {
		td, _ := newDraftDaemon(t)
		_ = td.store.Set(ctx, "installed_at", time.Now().Add(-8*24*time.Hour).Format(time.RFC3339))
		tell(t, td, "screen:local", "user: a", "user: b", "user: c", "user: d", "user: e")
		if p, _ := td.store.GetPortrait(ctx); p.Text != "" {
			t.Fatalf("a draft in week two: %q", p.Text)
		}
	})
	t.Run("no more tips", func(t *testing.T) {
		td, _ := newDraftDaemon(t)
		if err := td.StopTips(ctx); err != nil {
			t.Fatal(err)
		}
		tell(t, td, "screen:local", "user: a", "user: b", "user: c", "user: d", "user: e")
		if p, _ := td.store.GetPortrait(ctx); p.Text != "" {
			t.Fatalf("a draft after No more tips: %q", p.Text)
		}
	})
	t.Run("a forget", func(t *testing.T) {
		td, _ := newDraftDaemon(t)
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		td.startBackup(rctx) // registers what hears a forget
		tell(t, td, "screen:local", "user: a", "user: b", "user: c", "user: d", "user: e")
		if !td.screenData(ctx).PortraitDraft {
			t.Fatal("no first draft")
		}
		id, err := td.store.Remember(ctx, "work", "the user is job hunting", "screen:local")
		if err != nil {
			t.Fatal(err)
		}
		td.draft.wg.Wait()
		if _, err := td.store.ForgetFact(ctx, id); err != nil {
			t.Fatal(err)
		}
		if sd := td.screenData(ctx); sd.Portrait != "" {
			t.Fatalf("the draft outlived the forget: %q", sd.Portrait)
		}
		if v, _ := td.store.Get(ctx, memory.KeyPortraitAside); v == "" {
			t.Fatal("the draft wasn't set aside")
		}
		withClock(t, 25*time.Hour)
		tell(t, td, "screen:local", "user: f")
		if p, _ := td.store.GetPortrait(ctx); p.Text != "" {
			t.Fatalf("a draft was written over one set aside: %q", p.Text)
		}
	})
}

// "Not quite" works on a draft as on any portrait, so the screen asks.
func TestTheScreenAsksAboutAFirstDraft(t *testing.T) {
	td, _ := newDraftDaemon(t)
	ctx := context.Background()
	tell(t, td, "screen:local", "user: a", "user: b", "user: c", "user: d", "user: e")
	sd := td.screenData(ctx)
	if !sd.PortraitDraft || sd.PortraitAck || sd.PortraitAt == "" {
		t.Fatalf("the screen gets draft %v, ack %v, at %q", sd.PortraitDraft, sd.PortraitAck, sd.PortraitAt)
	}
}

// The model takes a while to write a draft, and the fact that set it off
// is the one the screen offers to undo. A forget while it writes means the
// draft may say the forgotten thing, so it isn't kept; nor is one that
// would replace a portrait written meanwhile.
func TestAFirstDraftWrittenWhileMemoryChangesIsNotKept(t *testing.T) {
	ctx := context.Background()
	remember := func(t *testing.T, td *testDaemon, fact string) int64 {
		t.Helper()
		subject, content, _ := strings.Cut(fact, ": ")
		id, err := td.store.Remember(ctx, subject, content, "screen:local")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	t.Run("the fifth thing undone", func(t *testing.T) {
		td, b := newDraftDaemon(t)
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		td.startBackup(rctx) // registers what hears a forget
		tell(t, td, "screen:local", "user: a", "user: b", "user: c", "user: d")
		started, release := b.hold()
		id := remember(t, td, "work: is quietly job hunting")
		<-started
		if err := td.UndoFact(ctx, id); err != nil {
			t.Fatal(err)
		}
		release()
		td.draft.wg.Wait()
		if p, _ := td.store.GetPortrait(ctx); p.Text != "" {
			t.Fatalf("a draft from a forgotten fact was kept: %q", p.Text)
		}
		if v, _ := td.store.Get(ctx, keyPortraitDraft); v != "" {
			t.Fatalf("portrait.draft is %q", v)
		}
	})
	t.Run("a forget over a draft", func(t *testing.T) {
		td, b := newDraftDaemon(t)
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		td.startBackup(rctx)
		tell(t, td, "screen:local", "user: a", "user: b", "user: c", "user: d", "user: e")
		if !td.screenData(ctx).PortraitDraft {
			t.Fatal("no first draft")
		}
		withClock(t, 25*time.Hour)
		b.set(draftTwo)
		started, release := b.hold()
		id := remember(t, td, "work: is quietly job hunting")
		<-started
		if _, err := td.store.ForgetFact(ctx, id); err != nil {
			t.Fatal(err)
		}
		release()
		td.draft.wg.Wait()
		if p, _ := td.store.GetPortrait(ctx); p.Text != "" {
			t.Fatalf("a draft from a forgotten fact was kept: %q", p.Text)
		}
		if !portraitAside(ctx, td.store) {
			t.Fatal("the portrait is no longer set aside")
		}
	})
	t.Run("a refresh meanwhile", func(t *testing.T) {
		td, b := newDraftDaemon(t)
		tell(t, td, "screen:local", "user: a", "user: b", "user: c", "user: d")
		started, release := b.hold()
		remember(t, td, "user: e")
		<-started
		if _, err := td.writePortrait(ctx, ownerKey); err != nil {
			t.Fatal(err)
		}
		release()
		td.draft.wg.Wait()
		sd := td.screenData(ctx)
		if sd.Portrait != weeklyText || sd.PortraitDraft {
			t.Fatalf("after a refresh the screen shows %q, draft %v", sd.Portrait, sd.PortraitDraft)
		}
	})
}
