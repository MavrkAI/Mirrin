package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// "Forget that I'm job hunting": the fact goes, and so does the portrait
// that said so in other words. It's off the screen and out of the next turn,
// and the memory page can say why until a fresh portrait is written.
func TestForgettingAFactSetsThePortraitAside(t *testing.T) {
	const fresh = "You like quiet mornings, long walks and plain answers without fuss."
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say(fresh) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	td.startBackup(ctx) // registers what hears a forget
	if err := td.store.SetPortrait(ctx, "You're quietly looking for a new role and keep it to yourself."); err != nil {
		t.Fatal(err)
	}
	id, err := td.store.Remember(ctx, "work", "the user is job hunting", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := td.store.ForgetFact(ctx, id); err != nil {
		t.Fatal(err)
	}
	if p, err := td.store.GetPortrait(ctx); err != nil || p.Text != "" {
		t.Fatalf("the portrait outlived the forget: %+v (%v)", p, err)
	}
	if sd := td.screenData(ctx); sd.Portrait != "" {
		t.Fatalf("the screen still shows the portrait: %q", sd.Portrait)
	}
	v, _ := td.store.Get(ctx, memory.KeyPortraitAside)
	if at, err := time.Parse(time.RFC3339, v); err != nil || time.Since(at) > time.Minute {
		t.Fatalf("portrait.aside is %q", v)
	}
	mem := memoryAdapter{td.store, td.Daemon}
	if text, _, aside, err := mem.Portrait(ctx); err != nil || text != "" || !aside {
		t.Fatalf("the memory page gets %q, aside %v (%v)", text, aside, err)
	}

	// Refresh (or the Sunday job) writes a new one, and it's no longer aside.
	if _, err := mem.RefreshPortrait(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := td.store.Get(ctx, memory.KeyPortraitAside); v != "" {
		t.Fatalf("a fresh portrait left portrait.aside at %q", v)
	}
	if text, _, aside, err := mem.Portrait(ctx); err != nil || text != fresh || aside {
		t.Fatalf("after a refresh: %q, aside %v (%v)", text, aside, err)
	}
}

// With no portrait yet, a forget sets nothing aside, so the memory page
// never says one was.
func TestForgettingWithNoPortraitSetsNothingAside(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	td.startBackup(ctx)
	id, err := td.store.Remember(ctx, "work", "the user is job hunting", "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := td.store.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}
	if v, _ := td.store.Get(ctx, memory.KeyPortraitAside); v != "" {
		t.Fatalf("portrait.aside is %q with no portrait to set aside", v)
	}
	if _, _, aside, _ := (memoryAdapter{td.store, td.Daemon}).Portrait(ctx); aside {
		t.Fatal("the memory page would say a portrait was set aside")
	}
}

// The portrait task tells the model to leave out what was forgotten, which
// an old conversation of the owner's own words may still mention.
func TestPortraitPromptLeavesOutWhatWasForgotten(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response {
		return say("You like quiet mornings, long walks and plain answers without fuss.")
	})
	if _, _, err := td.agent.WritePortrait(context.Background(), scratchKey(ownerKey, "portrait")); err != nil {
		t.Fatal(err)
	}
	heard := td.llm.heard()
	if len(heard) == 0 || !strings.Contains(heard[0], "If something isn't in memory any more, leave it out") {
		t.Fatalf("the portrait task reads %q", heard)
	}
}

// portraitBrain writes portraits with the given NEW line, and says back
// any other task (as echo does).
func portraitBrain(text, news string) func(string, llm.Request) llm.Response {
	return func(last string, req llm.Request) llm.Response {
		if strings.Contains(last, "refresh your portrait") {
			return say(text + "\nNEW: " + news)
		}
		return echo(last, req)
	}
}

// A new portrait keeps the one before it, and its line on what's new comes
// off the portrait and goes on the screen beside it. A portrait with
// nothing new (NONE) keeps nothing new, not the last one's line.
func TestANewPortraitKeepsTheOldOneAndWhatsNew(t *testing.T) {
	const (
		before = "You're settling into a new job and like your mornings quiet."
		now    = "You like quiet mornings, long walks and plain answers without fuss."
		next   = "You keep Friday afternoons free, and you'd rather be asked than guessed about."
	)
	ctx := context.Background()
	td := newTestDaemon(t, portraitBrain(now, "you've been guarding Friday afternoons."))
	if err := td.store.SetPortrait(ctx, before); err != nil {
		t.Fatal(err)
	}
	text, err := td.writePortrait(ctx, scratchKey(ownerKey, "portrait"))
	if err != nil || text != now {
		t.Fatalf("wrote %q (%v)", text, err)
	}
	if p, _ := td.store.GetPortrait(ctx); p.Text != now {
		t.Fatalf("the stored portrait is %q", p.Text)
	}
	if prev, _ := td.store.Get(ctx, keyPortraitPrev); prev != before {
		t.Fatalf("portrait.prev is %q", prev)
	}
	sd := td.screenData(ctx)
	if sd.Portrait != now || sd.PortraitNew != "you've been guarding Friday afternoons." || sd.PortraitAt == "" || sd.PortraitAck {
		t.Fatalf("the screen gets %q, new %q, at %q, ack %v", sd.Portrait, sd.PortraitNew, sd.PortraitAt, sd.PortraitAck)
	}

	// "That's you" holds for this portrait, and the next one asks again.
	if err := td.AckPortrait(ctx); err != nil {
		t.Fatal(err)
	}
	if !td.screenData(ctx).PortraitAck {
		t.Fatal("That's you wasn't kept")
	}
	td.llm.mu.Lock()
	td.llm.brain = portraitBrain(next, "NONE")
	td.llm.mu.Unlock()
	if _, err := td.writePortrait(ctx, scratchKey(ownerKey, "portrait")); err != nil {
		t.Fatal(err)
	}
	if raw, _ := td.store.Get(ctx, keyPortraitNew); raw != "" {
		t.Fatalf("NONE kept %q", raw)
	}
	if prev, _ := td.store.Get(ctx, keyPortraitPrev); prev != now {
		t.Fatalf("portrait.prev is %q", prev)
	}
	if sd := td.screenData(ctx); sd.Portrait != next || sd.PortraitNew != "" || sd.PortraitAck {
		t.Fatalf("after NONE the screen gets %q, new %q, ack %v", sd.Portrait, sd.PortraitNew, sd.PortraitAck)
	}
}

// What's new is shown for a week after the portrait was written, and only
// with the portrait it came with.
func TestWhatsNewIsShownForAWeek(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	put := func(age time.Duration) {
		at := time.Now().Add(-age)
		p, _ := json.Marshal(memory.Portrait{Text: "You like quiet mornings and plain answers.", UpdatedAt: at})
		n, _ := json.Marshal(portraitNews{Text: "you've been guarding Friday afternoons.", At: at})
		_ = td.store.Set(ctx, "portrait", string(p))
		_ = td.store.Set(ctx, keyPortraitNew, string(n))
	}
	put(6 * 24 * time.Hour)
	if sd := td.screenData(ctx); sd.PortraitNew == "" {
		t.Fatal("six days on, what's new is gone")
	}
	put(8 * 24 * time.Hour)
	if sd := td.screenData(ctx); sd.Portrait == "" || sd.PortraitNew != "" {
		t.Fatalf("eight days on: %q, new %q", sd.Portrait, sd.PortraitNew)
	}
	// A portrait put in some other way (an import) doesn't take the last
	// one's line.
	put(time.Hour)
	if err := td.store.SetPortrait(ctx, "You're a tea person who reads on the train."); err != nil {
		t.Fatal(err)
	}
	if sd := td.screenData(ctx); sd.PortraitNew != "" {
		t.Fatalf("an imported portrait shows %q as new", sd.PortraitNew)
	}
}

// Someone with only the screen gets a Sunday portrait too, written in the
// screen's conversation.
func TestAScreenOnlyOwnerGetsASundayPortrait(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, portraitBrain("You like quiet mornings, long walks and plain answers without fuss.", "NONE"))
	screenOnly(td)
	td.sundayPortrait(ctx)
	if p, _ := td.store.GetPortrait(ctx); p.Text == "" {
		t.Fatal("no Sunday portrait for a screen-only owner")
	}
	as, err := td.store.RecentAuditOfKind(ctx, "memory.portrait", 1)
	if err != nil || len(as) != 1 || !strings.HasPrefix(as[0].ChatKey, screenChat+"#portrait-") {
		t.Fatalf("written in %+v (%v)", as, err)
	}
}

// The last day of the first week writes the first portrait, and the tip
// says where to see it.
func TestTheFirstWeekEndsWithAPortrait(t *testing.T) {
	ctx := context.Background()
	const portrait = "You like quiet mornings, long walks and plain answers without fuss."
	td := newTestDaemon(t, portraitBrain(portrait, "you've been guarding Friday afternoons."))
	onTourDay(t, td, 7)
	td.nudge(ctx)
	if got := td.ch.next(t); got != weekOneTip {
		t.Fatalf("the day-7 tip reads %q", got)
	}
	if sd := td.screenData(ctx); sd.Portrait != portrait || sd.PortraitNew != "you've been guarding Friday afternoons." {
		t.Fatalf("the screen gets %q, new %q", sd.Portrait, sd.PortraitNew)
	}
	if heard := td.llm.heard(); len(heard) != 1 || !strings.Contains(heard[0], "refresh your portrait") {
		t.Fatalf("the model was asked %q", heard)
	}
}

// With a portrait already written, day 7 points to it without writing
// another. One set aside after a forget isn't rewritten there and then:
// the old last tip goes out instead.
func TestTheFirstWeekKeepsAPortraitItHasAndOneSetAside(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, portraitBrain("You like quiet mornings, long walks and plain answers without fuss.", "NONE"))
	if err := td.store.SetPortrait(ctx, "You keep Sundays for the family and plain answers for everyone."); err != nil {
		t.Fatal(err)
	}
	onTourDay(t, td, 7)
	td.nudge(ctx)
	if got := td.ch.next(t); got != weekOneTip {
		t.Fatalf("the day-7 tip reads %q", got)
	}
	if heard := td.llm.heard(); len(heard) != 0 {
		t.Fatalf("a portrait was written over the one there: %q", heard)
	}

	td.setPortraitAside(ctx)
	onTourDay(t, td, 7)
	td.nudge(ctx)
	if got := td.ch.next(t); !strings.Contains(got, "Day 7") {
		t.Fatalf("with the portrait set aside, day 7 says %q", got)
	}
	if p, _ := td.store.GetPortrait(ctx); p.Text != "" {
		t.Fatalf("a portrait set aside was rewritten on day 7: %q", p.Text)
	}
}
