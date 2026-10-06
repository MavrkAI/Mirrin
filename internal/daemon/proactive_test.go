package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/protocols"
)

var briefingProtocol = protocols.Protocol{Name: "morning briefing", Tags: []string{"briefing", "daily"}, Schedule: "0 7 * * *", Prompt: "Prepare the morning briefing."}

const briefingText = "Good morning. Dentist at 3, nothing else today."

// notifications collects the desktop notifications shown for the rest of
// the test.
func notifications(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	prev := desktopNotify
	desktopNotify = func(_, body string) error {
		mu.Lock()
		got = append(got, body)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { desktopNotify = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// atHour sets the daemon's clock to that hour today, in the twin's zone.
func atHour(t *testing.T, d *Daemon, hour int) {
	t.Helper()
	prev := clock
	now := time.Now().In(d.location())
	base := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, d.location())
	start := time.Now()
	clock = func() time.Time { return base.Add(time.Since(start)) }
	t.Cleanup(func() { clock = prev })
}

// screenOnly takes the owner's messaging app away, as for someone who set
// up through the welcome window: only the screen is left.
func screenOnly(td *testDaemon) {
	td.chmu.Lock()
	delete(td.channels, "telegram")
	td.chmu.Unlock()
}

func withVoice(td *testDaemon) *fakeChannel {
	voice := &fakeChannel{name: "voice", owner: "local", out: make(chan string, 8)}
	td.chmu.Lock()
	td.channels["voice"] = voice
	td.chmu.Unlock()
	return voice
}

func screenHistory(t *testing.T, td *testDaemon) []string {
	t.Helper()
	h, err := td.store.History(context.Background(), screenChat, 20)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range h {
		out = append(out, m.PlainText())
	}
	return out
}

func TestProactiveMessagesGoToAMessagingAppElseTheScreen(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	if got := td.proactiveChatKey(); got != ownerKey {
		t.Fatalf("with Telegram: %q", got)
	}
	screenOnly(td)
	withVoice(td)
	td.chmu.Lock()
	td.channels["cli"] = &fakeChannel{name: "cli", owner: "terminal"}
	td.chmu.Unlock()
	if got := td.proactiveChatKey(); got != screenChat {
		t.Fatalf("with only voice and a terminal: %q, want the screen", got)
	}
}

// The regression: with no WhatsApp or Telegram the heartbeat logged "no
// owner chat; skipping protocol", and the briefing it promised never came.
func TestBriefingReachesAnOwnerWithNoMessagingApp(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say(briefingText) })
	screenOnly(td)
	pings := notifications(t)
	seen := listen(t, td.bus)
	td.beat.RunProtocol(context.Background(), briefingProtocol)

	var shown []events.Event
	for _, ev := range seen() {
		if ev.Text == briefingText {
			shown = append(shown, ev)
		}
	}
	if len(shown) != 1 || shown[0].Kind != "message" {
		t.Fatalf("the briefing should be on the screen once, got %+v", shown)
	}
	if data, _ := shown[0].Data.(Left); data.Title != "Morning briefing" || !data.Briefing {
		t.Fatalf("it should be the owner's own message, kept under Left for you: %+v", shown[0].Data)
	}
	if got := pings(); !slices.Equal(got, []string{"Morning briefing is ready."}) {
		t.Fatalf("notifications %q", got)
	}
	if h := screenHistory(t, td); !slices.Contains(h, briefingText) {
		t.Fatalf("a reply on the screen should have the briefing as context: %q", h)
	}
}

func TestBriefingStillGoesToTelegram(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say(briefingText) })
	pings := notifications(t)
	td.beat.RunProtocol(context.Background(), briefingProtocol)
	if got := td.ch.messages(); !slices.Equal(got, []string{"owner: " + briefingText}) {
		t.Fatalf("Telegram got %q", got)
	}
	if got := pings(); len(got) != 0 {
		t.Fatalf("notifications %q", got)
	}
}

// A 7am briefing isn't read to an empty room; a reminder is said, and so is
// what comes while the owner is talking to the twin.
func TestVoiceOnlyOwnerHearsOnlyWhatIsMeantForTheRoom(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say(briefingText) })
	screenOnly(td)
	voice := withVoice(td)
	pings := notifications(t)
	atHour(t, td.Daemon, 7)

	td.beat.RunProtocol(ctx, briefingProtocol)
	if got := voice.messages(); len(got) != 0 {
		t.Fatalf("read to an empty room: %q", got)
	}
	reminder := events.WithSource(ctx, events.Source{Kind: "reminder"})
	if err := td.remind(reminder, screenChat, "Reminder: stretch"); err != nil {
		t.Fatal(err)
	}
	if got := voice.messages(); !slices.Equal(got, []string{"local: Reminder: stretch"}) {
		t.Fatalf("the reminder should be said: %q", got)
	}

	td.noteHeard(channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: "what's on today?", IsOwner: true})
	td.beat.RunProtocol(ctx, briefingProtocol)
	if got := voice.messages(); len(got) != 2 || got[1] != "local: "+briefingText {
		t.Fatalf("said to someone talking to it: %q", got)
	}
	withClock(t, 11*time.Minute)
	td.beat.RunProtocol(ctx, briefingProtocol)
	if got := voice.messages(); len(got) != 2 {
		t.Fatalf("said eleven minutes after they last spoke: %q", got)
	}
	if got := pings(); !slices.Equal(got, []string{"Morning briefing is ready.", "Reminder: stretch", "Morning briefing is ready.", "Morning briefing is ready."}) {
		t.Fatalf("notifications %q", got)
	}
}

// What a follow-up found can be private ("£42 back to your Visa"): unlike a
// reminder it isn't read into a room no one has spoken in, and the desktop
// notification says only what it was about.
func TestAFollowUpIsNotReadIntoAnEmptyRoom(t *testing.T) {
	ctx := events.WithSource(context.Background(), events.Source{Kind: "followup", Name: "Acme's reply about the refund"})
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	screenOnly(td)
	voice := withVoice(td)
	pings := notifications(t)
	atHour(t, td.Daemon, 11)
	found := "Acme replied: £42 back to your Visa in 3 to 5 days."
	if err := td.remind(ctx, screenChat, found); err != nil {
		t.Fatal(err)
	}
	if got := voice.messages(); len(got) != 0 {
		t.Fatalf("read to an empty room: %q", got)
	}
	if got := pings(); !slices.Equal(got, []string{"Following up on Acme's reply about the refund."}) {
		t.Fatalf("notifications %q", got)
	}
	if h := screenHistory(t, td); !slices.Contains(h, found) {
		t.Fatalf("what it found isn't on the screen: %q", h)
	}

	// Said to someone talking to the twin.
	td.noteHeard(channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: "anything new?", IsOwner: true})
	if err := td.remind(ctx, screenChat, found); err != nil {
		t.Fatal(err)
	}
	if got := voice.messages(); !slices.Equal(got, []string{"local: " + found}) {
		t.Fatalf("said %q", got)
	}

	// A notification that can't show, with no one to say it to, means it
	// didn't go, as for a reminder.
	withClock(t, 11*time.Minute)
	desktopNotify = func(string, string) error { return errors.New("no notifications here") }
	if err := td.remind(ctx, screenChat, found); err == nil {
		t.Fatal("a follow-up no one was told of counted as delivered")
	}
}

// Someone else's chat is told apart from the owner's own for the
// heartbeat, so a follow-up promised there doesn't look on its own.
func TestTheHeartbeatKnowsSomeoneElsesChat(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	for key, want := range map[string]bool{"telegram:999": true, "telegram:999#protocol-20261003-101500": true, ownerKey: false, screenChat: false, voiceChat: false} {
		if got := td.beat.Theirs(key); got != want {
			t.Errorf("Theirs(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestNothingIsReadAloudInQuietHours(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	td.cfg.User.QuietHours = "" // the default, 22:00 to 07:00
	screenOnly(td)
	voice := withVoice(td)
	pings := notifications(t)
	atHour(t, td.Daemon, 23)
	td.noteHeard(channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: "goodnight", IsOwner: true})
	if err := td.remind(events.WithSource(ctx, events.Source{Kind: "reminder"}), screenChat, "Reminder: bins out"); err != nil {
		t.Fatal(err)
	}
	if got := voice.messages(); len(got) != 0 {
		t.Fatalf("said at 23:00: %q", got)
	}
	if got := pings(); !slices.Equal(got, []string{"Reminder: bins out"}) {
		t.Fatalf("notifications %q", got)
	}
}

// The watcher and the first-week tips reach an owner who has only the
// screen; a tip comes without a notification.
func TestWatcherAndTipsReachAScreenOnlyOwner(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, func(last string, _ llm.Request) llm.Response {
		if strings.Contains(last, "First-week tour") {
			return say("I can run a briefing at 7 if you'd like one.")
		}
		return say("Your dentist moved to 4pm.")
	})
	screenOnly(td)
	pings := notifications(t)

	source := &injectedInbox{}
	td.watcher.Add(source, true)
	td.watcher.Poll(ctx)
	source.text = "Dentist: your appointment is now at 4pm"
	td.watcher.Poll(ctx)
	if h := screenHistory(t, td); !slices.Contains(h, "Your dentist moved to 4pm.") {
		t.Fatalf("the watcher's message isn't on the screen: %q", h)
	}
	if got := pings(); !slices.Equal(got, []string{"Your dentist moved to 4pm."}) {
		t.Fatalf("notifications %q", got)
	}

	if err := td.store.Set(ctx, "installed_at", time.Now().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	td.nudge(ctx)
	if h := screenHistory(t, td); !slices.Contains(h, "I can run a briefing at 7 if you'd like one.") {
		t.Fatalf("the tip isn't on the screen: %q", h)
	}
	if got := pings(); len(got) != 1 {
		t.Fatalf("a tip came with a notification: %q", got)
	}
}
