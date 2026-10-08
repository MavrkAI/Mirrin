package heartbeat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// travel moves a twin first seen in Melbourne to zone, with facts kept
// beforehand, and returns what the owner was told.
func travel(t *testing.T, zone string, facts ...[2]string) (*Heartbeat, *outbox, *string) {
	t.Helper()
	ctx := context.Background()
	sys := "Australia/Melbourne"
	h, o := zoned(t, &sys)
	for _, f := range facts {
		if _, err := h.store.Remember(ctx, f[0], f[1], "telegram:1"); err != nil {
			t.Fatal(err)
		}
	}
	h.look(ctx)
	sys = zone
	h.look(ctx)
	return h, o, &sys
}

func lastSaid(t *testing.T, o *outbox) string {
	t.Helper()
	got := o.messages()
	if len(got) == 0 {
		t.Fatal("nothing was said")
	}
	return got[len(got)-1]
}

func storedOffer(t *testing.T, h *Heartbeat) TravelOffer {
	t.Helper()
	raw, _ := h.store.Get(context.Background(), TravelOfferKey)
	var o TravelOffer
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &o); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func TestTravelWelcomeOffersANudgeAboutSomeoneThere(t *testing.T) {
	h, o, _ := travel(t, "Europe/Lisbon", [2]string{"people", "Dan moved to Lisbon last spring."})
	want := "telegram:1 You're in Lisbon now; reminders and routines follow local time. You told me Dan moved here. Want a nudge tomorrow to see if Dan's free for coffee?"
	if got := lastSaid(t, o); got != want {
		t.Fatalf("want\n%q\ngot\n%q", want, got)
	}
	off := storedOffer(t, h)
	if off.Who != "Dan" || off.Remind != "See if Dan's free for coffee while you're in Lisbon" || off.At.IsZero() || !strings.HasSuffix(want, off.Ask) {
		t.Fatalf("offer kept for the yes: %+v", off)
	}
}

func TestTravelWelcomeSaysNothingMoreWhenNothingMatches(t *testing.T) {
	h, o, _ := travel(t, "Europe/Lisbon", [2]string{"people", "Dan moved to Porto."}, [2]string{"work", "Priya used to live in Lisbon."})
	want := "telegram:1 You're in Lisbon now; reminders and routines follow local time."
	if got := lastSaid(t, o); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if off := storedOffer(t, h); off.Ask != "" {
		t.Fatalf("no offer was made, yet one is kept: %+v", off)
	}
}

func TestTravelWelcomeNeverDrawsOnPrivateFacts(t *testing.T) {
	for _, f := range [][2]string{
		{"relationships", "Sam moved to Lisbon."},
		{"health", "Dan lives in Lisbon while he has treatment."},
		{"family", "Ana lives in Lisbon."},
		{"people", "My ex Sam moved to Lisbon."},
		{"people", "Dan lives in Lisbon and owes me money."},
		{"secrets", "Jo works in Lisbon."},
	} {
		t.Run(f[1], func(t *testing.T) {
			h, o, _ := travel(t, "Europe/Lisbon", f)
			if got := lastSaid(t, o); strings.Contains(got, "You told me") || storedOffer(t, h).Ask != "" {
				t.Fatalf("a private fact came up: %q", got)
			}
		})
	}
}

func TestTravelWelcomeIsHedgedWhereTheZoneSpansCities(t *testing.T) {
	_, o, _ := travel(t, "Europe/Berlin", [2]string{"people", "Dan lives in Berlin."})
	if got := lastSaid(t, o); !strings.HasSuffix(got, " You told me Dan lives in Berlin. If you're nearby, want a nudge tomorrow to see if Dan's free for coffee?") {
		t.Fatalf("got %q", got)
	}
	// Berlin's time is Munich's too: someone in Munich is not "here".
	_, o, _ = travel(t, "Europe/Berlin", [2]string{"people", "Priya lives in Munich."})
	if got := lastSaid(t, o); strings.Contains(got, "You told me") {
		t.Fatalf("a city the zone isn't named for came up: %q", got)
	}
}

func TestTravelWelcomeOffersSomethingTheyWantedToDoThere(t *testing.T) {
	_, o, _ := travel(t, "Europe/Lisbon", [2]string{"travel", "User wants to visit the Tile Museum in Lisbon."})
	if got := lastSaid(t, o); !strings.HasSuffix(got, " You told me you'd like to visit the Tile Museum in Lisbon. Want a nudge tomorrow about it?") {
		t.Fatalf("got %q", got)
	}
}

func TestTravelWelcomeOncePerCityPerTrip(t *testing.T) {
	ctx := context.Background()
	h, o, sys := travel(t, "Europe/Lisbon", [2]string{"people", "Dan moved to Lisbon."})
	if !strings.Contains(lastSaid(t, o), "Dan moved here") {
		t.Fatal("no offer on arrival")
	}
	// The same city again (a notice told again for the same move): no offer.
	if offer, _ := h.knownHere(ctx, "Australia/Melbourne", "Europe/Lisbon"); offer != "" {
		t.Fatalf("offered twice in one trip: %q", offer)
	}
	// Home, and nothing about home.
	*sys = "Australia/Melbourne"
	h.look(ctx)
	if got := lastSaid(t, o); strings.Contains(got, "You told me") {
		t.Fatalf("home is no trip: %q", got)
	}
	// A new trip to the same city is a new welcome.
	*sys = "Europe/Lisbon"
	h.look(ctx)
	if got := lastSaid(t, o); !strings.Contains(got, "Dan moved here") || len(o.messages()) != 3 {
		t.Fatalf("a new trip: %q", o.messages())
	}
}

func TestTravelWelcomeOncePerCityAcrossATripThatComesBack(t *testing.T) {
	ctx := context.Background()
	h, o, sys := travel(t, "Europe/Lisbon",
		[2]string{"people", "Dan moved to Lisbon."}, [2]string{"people", "Priya lives in Rome."})
	if !strings.Contains(lastSaid(t, o), "Dan moved here") {
		t.Fatal("no offer on arrival")
	}
	// Lisbon, Rome and back to Lisbon on one trip: Dan is offered once.
	*sys = "Europe/Rome"
	h.look(ctx)
	if got := lastSaid(t, o); !strings.Contains(got, "Priya lives in Rome") {
		t.Fatalf("Rome: %q", got)
	}
	*sys = "Europe/Lisbon"
	h.look(ctx)
	if got := lastSaid(t, o); strings.Contains(got, "You told me") || !strings.Contains(got, "Lisbon") {
		t.Fatalf("back in Lisbon on the same trip: %q", got)
	}
	// A clock flipping across a border offers nothing new either.
	*sys = "Europe/Rome"
	h.look(ctx)
	if got := lastSaid(t, o); strings.Contains(got, "You told me") || !strings.Contains(got, "Rome") {
		t.Fatalf("back in Rome on the same trip: %q", got)
	}
	// Home ends the trip; the next one welcomes afresh.
	*sys = "Australia/Melbourne"
	h.look(ctx)
	*sys = "Europe/Lisbon"
	h.look(ctx)
	if got := lastSaid(t, o); !strings.Contains(got, "Dan moved here") {
		t.Fatalf("a new trip: %q", o.messages())
	}
}

func TestTravelWelcomeLearnsHomeFromWhereTheOwnerLives(t *testing.T) {
	ctx := context.Background()
	// The first move this twin sees is mid-trip, Lisbon to Rome, so Lisbon
	// looks like home. The owner said they live in Melbourne.
	sys := "Europe/Lisbon"
	h, o := zoned(t, &sys)
	_, _ = h.store.Remember(ctx, "user", "User lives in Melbourne.", "telegram:1")
	_, _ = h.store.Remember(ctx, "people", "Dan moved to Lisbon.", "telegram:1")
	h.look(ctx)
	sys = "Europe/Rome"
	h.look(ctx)
	sys = "Australia/Melbourne"
	h.look(ctx)
	if home, _ := h.store.Get(ctx, travelHomeKey); home != "Australia/Melbourne" {
		t.Fatalf("home %q", home)
	}
	// Lisbon is now a trip, and Dan is offered.
	sys = "Europe/Lisbon"
	h.look(ctx)
	if got := lastSaid(t, o); !strings.Contains(got, "Dan moved here") {
		t.Fatalf("Lisbon after home was learned: %q", o.messages())
	}
}

func TestTravelWelcomeSkipsOrganisationsPlansAndMorePrivateFacts(t *testing.T) {
	for _, f := range [][2]string{
		{"work", "Acme is based in Lisbon."},
		{"work", "User plans to quit the job and move to Lisbon."},
		{"people", "Dan lives in Lisbon and has anxiety."},
		{"people", "Jo moved to Lisbon after the diagnosis."},
		{"people", "Sam lives in Lisbon and can't make rent."},
	} {
		t.Run(f[1], func(t *testing.T) {
			h, o, _ := travel(t, "Europe/Lisbon", f)
			if got := lastSaid(t, o); strings.Contains(got, "You told me") || storedOffer(t, h).Ask != "" {
				t.Fatalf("offered: %q", got)
			}
		})
	}
}

func TestTravelWelcomeSkipsWhereTheOwnerLives(t *testing.T) {
	_, o, _ := travel(t, "Europe/Lisbon", [2]string{"user", "User lives in Lisbon."}, [2]string{"people", "Dan moved to Lisbon."})
	if got := lastSaid(t, o); strings.Contains(got, "You told me") {
		t.Fatalf("welcomed home: %q", got)
	}
}

func TestTravelWelcomeCanBeTurnedOff(t *testing.T) {
	ctx := context.Background()
	sys := "Australia/Melbourne"
	h, o := zoned(t, &sys)
	_, _ = h.store.Remember(ctx, "people", "Dan moved to Lisbon.", "telegram:1")
	_ = h.store.Set(ctx, TravelPeopleOffKey, "1")
	h.look(ctx)
	sys = "Europe/Lisbon"
	h.look(ctx)
	if got := lastSaid(t, o); strings.Contains(got, "You told me") {
		t.Fatalf("turned off, still offered: %q", got)
	}
}

func TestTravelOfferKeptOnlyOnceTheNoticeIsOut(t *testing.T) {
	ctx := context.Background()
	sys := "Australia/Melbourne"
	h, o := zoned(t, &sys)
	_, _ = h.store.Remember(ctx, "people", "Dan moved to Lisbon.", "telegram:1")
	h.look(ctx)
	o.down["telegram:1"] = true
	sys = "Europe/Lisbon"
	h.look(ctx)
	if off := storedOffer(t, h); off.Ask != "" {
		t.Fatalf("an offer that never went out is kept: %+v", off)
	}
	delete(o.down, "telegram:1")
	h.mu.Lock()
	h.zoneRetry = h.now()
	h.mu.Unlock()
	h.look(ctx)
	if got := lastSaid(t, o); !strings.Contains(got, "Dan moved here") || storedOffer(t, h).Ask == "" {
		t.Fatalf("the retried notice lost its offer: %q", o.messages())
	}
}

// The regression: a landing at 01:30 sent the named offer ("You told me
// Dan moved here...") in the owner's quiet hours, with the notice, as a
// full-text notification. The bare notice still goes out; the offer waits
// for the quiet hours to end and is used up only once it has gone out.
func TestTravelWelcomeOfferWaitsOutQuietHours(t *testing.T) {
	ctx := context.Background()
	sys := "Australia/Melbourne"
	h, o := zoned(t, &sys)
	quiet := true
	h.Quiet = func(time.Time) bool { return quiet }
	if _, err := h.store.Remember(ctx, "people", "Dan moved to Lisbon last spring.", "telegram:1"); err != nil {
		t.Fatal(err)
	}
	h.look(ctx)
	sys = "Europe/Lisbon"
	h.look(ctx)
	if got, want := lastSaid(t, o), "telegram:1 You're in Lisbon now; reminders and routines follow local time."; got != want {
		t.Fatalf("in quiet hours, want the bare notice %q, got %q", want, got)
	}
	if off := storedOffer(t, h); off.Ask != "" {
		t.Fatalf("an offer not yet made is kept for a yes: %+v", off)
	}
	h.look(ctx)
	if n := len(o.messages()); n != 1 {
		t.Fatalf("still quiet, yet more was said: %q", o.messages())
	}
	quiet = false
	h.look(ctx)
	want := "telegram:1 You're in Lisbon now. You told me Dan moved here. Want a nudge tomorrow to see if Dan's free for coffee?"
	if got := lastSaid(t, o); got != want || len(o.messages()) != 2 {
		t.Fatalf("once quiet hours end, want\n%q\ngot %q", want, o.messages())
	}
	if off := storedOffer(t, h); off.Who != "Dan" || off.At.IsZero() || !strings.HasSuffix(want, off.Ask) {
		t.Fatalf("offer kept for the yes: %+v", off)
	}
	h.look(ctx)
	if offer, _ := h.knownHere(ctx, "Australia/Melbourne", "Europe/Lisbon"); len(o.messages()) != 2 || offer != "" {
		t.Fatalf("offered twice: %q", o.messages())
	}
}

func TestTravelWelcomeOfferKeptForQuietHoursIsDroppedOnAMove(t *testing.T) {
	ctx := context.Background()
	sys := "Australia/Melbourne"
	h, o := zoned(t, &sys)
	quiet := true
	h.Quiet = func(time.Time) bool { return quiet }
	if _, err := h.store.Remember(ctx, "people", "Dan moved to Lisbon.", "telegram:1"); err != nil {
		t.Fatal(err)
	}
	h.look(ctx)
	sys = "Europe/Lisbon"
	h.look(ctx)
	sys = "Asia/Kathmandu" // moved on before the morning
	h.look(ctx)
	quiet = false
	h.look(ctx)
	for _, m := range o.messages() {
		if strings.Contains(m, "You told me") {
			t.Fatalf("an offer for a city left behind went out: %q", o.messages())
		}
	}
}

func TestPersonInReadsNames(t *testing.T) {
	for in, want := range map[string]string{
		"My friend Dan moved to Lisbon.":         "Dan",
		"Dan Smith lives in Lisbon":              "Dan Smith",
		"In March Dan moved to Lisbon":           "Dan",
		"User's colleague Priya works in Lisbon": "Priya",
		"Dan has just moved to Lisbon":           "Dan",
		"User moved to Lisbon":                   "",
		"Dan moved from Lisbon to Porto":         "",
		"Acme is based in Lisbon":                "",
	} {
		who, _, _ := personIn(in, "Lisbon")
		if who != want {
			t.Errorf("%q: want %q, got %q", in, want, who)
		}
	}
}
