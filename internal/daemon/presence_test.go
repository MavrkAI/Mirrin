package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// kinds lists the events seen as "kind: text".
func kinds(evs []events.Event) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Kind+": "+ev.Text)
	}
	return out
}

// She is answering out loud when a Telegram reply finishes elsewhere: she
// keeps speaking, and the screen never hears she went idle mid-sentence.
func TestATurnEndingElsewhereLeavesHerSpeaking(t *testing.T) {
	td := newTestDaemon(t, looker)
	ch := td.newVoiceChannel(td.Config().Channels.Voice)
	seen := listen(t, td.bus)
	ch.OnState("listening")
	ch.OnState("thinking")
	ch.OnState("speaking")
	td.answer(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "hello", IsOwner: true})
	if got := td.Events().State(); got != "speaking" {
		t.Fatalf("state %q after the Telegram turn, want speaking", got)
	}
	ch.OnState("idle")
	if got := td.Events().State(); got != "idle" {
		t.Fatalf("state %q after she finished", got)
	}
	want := []string{"state: listening", "state: thinking", "state: speaking", "heard: hello", "said: Heard: hello", "state: idle"}
	if got := kinds(seen()); !slices.Equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
}

// Someone else's chat doesn't animate the owner's character or show on the
// screen as the owner's own lines; they still get their answer.
func TestAStrangersChatStaysOffTheScreen(t *testing.T) {
	td := newTestDaemon(t, looker)
	seen := listen(t, td.bus)
	td.answer(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "999", Sender: "Bob", Text: "is Tony in?"})
	if got := seen(); len(got) != 0 {
		t.Fatalf("a stranger's chat reached the screen: %q", kinds(got))
	}
	if got := td.ch.messages(); !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "999: ") }) {
		t.Fatalf("Bob wasn't answered: %q", got)
	}
	// The owner's own chat still shows.
	td.answer(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "hello", IsOwner: true})
	want := []string{"heard: hello", "state: thinking", "state: idle", "said: Heard: hello"}
	if got := kinds(seen()); !slices.Equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
}

// A voice channel that was replaced (voice stopped and started again) can't
// show over the new one as it winds down.
func TestAReplacedVoiceChannelNoLongerShows(t *testing.T) {
	td := newTestDaemon(t, looker)
	old := td.newVoiceChannel(td.Config().Channels.Voice)
	old.OnState("speaking")
	cur := td.newVoiceChannel(td.Config().Channels.Voice)
	if got := td.Events().State(); got != "idle" {
		t.Fatalf("state %q once the old channel was replaced", got)
	}
	cur.OnState("listening")
	old.OnState("speaking")
	if got := td.Events().State(); got != "listening" {
		t.Fatalf("state %q, want the new channel's listening", got)
	}
}

// Voice turned off just after "Hey …": the screen goes back to idle rather
// than saying Listening until voice is next turned on, and the channel
// winding down can't bring it back.
func TestVoiceStoppedMidListenGoesIdle(t *testing.T) {
	td := newTestDaemon(t, looker)
	ch := td.newVoiceChannel(td.Config().Channels.Voice)
	ch.OnState("listening")
	if got := td.Events().State(); got != "listening" {
		t.Fatalf("state %q after the wake word", got)
	}
	td.StopVoice()
	if got := td.Events().State(); got != "idle" {
		t.Fatalf("state %q once voice was turned off, want idle", got)
	}
	ch.OnState("listening")
	if got := td.Events().State(); got != "idle" {
		t.Fatalf("state %q from a stopped channel, want idle", got)
	}
}
