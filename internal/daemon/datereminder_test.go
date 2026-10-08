package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/memory"
	memskill "github.com/MavrkAI/Mirrin/internal/skills/memory"
)

// birthdayIn is a fact about Mum's birthday days from now, and the "the
// 11th" it is reminded on, the day before.
func birthdayIn(td *testDaemon, days int) (fact, ask string) {
	day := time.Now().In(td.location()).AddDate(0, 0, days)
	before := day.AddDate(0, 0, -1)
	return fmt.Sprintf("Mum's birthday is on %d %s.", day.Day(), day.Month()), fmt.Sprintf("Remind me on the %d%s?", before.Day(), suffix(before.Day()))
}

// remindOf is a "remembered" event's offer, if it makes one.
func remindOf(ev events.Event) string {
	m, _ := ev.Data.(map[string]any)
	s, _ := m["remind"].(string)
	return s
}

func suffix(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return "th"
	case n%10 == 1:
		return "st"
	case n%10 == 2:
		return "nd"
	case n%10 == 3:
		return "rd"
	}
	return "th"
}

// A dated fact noted on the screen asks "Remind me on the 11th?"; one tap
// sets that reminder, once, without a model turn. A fact with no date, a
// private one, or one from elsewhere offers nothing and can't be set.
func TestANotedDateOffersAReminderOnce(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	got := listen(t, td.bus)
	fact, ask := birthdayIn(td, 10)
	id := keep(t, td, "remember", "screen:local", "family", fact)
	plain := keep(t, td, "remember", "screen:local", "preferences", "Akshay doesn't eat meat.")
	evs := noted(got())
	if len(evs) != 2 || remindOf(evs[0]) != ask {
		t.Fatalf("the noted date's offer: %+v, want %q", evs, ask)
	}
	if remindOf(evs[1]) != "" {
		t.Fatalf("a fact with no date offered a reminder: %+v", evs[1])
	}
	if rs, _ := td.store.AllPendingReminders(ctx, 10); len(rs) != 0 {
		t.Fatalf("a reminder was set before the tap: %+v", rs)
	}
	calls := func() int { td.llm.mu.Lock(); defer td.llm.mu.Unlock(); return len(td.llm.seen) }
	turns := calls()
	said, err := td.RemindFact(ctx, id)
	if err != nil || !strings.HasPrefix(said, "I'll remind you on the ") || !strings.HasSuffix(said, ", and every year after.") {
		t.Fatalf("tap: %q %v", said, err)
	}
	if calls() != turns {
		t.Fatal("the tap ran a model turn")
	}
	rs, _ := td.store.PendingReminders(ctx, "screen:local")
	want := strings.TrimSuffix(fact, ".") + " (tomorrow)"
	if len(rs) != 1 || rs[0].Text != want || rs[0].DueAt.In(td.location()).Hour() != 9 {
		t.Fatalf("reminder set: %+v, want %q at 9am", rs, want)
	}
	if said, err := td.RemindFact(ctx, id); err != nil || said != "That reminder is already set." {
		t.Fatalf("second tap: %q %v", said, err)
	}
	if rs, _ := td.store.AllPendingReminders(ctx, 10); len(rs) != 1 {
		t.Fatalf("a second tap set another: %+v", rs)
	}

	private := keep(t, td, "remember_sensitive", "screen:local", "health", "Akshay's scan is on the 20th at 3pm.")
	away := keep(t, td, "remember", "telegram:owner", "family", fact)
	task := keep(t, td, "remember", "screen:local#watch-20261007", "family", fact) // read in mail
	for name, id := range map[string]int64{"undated": plain, "private": private, "telegram": away, "mail": task, "gone": 9999} {
		if _, err := td.RemindFact(ctx, id); !errors.Is(err, api.ErrNoOffer) {
			t.Fatalf("tap on the %s fact: %v", name, err)
		}
	}
	for _, ev := range noted(got())[2:] {
		if remindOf(ev) != "" {
			t.Fatalf("offered for %+v", ev)
		}
	}
	// An offer can be taken up for an hour, not days later.
	late := keep(t, td, "remember", "voice:local", "family", "Priya's anniversary is on the 14th of "+time.Now().AddDate(0, 2, 0).Month().String()+".")
	withClock(t, 61*time.Minute)
	if _, err := td.RemindFact(ctx, late); !errors.Is(err, api.ErrNoOffer) {
		t.Fatalf("tap after an hour: %v", err)
	}
}

// Forgetting the fact, by Undo or by asking, removes its reminder.
func TestForgettingTheFactRemovesItsReminder(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	fact, _ := birthdayIn(td, 10)
	id := keep(t, td, "remember", "screen:local", "family", fact)
	other := keep(t, td, "remember", "screen:local", "family", "Dad's birthday is on 3 "+time.Now().AddDate(0, 3, 0).Month().String()+".")
	if _, err := td.RemindFact(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := td.RemindFact(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := td.UndoFact(ctx, id); err != nil {
		t.Fatal(err)
	}
	rs, _ := td.store.AllPendingReminders(ctx, 10)
	if len(rs) != 1 || !strings.HasPrefix(rs[0].Text, "Dad's") {
		t.Fatalf("after Undo: %+v", rs)
	}
	if links := td.factLinks(ctx); len(links) != 1 || links[0].Fact != other {
		t.Fatalf("links after Undo: %+v", links)
	}
	if _, err := td.store.ForgetFact(ctx, other); err != nil {
		t.Fatal(err)
	}
	if rs, _ := td.store.AllPendingReminders(ctx, 10); len(rs) != 0 {
		t.Fatalf("after forget: %+v", rs)
	}
	if v, _ := td.store.Get(ctx, factRemindersKey); v != "" {
		t.Fatalf("record after forget: %s", v)
	}
}

// As it goes out, a birthday's reminder brings back what else the owner
// told the twin about that person, never anything private or picked up in
// the background, and sets next year's.
func TestABirthdayReminderBringsBackGiftIdeasAndComesRoundAgain(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	fact, _ := birthdayIn(td, 10)
	id := keep(t, td, "remember", "screen:local", "family", fact)
	keep(t, td, "remember", "telegram:owner", "family", "Mum would love a new teapot.")
	keep(t, td, "remember_sensitive", "screen:local", "health", "Mum has a bad knee.")
	keep(t, td, "remember", "screen:local#watch-20261007", "family", "Mum ordered flowers.")
	keep(t, td, "remember", "screen:local", "preferences", "Akshay likes tea.")
	if _, err := td.RemindFact(ctx, id); err != nil {
		t.Fatal(err)
	}
	rs, _ := td.store.PendingReminders(ctx, "screen:local")
	if len(rs) != 1 {
		t.Fatalf("reminders: %+v", rs)
	}
	r := rs[0]
	if got := td.factRelated(ctx, r); got != "\nYou also told me: Mum would love a new teapot." {
		t.Fatalf("brought back %q", got)
	}
	if got := td.factRelated(ctx, memory.Reminder{ID: r.ID + 100}); got != "" {
		t.Fatalf("another reminder brought back %q", got)
	}
	withClock(t, 9*24*time.Hour+time.Hour) // the day before, once it has gone out
	_ = td.store.MarkFired(ctx, r.ID)
	td.factReminderFired(ctx, r)
	next, _ := td.store.PendingReminders(ctx, "screen:local")
	if len(next) != 1 || next[0].Text != r.Text || next[0].DueAt.In(td.location()).Format("01-02 15:04") != r.DueAt.In(td.location()).Format("01-02 15:04") || next[0].DueAt.Year() != r.DueAt.Year()+1 {
		t.Fatalf("next year's: %+v after %+v", next, r)
	}
}

// A one-off's reminder doesn't come round again.
func TestAOneOffReminderDoesNotComeRoundAgain(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	lunch := keep(t, td, "remember", "screen:local", "plans", "Lunch with Sam on 3 "+time.Now().AddDate(0, 2, 0).Month().String()+".")
	if _, err := td.RemindFact(ctx, lunch); err != nil {
		t.Fatal(err)
	}
	rs, _ := td.store.PendingReminders(ctx, "screen:local")
	if len(rs) != 1 || rs[0].DueAt.In(td.location()).Hour() != 8 {
		t.Fatalf("lunch: %+v", rs)
	}
	_ = td.store.MarkFired(ctx, rs[0].ID)
	td.factReminderFired(ctx, rs[0])
	if after, _ := td.store.PendingReminders(ctx, "screen:local"); len(after) != 0 {
		t.Fatalf("a one-off came round again: %+v", after)
	}
}

// aboutWhom reads who a dated fact is about, never the owner themselves.
func TestAboutWhom(t *testing.T) {
	for fact, want := range map[string]string{
		"Mum's birthday is on the 12th.":      "mum",
		"My sister Priya's birthday is 3 May": "sister priya",
		"Akshay's birthday is 3 May":          "",
		"Our anniversary is the 14th of June": "",
		"Dentist on the 20th at 3pm":          "dentist",
	} {
		if got := strings.Join(aboutWhom(fact, "akshay"), " "); got != want {
			t.Errorf("%q: %q, want %q", fact, got, want)
		}
	}
}

// A dated fact that is private by what it says, filed under an ordinary
// subject, offers no reminder: a reminder is read out loud and shown on
// the wall screen.
func TestAPrivateDateUnderAnOrdinarySubjectOffersNoReminder(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	got := listen(t, td.bus)
	month := time.Now().AddDate(0, 2, 0).Month().String()
	for _, fact := range []string{
		"My biopsy results appointment is on 12 " + month + ".",
		"Mum's chemo starts on 3 " + month + ".",
		"Hospital visit on 9 " + month + ".",
	} {
		id := keep(t, td, "remember", "screen:local", "appointments", fact)
		if _, err := td.RemindFact(ctx, id); !errors.Is(err, api.ErrNoOffer) {
			t.Fatalf("tap on %q: %v", fact, err)
		}
	}
	for _, ev := range noted(got()) {
		if remindOf(ev) != "" {
			t.Fatalf("offered for %+v", ev)
		}
	}
}

// A reminder brings back only what the owner said, never a private fact
// kept under an ordinary subject, nor a saved web page.
func TestABirthdayReminderLeavesOutPrivateAndOthersFacts(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	fact, _ := birthdayIn(td, 10)
	id := keep(t, td, "remember", "screen:local", "family", fact)
	if _, err := td.RemindFact(ctx, id); err != nil {
		t.Fatal(err)
	}
	rs, _ := td.store.PendingReminders(ctx, "screen:local")
	if len(rs) != 1 {
		t.Fatalf("reminders: %+v", rs)
	}
	// Each on its own, so none hides behind another.
	for name, add := range map[string]func() int64{
		"chemo": func() int64 {
			return keep(t, td, "remember", "screen:local", "family", "Mum's chemo starts on the 3rd.")
		},
		"hospital": func() int64 { return keep(t, td, "remember", "screen:local", "mum", "Mum is in hospital on Friday.") },
		"someone else's": func() int64 {
			return keep(t, td, "remember", "telegram:someone-else", "family", "Mum said the party is a surprise.")
		},
		"page": func() int64 {
			pid, _, err := memskill.SavePage(ctx, td.store, "family", memskill.Page{URL: "https://example.com/mum-gifts", Title: "Gifts for Mum"}, "", "screen:local", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			return pid
		},
	} {
		other := add()
		if got := td.factRelated(ctx, rs[0]); got != "" {
			t.Fatalf("the %s fact was brought back: %q", name, got)
		}
		if _, err := td.store.ForgetFact(ctx, other); err != nil {
			t.Fatal(err)
		}
	}
	keep(t, td, "remember", "screen:local", "family", "Mum loves orchids.")
	if got := td.factRelated(ctx, rs[0]); got != "\nYou also told me: Mum loves orchids." {
		t.Fatalf("brought back %q", got)
	}
}

// A page saved from the browser sheet offers no reminder, though its title
// says "birthday" or its link has a date in it.
func TestASavedPageOffersNoReminder(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	got := listen(t, td.bus)
	next := time.Now().AddDate(0, 1, 0).Format(time.DateOnly)
	for _, p := range []memskill.Page{
		{URL: "https://www.bbcgoodfood.com/recipes/birthday-cake", Title: "Birthday cake ideas"},
		{URL: "https://example.com/events/" + next + "-gig", Title: "Gig tickets"},
	} {
		id, _, err := memskill.SavePage(ctx, td.store, "", p, "", "screen:local", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := td.RemindFact(ctx, id); !errors.Is(err, api.ErrNoOffer) {
			t.Fatalf("tap on %q: %v", p.Title, err)
		}
	}
	evs := noted(got())
	if len(evs) != 2 {
		t.Fatalf("noted: %+v", evs)
	}
	for _, ev := range evs {
		if remindOf(ev) != "" {
			t.Fatalf("offered for %+v", ev)
		}
	}
}
