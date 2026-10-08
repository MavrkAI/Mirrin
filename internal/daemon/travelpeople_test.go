package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/heartbeat"
)

const travelNotice = "You're in Lisbon now; reminders and routines follow local time. You told me Dan moved here. Want a nudge tomorrow to see if Dan's free for coffee?"

// offerTravel sends the travel notice to the owner, as the heartbeat does,
// with its offer kept made at `at`.
func offerTravel(t *testing.T, td *testDaemon, at time.Time) {
	t.Helper()
	ctx := context.Background()
	b, _ := json.Marshal(heartbeat.TravelOffer{Ask: "You told me Dan moved here. Want a nudge tomorrow to see if Dan's free for coffee?",
		Remind: "See if Dan's free for coffee while you're in Lisbon", Who: "Dan", Zone: "Europe/Lisbon", At: at})
	if err := td.store.Set(ctx, heartbeat.TravelOfferKey, string(b)); err != nil {
		t.Fatal(err)
	}
	if err := td.Notify(ctx, ownerKey, travelNotice); err != nil {
		t.Fatal(err)
	}
}

// The travel welcome's offer waits out the owner's quiet hours: the
// heartbeat asks the daemon whether it's quiet now.
func TestTravelOfferKnowsTheOwnersQuietHours(t *testing.T) {
	td := newTestDaemon(t, butler)
	withQuietHours(td)
	if td.beat.Quiet == nil {
		t.Fatal("the heartbeat can't tell quiet hours")
	}
	if !td.beat.Quiet(at(t, td.Daemon, 1, 30)) {
		t.Fatal("01:30 is not quiet")
	}
	if td.beat.Quiet(at(t, td.Daemon, 12, 0)) {
		t.Fatal("noon is quiet")
	}
}

func TestYesToTheTravelOfferSetsAReminderAndMessagesNobody(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, butler)
	offerTravel(t, td, clock())
	got := td.owner(t, "Yes please!")
	if !strings.HasPrefix(got, "Done. I'll nudge you at 10:00 am") || !strings.HasSuffix(got, " I won't message Dan myself.") {
		t.Fatalf("reply %q", got)
	}
	if heard := td.llm.heard(); len(heard) != 0 {
		t.Fatalf("the model was asked: %q", heard)
	}
	rs, _ := td.store.PendingReminders(ctx, ownerKey)
	if len(rs) != 1 || rs[0].Text != "See if Dan's free for coffee while you're in Lisbon" {
		t.Fatalf("reminders %+v", rs)
	}
	if h := rs[0].DueAt.In(td.location()); h.Hour() != 10 || h.Minute() != 0 || !rs[0].DueAt.After(clock()) {
		t.Fatalf("due %v", rs[0].DueAt)
	}
	if len(td.ran()) != 0 {
		t.Fatalf("someone was messaged: %q", td.ran())
	}
	// Answered: a second yes is conversation.
	if got := td.owner(t, "yes"); got != "Heard: yes" {
		t.Fatalf("second yes: %q", got)
	}
}

func TestNoAndStopToTheTravelOffer(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, butler)
	offerTravel(t, td, clock())
	if got := td.owner(t, "no thanks"); got != "No problem. Enjoy the trip." {
		t.Fatalf("no: %q", got)
	}
	if rs, _ := td.store.PendingReminders(ctx, ownerKey); len(rs) != 0 {
		t.Fatalf("a no set a reminder: %+v", rs)
	}
	offerTravel(t, td, clock())
	if got := td.owner(t, "Stop telling me these."); !strings.HasPrefix(got, "Understood. I won't bring up") {
		t.Fatalf("stop: %q", got)
	}
	if off, _ := td.store.Get(ctx, heartbeat.TravelPeopleOffKey); off != "1" {
		t.Fatal("not turned off")
	}
}

func TestYesOnlyAnswersTheTravelOfferWhileItIsTheLastWord(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, butler)
	offerTravel(t, td, clock())
	_ = td.Notify(ctx, ownerKey, "Shall I book the table?")
	if got := td.owner(t, "yes"); got != "Heard: yes" {
		t.Fatalf("a yes to something else: %q", got)
	}
	td2 := newTestDaemon(t, butler)
	offerTravel(t, td2, clock().Add(-25*time.Hour))
	if got := td2.owner(t, "yes"); got != "Heard: yes" {
		t.Fatalf("a day-old offer: %q", got)
	}
	if rs, _ := td2.store.PendingReminders(ctx, ownerKey); len(rs) != 0 {
		t.Fatalf("reminder set: %+v", rs)
	}
}

func TestTravelOffersCanBeTurnedBackOn(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, butler)
	// While they're on, the words are just conversation.
	if got := td.owner(t, "start telling me these again"); got != "Heard: start telling me these again" {
		t.Fatalf("on already: %q", got)
	}
	offerTravel(t, td, clock())
	if got := td.owner(t, "stop telling me these"); !strings.Contains(got, `Say "start telling me these again"`) {
		t.Fatalf("stop: %q", got)
	}
	if got := td.owner(t, "Start telling me these again."); !strings.HasPrefix(got, "Happy to.") {
		t.Fatalf("resume: %q", got)
	}
	if off, _ := td.store.Get(ctx, heartbeat.TravelPeopleOffKey); off != "" {
		t.Fatalf("still off: %q", off)
	}
}

func TestABareOkIsNoYesToTheTravelOffer(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, butler)
	offerTravel(t, td, clock())
	if got := td.owner(t, "ok"); got != "Heard: ok" {
		t.Fatalf("ok: %q", got)
	}
	if rs, _ := td.store.PendingReminders(ctx, ownerKey); len(rs) != 0 {
		t.Fatalf("an ok set a reminder: %+v", rs)
	}
}

func TestNudgeAtIsTomorrowMorningUnlessItIsTheSmallHours(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Lisbon")
	if got := nudgeAt(time.Date(2030, 10, 1, 18, 0, 0, 0, loc), loc); !got.Equal(time.Date(2030, 10, 2, 10, 0, 0, 0, loc)) {
		t.Fatalf("evening: %v", got)
	}
	if got := nudgeAt(time.Date(2030, 10, 1, 2, 0, 0, 0, loc), loc); !got.Equal(time.Date(2030, 10, 1, 10, 0, 0, 0, loc)) {
		t.Fatalf("small hours: %v", got)
	}
}
