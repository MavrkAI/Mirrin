package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/protocols"
	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
)

// briefingTwin is a test twin on its first hello, with the protocols a
// fresh install ships (the bundled morning briefing among them) unless
// bundled is false. facts collects what each hello was told.
func briefingTwin(t *testing.T, bundled bool) (*testDaemon, *[]string) {
	t.Helper()
	facts := &[]string{}
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		if strings.HasPrefix(last, "Time: ") {
			*facts = append(*facts, last)
			return say("Afternoon, Akshay. Type to me whenever you like.")
		}
		return say("All set.")
	})
	off := false
	td.cmu.Lock()
	td.cfg.UI.Weather = &off // only the model answers here
	td.cmu.Unlock()
	if bundled {
		if err := protocols.WriteExamples(td.cfg.ProtocolsDir); err != nil {
			t.Fatal(err)
		}
		if err := td.ReloadProtocols(); err != nil {
			t.Fatal(err)
		}
	}
	return td, facts
}

func briefingSchedule(t *testing.T, td *testDaemon) (string, bool) {
	t.Helper()
	p, ok := protocols.Find(td.Protocols(), briefingName)
	if !ok {
		return "", false
	}
	return p.Schedule, p.IsEnabled()
}

// The first hello offers the morning briefing once, after its words: as a
// request on the screen's conversation, said there, and not promised in
// the hello itself. A hello said again offers the same request.
func TestFirstHelloOffersTheBriefing(t *testing.T) {
	td, facts := briefingTwin(t, true)
	ctx := context.Background()
	at(t, td.Daemon, 14, 0)
	reply, err := td.Hello(ctx, func(string) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Offer == nil || reply.Offer.Text != "Want me to brief you at seven tomorrow?" || reply.Offer.Time != "07:00" {
		t.Fatalf("offer %+v", reply.Offer)
	}
	if strings.Contains((*facts)[0], "morning briefing") {
		t.Fatalf("the hello promised the briefing it then offers:\n%s", (*facts)[0])
	}
	ap, err := td.store.GetApproval(ctx, reply.Offer.ID)
	if err != nil || ap.Status != "pending" || ap.Tool != briefingTool || ap.ChatKey != screenChat {
		t.Fatalf("the request: %+v %v", ap, err)
	}
	h, _ := td.store.History(ctx, screenChat, 10)
	if len(h) < 2 || h[len(h)-1].PlainText() != briefingAsk || h[len(h)-2].PlainText() != reply.Text {
		t.Fatalf("the screen's conversation: %v", h)
	}
	// Nothing is installed by the offer itself.
	b, _ := os.ReadFile(filepath.Join(td.cfg.ProtocolsDir, briefingFile))
	if string(b) != protocols.Examples[briefingFile] {
		t.Fatalf("the offer changed the briefing:\n%s", b)
	}

	again, err := td.Hello(ctx, func(string) {}, nil)
	if err != nil || again.Offer == nil || again.Offer.ID != reply.Offer.ID {
		t.Fatalf("hello again: %+v %v", again.Offer, err)
	}
	if aps, _ := td.store.PendingApprovals(ctx, screenChat); len(aps) != 1 {
		t.Fatalf("%d requests waiting", len(aps))
	}
}

// Yes on the welcome card at the time chosen sets the briefing to run then,
// turned on; the request is settled, the offer isn't made again, and the
// first week's day-one tip about briefings is skipped.
func TestBriefingYesFromTheWelcomeCard(t *testing.T) {
	td, _ := briefingTwin(t, true)
	ctx := context.Background()
	if _, ok := td.nudgeTask(1); !ok {
		t.Fatal("setup: the day-one tip is due")
	}
	reply, err := td.Hello(ctx, func(string) {}, nil)
	if err != nil || reply.Offer == nil {
		t.Fatalf("%+v %v", reply, err)
	}
	said, err := td.AnswerBriefing(ctx, true, "07:30")
	if err != nil || !strings.HasPrefix(said, "Done. I'll brief you at 7:30 am every morning, starting ") {
		t.Fatalf("%q %v", said, err)
	}
	if s, on := briefingSchedule(t, td); s != "30 7 * * *" || !on {
		t.Fatalf("briefing %q on %v", s, on)
	}
	if ap, _ := td.store.GetApproval(ctx, reply.Offer.ID); ap.Status != "superseded" {
		t.Fatalf("the request is %s", ap.Status)
	}
	if _, ok := td.nudgeTask(1); ok {
		t.Fatal("the day-one tip about briefings after saying yes to one")
	}
	if _, ok := td.nudgeTask(2); !ok {
		t.Fatal("the rest of the tour stopped")
	}
	if again, _ := td.Hello(ctx, func(string) {}, nil); again.Offer != nil {
		t.Fatal("offered again after yes")
	}
}

// With no briefing on this twin at all, yes installs the bundled one.
func TestBriefingYesInstallsTheBundledOne(t *testing.T) {
	td, _ := briefingTwin(t, false)
	ctx := context.Background()
	if r, _ := td.Hello(ctx, func(string) {}, nil); r.Offer == nil {
		t.Fatal("no offer")
	}
	if _, err := td.AnswerBriefing(ctx, true, "6:00"); err != nil {
		t.Fatal(err)
	}
	p, ok := protocols.Find(td.Protocols(), briefingName)
	if !ok || p.Schedule != "0 6 * * *" || !p.IsEnabled() || !strings.Contains(p.Prompt, "NOTHING_TO_REPORT") {
		t.Fatalf("installed %+v", p)
	}
	if _, err := td.AnswerBriefing(ctx, true, "25:00"); err == nil {
		t.Fatal("a time that doesn't exist was taken")
	}
}

// Later installs nothing, turns the request down, and isn't asked again.
func TestBriefingLaterInstallsNothing(t *testing.T) {
	td, _ := briefingTwin(t, false)
	ctx := context.Background()
	reply, _ := td.Hello(ctx, func(string) {}, nil)
	if reply.Offer == nil {
		t.Fatal("no offer")
	}
	said, err := td.AnswerBriefing(ctx, false, "")
	if err != nil || said != briefingLater {
		t.Fatalf("%q %v", said, err)
	}
	if _, ok := protocols.Find(td.Protocols(), briefingName); ok {
		t.Fatal("installed on later")
	}
	if ap, _ := td.store.GetApproval(ctx, reply.Offer.ID); ap.Status != "denied" {
		t.Fatalf("the request is %s", ap.Status)
	}
	if again, _ := td.Hello(ctx, func(string) {}, nil); again.Offer != nil {
		t.Fatal("offered again after later")
	}
	tip, ok := td.nudgeTask(1)
	if !ok || !strings.Contains(tip, "reminders") {
		t.Fatalf("the day-one tip went with a later: %q", tip)
	}
	if strings.Contains(tip, "offer to set one up") || strings.Contains(tip, "briefing at 7") {
		t.Fatalf("the day-one tip offers the briefing again after later: %q", tip)
	}
}

// While the welcome card's offer still waits, the day-one tip doesn't make
// a second one; before any offer, it does.
func TestDayOneTipWhileTheOfferWaits(t *testing.T) {
	td, _ := briefingTwin(t, false)
	ctx := context.Background()
	if tip, _ := td.nudgeTask(1); !strings.Contains(tip, "offer to set one up") {
		t.Fatalf("before the hello: %q", tip)
	}
	if r, _ := td.Hello(ctx, func(string) {}, nil); r.Offer == nil {
		t.Fatal("no offer")
	}
	tip, ok := td.nudgeTask(1)
	if !ok || strings.Contains(tip, "offer to set one up") || strings.Contains(tip, "briefing at 7") {
		t.Fatalf("the day-one tip with the offer waiting: %q", tip)
	}
}

// On a fresh install the bundled briefing ships turned on at seven. Later
// on the welcome card parks it, so no briefing comes after all, and the
// offer isn't made again.
func TestBriefingLaterParksTheBundledOne(t *testing.T) {
	td, _ := briefingTwin(t, true)
	ctx := context.Background()
	if s, on := briefingSchedule(t, td); s != "0 7 * * *" || !on {
		t.Fatalf("setup: the bundled briefing is %q on %v", s, on)
	}
	reply, _ := td.Hello(ctx, func(string) {}, nil)
	if reply.Offer == nil {
		t.Fatal("no offer")
	}
	if said, err := td.AnswerBriefing(ctx, false, ""); err != nil || said != briefingLater {
		t.Fatalf("%q %v", said, err)
	}
	if s, on := briefingSchedule(t, td); s == "" || on {
		t.Fatalf("after later the briefing is %q on %v", s, on)
	}
	if v, _ := td.store.Get(ctx, briefingOfferKey); v != "later" {
		t.Fatalf("the offer is %q", v)
	}
	if again, _ := td.Hello(ctx, func(string) {}, nil); again.Offer != nil {
		t.Fatal("offered again after later")
	}
	if s, on := briefingSchedule(t, td); s == "" || on {
		t.Fatalf("hello again turned it back on: %q on %v", s, on)
	}
}

// The offer turned down through the ordinary request (a bare "no" on the
// screen) or left to lapse is a later too: the bundled briefing is parked.
func TestBriefingNoOrLapseParksTheBundledOne(t *testing.T) {
	for _, how := range []string{"no", "lapse"} {
		t.Run(how, func(t *testing.T) {
			td, _ := briefingTwin(t, true)
			ctx := context.Background()
			reply, _ := td.Hello(ctx, func(string) {}, nil)
			if reply.Offer == nil {
				t.Fatal("no offer")
			}
			if how == "no" {
				in := channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "no", IsOwner: true}
				if _, err := td.message(ctx, in, agent.Events{}); err != nil {
					t.Fatal(err)
				}
			} else {
				withClock(t, approvalTTL+time.Minute)
				td.expireApprovals(ctx)
			}
			want := map[string]string{"no": "denied", "lapse": "expired"}[how]
			if ap, _ := td.store.GetApproval(ctx, reply.Offer.ID); ap.Status != want {
				t.Fatalf("the request is %s", ap.Status)
			}
			if s, on := briefingSchedule(t, td); s == "" || on {
				t.Fatalf("the briefing is %q on %v", s, on)
			}
			if v, _ := td.store.Get(ctx, briefingOfferKey); v != "later" {
				t.Fatalf("the offer is %q", v)
			}
		})
	}
}

// A briefing the owner changed themselves while the offer waited is
// theirs: Later leaves it running, exactly as they wrote it.
func TestBriefingLaterLeavesTheOwnersOneAlone(t *testing.T) {
	td, _ := briefingTwin(t, true)
	ctx := context.Background()
	if r, _ := td.Hello(ctx, func(string) {}, nil); r.Offer == nil {
		t.Fatal("no offer")
	}
	file := filepath.Join(td.cfg.ProtocolsDir, briefingFile)
	mine := "# mine now\n" + protocols.Examples[briefingFile]
	if err := os.WriteFile(file, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := td.ReloadProtocols(); err != nil {
		t.Fatal(err)
	}
	if _, err := td.AnswerBriefing(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	if s, on := briefingSchedule(t, td); s != "0 7 * * *" || !on {
		t.Fatalf("the owner's briefing is %q on %v", s, on)
	}
	if b, _ := os.ReadFile(file); string(b) != mine {
		t.Fatalf("the owner's file changed:\n%s", b)
	}
}

// Before seven the first briefing is today, and the offer says so.
func TestBriefingOfferBeforeSeven(t *testing.T) {
	td, _ := briefingTwin(t, true)
	at(t, td.Daemon, 6, 0)
	reply, err := td.Hello(context.Background(), func(string) {}, nil)
	if err != nil || reply.Offer == nil || reply.Offer.Text != briefingAskAM {
		t.Fatalf("%+v %v", reply.Offer, err)
	}
}

// A bare "yes" typed on the screen after the hello answers the offer.
func TestBriefingBareYesOnTheScreen(t *testing.T) {
	td, _ := briefingTwin(t, false)
	ctx := context.Background()
	if r, _ := td.Hello(ctx, func(string) {}, nil); r.Offer == nil {
		t.Fatal("no offer")
	}
	in := channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "yes", IsOwner: true}
	if _, err := td.message(ctx, in, agent.Events{}); err != nil {
		t.Fatal(err)
	}
	if s, on := briefingSchedule(t, td); s != "0 7 * * *" || !on {
		t.Fatalf("briefing %q on %v", s, on)
	}
	if !td.briefingAccepted() {
		t.Fatal("the yes wasn't kept")
	}
}

// A briefing the owner set up themselves is theirs: nothing is offered,
// and the hello may mention it.
func TestNoBriefingOfferWhenTheOwnerHasOne(t *testing.T) {
	td, facts := briefingTwin(t, false)
	if err := os.MkdirAll(td.cfg.ProtocolsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td.cfg.ProtocolsDir, briefingFile), []byte("name: morning briefing\nschedule: \"0 6 * * 1-5\"\nprompt: Brief me.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := td.ReloadProtocols(); err != nil {
		t.Fatal(err)
	}
	reply, err := td.Hello(context.Background(), func(string) {}, nil)
	if err != nil || reply.Offer != nil {
		t.Fatalf("%+v %v", reply.Offer, err)
	}
	if !strings.Contains((*facts)[0], "morning briefing") {
		t.Fatalf("their routine left out:\n%s", (*facts)[0])
	}
}

// With the calendar connected, the hello knows the afternoon: the only
// thing left today.
func TestFirstHelloKnowsYourAfternoon(t *testing.T) {
	td, facts := briefingTwin(t, true)
	fake := googletest.NewInProcess()
	td.google.Transport = fake.Transport()
	t.Cleanup(func() { settleGoogle(td) })
	fake.WriteFiles(t, td.google.CredentialsFile, td.google.TokenFile, time.Now().Add(time.Hour))
	if err := td.UpdateConfig(func(c *config.Config) { c.Skills.Calendar.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	td.applyGoogle()
	if !td.CalendarConnected(context.Background()) {
		t.Fatal("setup: calendar not connected")
	}
	now := at(t, td.Daemon, 14, 0)
	start := now.Add(90 * time.Minute)
	fake.AddEvent(googletest.Event{ID: "e1", Summary: "Coffee with Dana", Start: start.Format(time.RFC3339), End: start.Add(30 * time.Minute).Format(time.RFC3339)})
	fake.AddEvent(googletest.Event{ID: "e2", Summary: "Train", Start: now.AddDate(0, 0, 1).Format(time.RFC3339), End: now.AddDate(0, 0, 1).Add(time.Hour).Format(time.RFC3339)})

	if _, err := td.Hello(context.Background(), func(string) {}, nil); err != nil {
		t.Fatal(err)
	}
	if want := "The only thing left on the calendar today: “Coffee with Dana” at 3:30 pm."; !strings.Contains((*facts)[0], want) {
		t.Fatalf("the hello wasn't told %q:\n%s", want, (*facts)[0])
	}
}

// When Google fails, the first hello goes on with the time and says nothing
// of the calendar or the failure.
func TestFirstHelloWithGoogleFailing(t *testing.T) {
	td2, facts2 := briefingTwin(t, true)
	broken := googletest.NewInProcess()
	td2.google.Transport = broken.Transport()
	t.Cleanup(func() { settleGoogle(td2) })
	broken.WriteFiles(t, td2.google.CredentialsFile, td2.google.TokenFile, time.Now().Add(time.Hour))
	if err := td2.UpdateConfig(func(c *config.Config) { c.Skills.Calendar.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	td2.applyGoogle()
	broken.Disable("calendar")
	reply, err := td2.Hello(context.Background(), func(string) {}, nil)
	if err != nil || reply.Text == "" {
		t.Fatalf("Google failing stopped the hello: %v", err)
	}
	if got := strings.ToLower((*facts2)[0]); strings.Contains(got, "calendar") || strings.Contains(got, "google") || !strings.Contains(got, "time: ") {
		t.Fatalf("the hello was told of the failure, or not the time:\n%s", (*facts2)[0])
	}
}
