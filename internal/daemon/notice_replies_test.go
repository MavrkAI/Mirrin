package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/heartbeat"
	"github.com/MavrkAI/Mirrin/internal/jev/jevtest"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
)

// withJev switches Jev on for td, asking a fake; on is false to leave it
// off with the key saved.
func withJev(t *testing.T, td *testDaemon, on bool) *jevtest.Server {
	t.Helper()
	srv := jevtest.New(t)
	td.jev.url = srv.URL
	if err := td.UpdateConfig(func(c *config.Config) { c.Jev = config.Jev{Enabled: on, TypeSafeAPIKey: srv.Key()} }); err != nil {
		t.Fatal(err)
	}
	return srv
}

// noticeDaemon is a butler twin with Jev on and the travel offer made.
func noticeDaemon(t *testing.T) (*testDaemon, *jevtest.Server) {
	t.Helper()
	t.Setenv("TYPESAFE_API_KEY", "")
	td := newTestDaemon(t, butler)
	srv := withJev(t, td, true)
	offerTravel(t, td, clock())
	return td, srv
}

func offerStored(t *testing.T, td *testDaemon) bool {
	t.Helper()
	v, _ := td.store.Get(context.Background(), heartbeat.TravelOfferKey)
	return v != ""
}

// "sure go ahead", read by Jev as a clear yes, sets the owner's reminder
// for ten tomorrow and messages nobody; Jev never hears the friend's name.
func TestJevReadsAYesToTheTravelOffer(t *testing.T) {
	ctx := context.Background()
	td, srv := noticeDaemon(t)
	srv.Choose("answer", "accept", 0.95, 0.9)
	got := td.owner(t, "sure go ahead")
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
	if offerStored(t, td) {
		t.Fatal("the offer is still open")
	}
	if len(td.ran()) != 0 {
		t.Fatalf("someone was messaged: %q", td.ran())
	}
	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	state := string(reqs[0].State)
	if !strings.Contains(state, "[a friend]") || strings.Contains(strings.ToLower(state), "dan") || !strings.Contains(state, "sure go ahead") {
		t.Fatalf("state sent: %s", state)
	}
	if opts := reqs[0].Questions["answer"].Options(); strings.Join(opts, ",") != "accept,accept_changed,decline,stop_all,ack,other" {
		t.Fatalf("options %q", opts)
	}
}

func TestJevReadsANoAndAStopToTheTravelOffer(t *testing.T) {
	ctx := context.Background()
	td, srv := noticeDaemon(t)
	srv.Choose("answer", "decline", 0.92, 0.8)
	if got := td.owner(t, "nah leave it"); got != "No problem. Enjoy the trip." {
		t.Fatalf("no: %q", got)
	}
	if rs, _ := td.store.PendingReminders(ctx, ownerKey); len(rs) != 0 {
		t.Fatalf("a no set a reminder: %+v", rs)
	}
	if offerStored(t, td) {
		t.Fatal("the offer is still open")
	}

	offerTravel(t, td, clock())
	srv.Choose("answer", "stop_all", 0.95, 0.9)
	if got := td.owner(t, "stop sending these"); !strings.Contains(got, `Say "start telling me these again"`) {
		t.Fatalf("stop: %q", got)
	}
	if off, _ := td.store.Get(ctx, heartbeat.TravelPeopleOffKey); off != "1" {
		t.Fatal("not turned off")
	}
}

// A yes with a change, or a yes Jev isn't sure enough of, goes to the
// model and leaves the offer open.
func TestJevLeavesChangedOrUnsureYesesToTheModel(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		text, option string
		p, conf      float64
	}{
		{"yes but friday", "accept_changed", 0.95, 0.9},
		{"sure go ahead", "accept", 0.85, 0.9},
		{"sure go ahead", "accept", 0.95, 0.6},
		{"ok", "ack", 0.95, 0.9},
	} {
		td, srv := noticeDaemon(t)
		srv.Choose("answer", c.option, c.p, c.conf)
		if got := td.owner(t, c.text); got != "Heard: "+c.text {
			t.Fatalf("%q as %s %.2f/%.2f: %q", c.text, c.option, c.p, c.conf, got)
		}
		if srv.Calls() != 1 || !offerStored(t, td) {
			t.Fatalf("%q: %d calls, offer kept %v", c.text, srv.Calls(), offerStored(t, td))
		}
		if rs, _ := td.store.PendingReminders(ctx, ownerKey); len(rs) != 0 {
			t.Fatalf("%q set a reminder", c.text)
		}
	}
}

// With Jev off (its key saved), nothing is sent and the exact words still
// work.
func TestJevOffLeavesTheTravelOfferToTheExactWords(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	td := newTestDaemon(t, butler)
	srv := withJev(t, td, false)
	offerTravel(t, td, clock())
	srv.Choose("answer", "accept", 0.99, 0.99)
	for _, text := range []string{"sure go ahead", "nah leave it", "stop sending these"} {
		if got := td.owner(t, text); got != "Heard: "+text {
			t.Fatalf("%q: %q", text, got)
		}
		offerTravel(t, td, clock())
	}
	if got := td.owner(t, "go ahead"); !strings.HasPrefix(got, "Done. I'll nudge you") {
		t.Fatalf("go ahead: %q", got)
	}
	if srv.Calls() != 0 {
		t.Fatalf("%d requests with Jev off", srv.Calls())
	}
}

// An offer a day old, or one the twin has spoken since, isn't asked about.
func TestJevIsNotAskedAboutAStaleOffer(t *testing.T) {
	ctx := context.Background()
	t.Setenv("TYPESAFE_API_KEY", "")
	td := newTestDaemon(t, butler)
	srv := withJev(t, td, true)
	offerTravel(t, td, clock().Add(-25*time.Hour))
	srv.Choose("answer", "accept", 0.99, 0.99)
	td.owner(t, "sure go ahead")

	td2, srv2 := noticeDaemon(t)
	srv2.Choose("answer", "accept", 0.99, 0.99)
	_ = td2.Notify(ctx, ownerKey, "Your parcel has arrived.")
	td2.owner(t, "sure go ahead")
	if srv.Calls()+srv2.Calls() != 0 {
		t.Fatalf("%d and %d requests", srv.Calls(), srv2.Calls())
	}
}

// While an approval waits on the owner here, Jev isn't asked, and the
// approval is left as it was.
func TestJevIsNotAskedWhileAnApprovalWaits(t *testing.T) {
	ctx := context.Background()
	td, srv := noticeDaemon(t)
	id := raise(t, td, ownerKey, "send", `{"to":"boss"}`)
	// Asked about in this chat, as a check the twin sent alongside would.
	c := td.conv(ownerKey)
	c.mu.Lock()
	c.asked, c.askedAt = []int64{id}, clock()
	c.mu.Unlock()
	if !td.openQuestion(ownerKey) {
		t.Fatal("setup: no open question")
	}
	if _, ok := td.travelOfferOpen(ctx, ownerKey); !ok {
		t.Fatal("setup: the offer isn't open")
	}
	srv.Choose("answer", "accept", 0.99, 0.99)
	td.owner(t, "that'd be great")
	if srv.Calls() != 0 {
		t.Fatalf("%d requests", srv.Calls())
	}
	if ap, _ := td.store.GetApproval(ctx, id); ap.Status != "pending" {
		t.Fatalf("approval %s", ap.Status)
	}
}

// Someone else, a group, or a channel anyone could forge: no request.
func TestJevIsOnlyAskedForTheOwnerInTheirOwnChat(t *testing.T) {
	ctx := context.Background()
	td, srv := noticeDaemon(t)
	srv.Choose("answer", "accept", 0.99, 0.99)
	for _, in := range []channels.Inbound{
		{Channel: "telegram", ChatID: "owner", Sender: "dan", Text: "sure go ahead"},
		{Channel: "telegram", ChatID: "family", Sender: "owner", Text: "sure go ahead", IsOwner: true},
		{Channel: "mail", ChatID: "owner", Sender: "owner", Text: "sure go ahead", IsOwner: true},
	} {
		if _, ok := td.noticeReply(ctx, in); ok {
			t.Fatalf("%+v was answered", in)
		}
		if _, err := td.message(ctx, in, agent.Events{}); err != nil {
			t.Fatal(err)
		}
	}
	if srv.Calls() != 0 || !offerStored(t, td) {
		t.Fatalf("%d requests, offer kept %v", srv.Calls(), offerStored(t, td))
	}
	// Nor for a long message.
	if _, ok := td.noticeReply(ctx, channels.Inbound{Channel: "telegram", ChatID: "owner", Text: strings.Repeat("a", 201), IsOwner: true}); ok || srv.Calls() != 0 {
		t.Fatal("a long message was asked about")
	}
}

// Slow or busy, Jev is given up on within its two seconds, and the message
// goes to the model with the offer still open.
func TestASlowOrBusyJevFallsThrough(t *testing.T) {
	for _, slow := range []bool{true, false} {
		td, srv := noticeDaemon(t)
		srv.Choose("answer", "accept", 0.99, 0.99)
		if slow {
			srv.Delay(3 * time.Second)
		} else {
			srv.Fail(529, 2)
		}
		start := time.Now()
		if got := td.owner(t, "sure go ahead"); got != "Heard: sure go ahead" {
			t.Fatalf("slow %v: %q", slow, got)
		}
		if took := time.Since(start); took > 2500*time.Millisecond {
			t.Fatalf("slow %v: took %v", slow, took)
		}
		if !offerStored(t, td) {
			t.Fatal("the offer was settled")
		}
	}
}

// weeklyJevDaemon is an echo twin with a busy week, Jev on, and the
// weekly note just sent.
func weeklyJevDaemon(t *testing.T) (*testDaemon, *jevtest.Server, string) {
	t.Helper()
	t.Setenv("TYPESAFE_API_KEY", "")
	td := newTestDaemon(t, echo)
	busyWeek(t, td)
	srv := withJev(t, td, true)
	td.weeklyNote(context.Background())
	return td, srv, td.ch.next(t)
}

// "Stop sending these" right after the weekly note turns it off; Jev is
// told only what kind of note it was, never what it said.
func TestJevReadsStopSendingTheseAfterTheWeeklyNote(t *testing.T) {
	ctx := context.Background()
	td, srv, note := weeklyJevDaemon(t)
	srv.Choose("answer", "stop_these", 0.95, 0.9)
	got := td.owner(t, "stop sending these")
	if !strings.Contains(got, "No more weekly notes") || !strings.Contains(got, "start the weekly note") {
		t.Fatalf("reply %q", got)
	}
	if off, _ := td.store.Get(ctx, keyWeeklyOff); off != "1" {
		t.Fatal("the weekly note is still on")
	}
	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	state := string(reqs[0].State)
	if !strings.Contains(state, "Sunday note") {
		t.Fatalf("state %s", state)
	}
	for _, line := range strings.Split(note, "\n") {
		for _, w := range []string{"parking", "42.10", "follow-up", "errand"} {
			if strings.Contains(line, w) && strings.Contains(state, w) {
				t.Fatalf("the note's %q was sent: %s", w, state)
			}
		}
	}
	if h, _ := td.store.History(ctx, ownerKey, 2); len(h) != 2 || h[0].PlainText() != "stop sending these" || h[1].PlainText() != got {
		t.Fatalf("history %+v", h)
	}
	td.weeklyNote(ctx)
	if sent := td.ch.messages(); len(sent) != 1 {
		t.Fatalf("a note after it was turned off: %q", sent)
	}
}

// Something said since the note, a note Jev isn't sure the owner wants
// stopped, or an exact phrase: no request, or no change.
func TestJevAndTheWeeklyNoteOtherwise(t *testing.T) {
	ctx := context.Background()
	td, srv, _ := weeklyJevDaemon(t)
	if err := td.store.AppendMessage(ctx, ownerKey, llm.Text(llm.RoleAssistant, "Anything else?")); err != nil {
		t.Fatal(err)
	}
	srv.Choose("answer", "stop_these", 0.99, 0.99)
	td.owner(t, "stop sending these")
	if srv.Calls() != 0 {
		t.Fatalf("%d requests after the twin spoke", srv.Calls())
	}

	td2, srv2, _ := weeklyJevDaemon(t)
	srv2.Choose("answer", "change_them", 0.95, 0.9)
	td2.owner(t, "only the money bits please")
	if off, _ := td2.store.Get(ctx, keyWeeklyOff); off != "" || srv2.Calls() != 1 {
		t.Fatalf("off %q after %d requests", off, srv2.Calls())
	}

	td3, srv3, _ := weeklyJevDaemon(t)
	if got := td3.owner(t, "No more weekly notes."); !strings.Contains(got, "No more weekly notes") || srv3.Calls() != 0 {
		t.Fatalf("exact phrase: %q after %d requests", got, srv3.Calls())
	}
}

// briefJevDaemon is a twin that has just briefed the owner about Priya,
// with Jev on.
func briefJevDaemon(t *testing.T) (*testDaemon, *jevtest.Server) {
	t.Helper()
	t.Setenv("TYPESAFE_API_KEY", "")
	ctx := context.Background()
	td, fake, now, _ := briefDaemon(t, func(string, llm.Request) llm.Response { return say(priyaLine) })
	srv := withJev(t, td, true)
	if _, err := td.store.Remember(ctx, "work", "Sam promised Priya the Q3 numbers.", ownerKey); err != nil {
		t.Fatal(err)
	}
	meeting(fake, "ev1", "Catch-up", now.Add(10*time.Minute), googletest.Attendee{Email: "priya@example.com", Name: "Priya"})
	td.meetingBriefs(ctx)
	if got := td.ch.next(t); !strings.HasSuffix(got, priyaLine) {
		t.Fatalf("the brief %q", got)
	}
	return td, srv
}

func TestJevReadsIDontNeedTheseAfterABrief(t *testing.T) {
	td, srv := briefJevDaemon(t)
	srv.Choose("answer", "stop_these", 0.95, 0.9)
	got := td.owner(t, "I don't need these")
	if !strings.HasPrefix(got, "Understood") || !strings.Contains(got, "start the meeting briefs") {
		t.Fatalf("reply %q", got)
	}
	if td.Config().Watch.Briefs() {
		t.Fatal("briefs are still on")
	}
	if state := string(srv.Requests()[0].State); strings.Contains(state, "Priya") || strings.Contains(state, "Q3") || !strings.Contains(state, "before a meeting") {
		t.Fatalf("state %s", state)
	}
}

func TestJevIsNotAskedAboutAnOldBriefOrForTheExactWords(t *testing.T) {
	td, srv := briefJevDaemon(t)
	withClock(t, 3*time.Hour+time.Minute)
	srv.Choose("answer", "stop_these", 0.99, 0.99)
	td.owner(t, "I don't need these")
	if srv.Calls() != 0 || !td.Config().Watch.Briefs() {
		t.Fatalf("%d requests, briefs on %v", srv.Calls(), td.Config().Watch.Briefs())
	}

	td2, srv2 := briefJevDaemon(t)
	if got := td2.owner(t, "stop the meeting briefs"); !strings.HasPrefix(got, "Understood") || srv2.Calls() != 0 {
		t.Fatalf("exact phrase: %q after %d requests", got, srv2.Calls())
	}
}

// modelHeard reports whether the model was handed text as the owner's turn.
func modelHeard(td *testDaemon, text string) bool {
	for _, h := range td.llm.heard() {
		if strings.Contains(h, text) {
			return true
		}
	}
	return false
}

// With Jev off (its key saved), a reply to the weekly note or a brief is
// the model's, exactly as before: nothing sent, nothing switched off.
func TestJevOffLeavesNoteRepliesToTheModel(t *testing.T) {
	ctx := context.Background()
	td, _, _ := weeklyJevDaemon(t)
	srv := withJev(t, td, false)
	srv.Choose("answer", "stop_these", 0.99, 0.99)
	if got := td.owner(t, "stop sending these"); !modelHeard(td, "stop sending these") {
		t.Fatalf("weekly: %q", got)
	}
	if off, _ := td.store.Get(ctx, keyWeeklyOff); off != "" {
		t.Fatal("the weekly note was turned off with Jev off")
	}

	td2, _ := briefJevDaemon(t)
	srv2 := withJev(t, td2, false)
	srv2.Choose("answer", "stop_these", 0.99, 0.99)
	td2.owner(t, "I don't need these")
	if !td2.Config().Watch.Briefs() {
		t.Fatal("briefs were turned off with Jev off")
	}
	if srv.Calls()+srv2.Calls() != 0 {
		t.Fatalf("%d and %d requests with Jev off", srv.Calls(), srv2.Calls())
	}
}

// Jev must be sure of a "stop these" before a note is switched off.
func TestJevMustBeSureToStopTheWeeklyNote(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		option  string
		p, conf float64
	}{
		{"stop_these", 0.85, 0.9},
		{"stop_these", 0.95, 0.6},
		{"about_this_one", 0.95, 0.9},
	} {
		td, srv, _ := weeklyJevDaemon(t)
		srv.Choose("answer", c.option, c.p, c.conf)
		if got := td.owner(t, "stop sending these"); !modelHeard(td, "stop sending these") {
			t.Fatalf("%s %.2f/%.2f: %q", c.option, c.p, c.conf, got)
		}
		if off, _ := td.store.Get(ctx, keyWeeklyOff); off != "" || srv.Calls() != 1 {
			t.Fatalf("%s %.2f/%.2f: off %q after %d requests", c.option, c.p, c.conf, off, srv.Calls())
		}
	}
}

// A reply a day and a half after the weekly note, or after briefs were
// switched off another way, isn't asked about.
func TestJevIsNotAskedAboutAnOldWeeklyNoteOrBriefsAlreadyOff(t *testing.T) {
	td, srv, _ := weeklyJevDaemon(t)
	srv.Choose("answer", "stop_these", 0.99, 0.99)
	func() {
		withClock(t, 36*time.Hour+time.Minute)
		td.owner(t, "stop sending these")
	}()

	td2, srv2 := briefJevDaemon(t)
	if err := td2.setMeetingBriefs(false); err != nil {
		t.Fatal(err)
	}
	srv2.Choose("answer", "stop_these", 0.99, 0.99)
	td2.owner(t, "I don't need these")
	if srv.Calls()+srv2.Calls() != 0 {
		t.Fatalf("%d and %d requests", srv.Calls(), srv2.Calls())
	}
}

// A no or a stop Jev isn't sure enough of leaves the offer open.
func TestJevMustBeSureOfANoOrAStop(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		text, option string
		p, conf      float64
	}{
		{"nah leave it", "decline", 0.80, 0.9},
		{"nah leave it", "decline", 0.92, 0.5},
		{"stop sending these", "stop_all", 0.85, 0.9},
		{"stop sending these", "stop_all", 0.95, 0.6},
	} {
		td, srv := noticeDaemon(t)
		srv.Choose("answer", c.option, c.p, c.conf)
		if got := td.owner(t, c.text); got != "Heard: "+c.text {
			t.Fatalf("%q as %s %.2f/%.2f: %q", c.text, c.option, c.p, c.conf, got)
		}
		if off, _ := td.store.Get(ctx, heartbeat.TravelPeopleOffKey); off != "" || !offerStored(t, td) {
			t.Fatalf("%q as %s %.2f/%.2f: off %q, offer kept %v", c.text, c.option, c.p, c.conf, off, offerStored(t, td))
		}
	}
}

// With Jev on, the exact words still settle the offer without asking it.
func TestJevOnLeavesTheExactWordsToTheExactRule(t *testing.T) {
	td, srv := noticeDaemon(t)
	srv.Choose("answer", "decline", 0.99, 0.99)
	if got := td.owner(t, "go ahead"); !strings.HasPrefix(got, "Done. I'll nudge you") || srv.Calls() != 0 {
		t.Fatalf("go ahead: %q after %d requests", got, srv.Calls())
	}
}

// Each local check on its own keeps a message from Jev, with the offer the
// twin's last word in that chat: a group, a channel anyone could forge, and
// a message too long in characters or in words.
func TestEachCheckAloneKeepsAMessageFromJev(t *testing.T) {
	ctx := context.Background()
	td, srv := noticeDaemon(t)
	srv.Choose("answer", "accept", 0.99, 0.99)
	td.channels["irc"] = &fakeChannel{name: "irc", owner: "owner", out: make(chan string, 1)}
	for _, key := range []string{"telegram:family", "irc:owner"} {
		if err := td.store.AppendMessage(ctx, key, llm.Text(llm.RoleAssistant, travelNotice)); err != nil {
			t.Fatal(err)
		}
		if _, ok := td.travelOfferOpen(ctx, key); !ok {
			t.Fatalf("setup: the offer isn't open in %s", key)
		}
	}
	if !td.ownersOwnChat("irc:owner") {
		t.Fatal("setup: irc:owner isn't the owner's own chat")
	}
	for _, in := range []channels.Inbound{
		{Channel: "telegram", ChatID: "family", Sender: "owner", Text: "sure go ahead", IsOwner: true},
		{Channel: "irc", ChatID: "owner", Sender: "owner", Text: "sure go ahead", IsOwner: true},
		{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: strings.Repeat("a", 201), IsOwner: true},
		{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: strings.TrimSpace(strings.Repeat("ok ", 31)), IsOwner: true},
	} {
		if _, ok := td.noticeReply(ctx, in); ok {
			t.Fatalf("%+v was answered", in)
		}
	}
	if srv.Calls() != 0 || !offerStored(t, td) {
		t.Fatalf("%d requests, offer kept %v", srv.Calls(), offerStored(t, td))
	}
	// The longest message allowed is asked about.
	if _, ok := td.noticeReply(ctx, channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: strings.Repeat("a", 200), IsOwner: true}); !ok || srv.Calls() != 1 {
		t.Fatalf("200 characters: %d requests", srv.Calls())
	}
}
