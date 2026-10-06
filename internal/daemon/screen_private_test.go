package daemon

import (
	"context"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Each line a turn shows on the screens says which channel it came from, so
// a wall screen that only looks can keep to what was said out loud (api's
// screen_private.go). A screen's own lines still say which page they came
// from.
func TestScreenLinesSayTheirChannel(t *testing.T) {
	td := newTestDaemon(t, looker)
	ctx := context.Background()
	seen := listen(t, td.bus)
	td.answer(ctx, channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "hello", IsOwner: true})
	in := channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "what's up?", IsOwner: true}
	if _, err := td.MessageEvents(api.WithClient(ctx, "phoneA"), in, agent.Events{}); err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{
		"heard: hello":            {"channel": "telegram"},
		"said: Heard: hello":      {"channel": "telegram"},
		"heard: what's up?":       {"channel": "screen", "origin": "phoneA"},
		"said: Heard: what's up?": {"channel": "screen", "origin": "phoneA"},
	}
	n := 0
	for _, ev := range seen() {
		if ev.Kind != "heard" && ev.Kind != "said" {
			continue
		}
		n++
		k := ev.Kind + ": " + ev.Text
		from, _ := ev.Data.(map[string]string)
		if w, ok := want[k]; !ok || len(from) != len(w) || from["channel"] != w["channel"] || from["origin"] != w["origin"] {
			t.Errorf("%s: data %v, want %v", k, ev.Data, w)
		}
	}
	if n != len(want) {
		t.Fatalf("saw %d lines, want %d", n, len(want))
	}
}

// A message the twin sends on its own (a watcher's news, a briefing) is the
// owner's business: its notice on the screens says where it went, which
// keeps it off a wall screen. The twin's own system lines carry nothing.
func TestAMessageToTheOwnerIsMarkedAsTheirs(t *testing.T) {
	td := newTestDaemon(t, looker)
	seen := listen(t, td.bus)
	if err := td.Notify(context.Background(), ownerKey, "Your overdraft fee is due"); err != nil {
		t.Fatal(err)
	}
	td.voiceProblem("The microphone stopped")
	var marked, system bool
	for _, ev := range seen() {
		switch {
		case ev.Kind == "notice" && ev.Text == "Your overdraft fee is due":
			from, _ := ev.Data.(map[string]string)
			marked = from["channel"] == "telegram"
		case ev.Kind == "notice" && ev.Text == "The microphone stopped":
			system = ev.Data == nil
		}
	}
	if !marked || !system {
		t.Fatalf("the owner's message marked %v, the system line bare %v", marked, system)
	}
}
