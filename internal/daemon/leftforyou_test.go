package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// leftTexts is what Left for you holds now, oldest first.
func leftTexts(td *testDaemon) []string {
	var out []string
	for _, l := range td.leftForYou(context.Background()) {
		out = append(out, l.Text)
	}
	return out
}

// What the twin sends on its own reaches the screens as a message, and stays
// under Left for you by what it came from. A system notice stays a notice,
// and a reminder for someone else's chat isn't the owner's to keep.
func TestWhatItSendsUnpromptedIsLeftForYou(t *testing.T) {
	td := newTestDaemon(t, looker)
	ctx := context.Background()
	seen := listen(t, td.bus)
	briefing := events.WithSource(ctx, events.Source{Kind: "protocol", Name: "morning briefing", Briefing: true})
	if err := td.Notify(briefing, ownerKey, "Good morning. Dentist at 3."); err != nil {
		t.Fatal(err)
	}
	if err := td.Notify(ctx, ownerKey, "Telegram was slow a moment ago"); err != nil {
		t.Fatal(err)
	}
	if err := td.Notify(events.WithSource(ctx, events.Source{Kind: "reminder"}), "telegram:friend", "Reminder: Sam's bike"); err != nil {
		t.Fatal(err)
	}
	if err := td.Notify(events.WithSource(ctx, events.Source{Kind: "question", Name: "Book the cab"}), ownerKey, "Book the cab: 5:10 or 5:40?"); err != nil {
		t.Fatal(err)
	}

	kinds := map[string]events.Event{}
	for _, ev := range seen() {
		kinds[ev.Text] = ev
	}
	ev := kinds["Good morning. Dentist at 3."]
	l, _ := ev.Data.(Left)
	if ev.Kind != "message" || l.ID == "" || l.Source != "protocol" || l.Title != "Morning briefing" || !l.Briefing || l.Text != "" {
		t.Fatalf("the briefing reached the screens as %s %+v", ev.Kind, ev.Data)
	}
	if ev := kinds["Telegram was slow a moment ago"]; ev.Kind != "notice" {
		t.Fatalf("a message with no source reached the screens as %s", ev.Kind)
	}
	if ev := kinds["Reminder: Sam's bike"]; ev.Kind != "notice" {
		t.Fatalf("someone else's reminder reached the screens as %s", ev.Kind)
	}
	if ev := kinds["Book the cab: 5:10 or 5:40?"]; ev.Kind != "message" {
		t.Fatalf("a task's question reached the screens as %s", ev.Kind)
	}

	left := td.leftForYou(ctx)
	if len(left) != 1 || left[0].ID != l.ID || left[0].Text != "Good morning. Dentist at 3." || left[0].Title != "Morning briefing" || !left[0].Briefing {
		t.Fatalf("left for you %+v (only the briefing: a question waits under Needs you)", left)
	}
}

// Each message is named by what it came from.
func TestLeftForYouTitles(t *testing.T) {
	td := newTestDaemon(t, looker)
	for _, c := range []struct {
		src  events.Source
		want string
	}{
		{events.Source{Kind: "protocol", Name: "morning briefing"}, "Morning briefing"},
		{events.Source{Kind: "reminder"}, "Reminder"},
		{events.Source{Kind: "task", Name: "Book the cab"}, "Book the cab"},
		{events.Source{Kind: "watch", Name: "calendar"}, "Calendar"},
		{events.Source{Kind: "watch", Name: "gmail"}, "Inbox"},
		{events.Source{Kind: "watch", Name: "inbox"}, "Inbox"},
		{events.Source{Kind: "tip"}, "From Mirrin"},
		{events.Source{Kind: "idea"}, "An idea"},
	} {
		if got := td.leftTitle(c.src); got != c.want {
			t.Errorf("%+v: %q, want %q", c.src, got, c.want)
		}
	}
}

// Left for you keeps the last twelve, for 18 hours: it is a card, not an
// inbox to empty.
func TestLeftForYouKeepsTwelveFor18Hours(t *testing.T) {
	td := newTestDaemon(t, looker)
	ctx := events.WithSource(context.Background(), events.Source{Kind: "reminder"})
	for i := 1; i <= 14; i++ {
		if err := td.Notify(ctx, ownerKey, fmt.Sprintf("Reminder: %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	got := leftTexts(td)
	if len(got) != leftMax || got[0] != "Reminder: 3" || got[11] != "Reminder: 14" {
		t.Fatalf("left for you %q", got)
	}
	ids := map[string]bool{}
	for _, l := range td.leftForYou(context.Background()) {
		ids[l.ID] = true
	}
	if len(ids) != leftMax {
		t.Fatalf("ids aren't each their own: %v", ids)
	}

	withClock(t, 17*time.Hour)
	if got := leftTexts(td); len(got) != leftMax {
		t.Fatalf("after 17 hours: %q", got)
	}
	withClock(t, 2*time.Hour)
	if got := leftTexts(td); len(got) != 0 {
		t.Fatalf("after 19 hours: %q", got)
	}
	if err := td.Notify(ctx, ownerKey, "Reminder: bins out"); err != nil {
		t.Fatal(err)
	}
	if got := leftTexts(td); len(got) != 1 || got[0] != "Reminder: bins out" {
		t.Fatalf("after a new one: %q", got)
	}
}

// The screen no longer has a Noticed list: the watcher's raw From and
// Subject lines are written before the model decides a change is routine.
// What the watcher has to say arrives in its own words, under Left for you.
func TestTheScreenShowsNoWatcherLines(t *testing.T) {
	td := newTestDaemon(t, looker)
	off := false
	td.cfg.UI.Weather = &off // no network
	ctx := context.Background()
	td.store.Audit(ctx, "watch.change", ownerKey, "gmail: NEW: unread from bank@example.com Subject: Your overdraft")
	if err := td.Notify(events.WithSource(ctx, events.Source{Kind: "watch", Name: "gmail"}), ownerKey, "The bank wrote; nothing to do."); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(td.screenData(ctx))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if strings.Contains(body, `"notices"`) || strings.Contains(body, "overdraft") {
		t.Fatalf("the screen shows the watcher's raw lines: %s", body)
	}
	if !strings.Contains(body, `"left_for_you":[{`) || !strings.Contains(body, `"title":"Inbox"`) || !strings.Contains(body, "The bank wrote; nothing to do.") {
		t.Fatalf("the watcher's message isn't left for you: %s", body)
	}
}

// quietReconnects scales the reconnect announcement for a test: a minute
// becomes a tenth of a second.
func quietReconnects(t *testing.T, quiet, back time.Duration) {
	t.Helper()
	oldQuiet, oldBack := reconnectQuiet, backAfter
	reconnectQuiet, backAfter = quiet, back
	t.Cleanup(func() { reconnectQuiet, backAfter = oldQuiet, oldBack })
}

func reconnecting(evs []events.Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Kind == "notice" && strings.Contains(ev.Text, "reconnecting") {
			out = append(out, ev.Text)
		}
	}
	return out
}

// Opening the laptop drops Telegram for a minute or two while the Wi-Fi
// comes back: the orb no longer pops up to say "Telegram is reconnecting".
// A channel down for three minutes is still announced.
func TestABriefChannelDropSaysNothing(t *testing.T) {
	d := newChannelsDaemon(t)
	quietReconnects(t, 600*time.Millisecond, 50*time.Millisecond)
	seen := listen(t, d.bus)
	downUntil := time.Now().Add(150 * time.Millisecond) // "90 seconds"
	mk := func() channels.Channel {
		return &fakeTransport{name: "telegram", owner: "42", start: func(f *fakeTransport, ctx context.Context, _ channels.Handler) error {
			if time.Now().Before(downUntil) {
				return offline
			}
			f.Up()
			<-ctx.Done()
			return nil
		}}
	}
	fastRestarts(t, mk)
	d.launch("telegram", mk())
	waitFor(t, "telegram back", func() bool { st, _ := d.channelStatus("telegram"); return st.State == channels.Connected })
	time.Sleep(900 * time.Millisecond) // past when it would have been announced
	if got := reconnecting(seen()); len(got) != 0 {
		t.Fatalf("a brief drop was announced: %q", got)
	}
}

func TestAChannelDownForMinutesIsAnnounced(t *testing.T) {
	d := newChannelsDaemon(t)
	quietReconnects(t, 300*time.Millisecond, 50*time.Millisecond)
	seen := listen(t, d.bus)
	mk := func() channels.Channel {
		return &fakeTransport{name: "telegram", owner: "42", start: func(*fakeTransport, context.Context, channels.Handler) error { return offline }}
	}
	fastRestarts(t, mk)
	d.launch("telegram", mk())
	if got := reconnecting(seen()); len(got) != 0 {
		t.Fatalf("announced at once: %q", got)
	}
	var got []string
	waitFor(t, "the announcement", func() bool { got = reconnecting(seen()); return len(got) > 0 })
	time.Sleep(400 * time.Millisecond)
	if got = reconnecting(seen()); len(got) != 1 || !strings.Contains(got[0], "Telegram is reconnecting") {
		t.Fatalf("announcements %q", got)
	}
}
