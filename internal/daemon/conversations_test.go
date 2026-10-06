package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// slowLook registers a read tool that runs until released.
func slowLook(td *testDaemon) (started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	td.agent.Tools().Register(tools.New("look", "look at the site", tools.Schema(nil), tools.RiskRead,
		func(context.Context, tools.Call) (string, error) {
			close(started)
			<-release
			return "looked", nil
		}))
	return started, release
}

func looker(last string, _ llm.Request) llm.Response {
	switch last {
	case "check the site":
		return call("t1", "look", `{}`)
	case "looked":
		return say("All quiet.")
	}
	return say("Heard: " + last)
}

func TestProactiveMessageWaitsForTheTurnToEnd(t *testing.T) {
	td := newTestDaemon(t, looker)
	started, release := slowLook(td)
	done := make(chan string)
	go func() {
		reply, _ := td.message(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "check the site", IsOwner: true}, agent.Events{})
		done <- reply
	}()
	<-started
	// A reminder fires while the tool is running in the same conversation.
	if err := td.Notify(context.Background(), ownerKey, "Reminder: call mum"); err != nil {
		t.Fatal(err)
	}
	if got := td.ch.next(t); got != "Reminder: call mum" {
		t.Fatalf("the reminder should go out at once, got %q", got)
	}
	close(release)
	if got := <-done; got != "All quiet." {
		t.Fatalf("reply %q", got)
	}
	if seen := td.llm.heard(); len(seen) != 2 || seen[1] != "looked" {
		t.Fatalf("the model lost the tool's result: %q", seen)
	}
	h, _ := td.store.History(context.Background(), ownerKey, 10)
	var texts []string
	for _, m := range h {
		if s := m.PlainText(); s != "" {
			texts = append(texts, string(m.Role)+": "+s)
		}
	}
	want := []string{"user: check the site", "assistant: All quiet.", "assistant: Reminder: call mum"}
	if !slices.Equal(texts, want) {
		t.Fatalf("history %q, want %q", texts, want)
	}
}

func TestMessagesDuringALongTurnAreQueuedNotDropped(t *testing.T) {
	td := newTestDaemon(t, looker)
	started, release := slowLook(td)
	send := func(text string) {
		t.Helper()
		returned := make(chan struct{})
		go func() {
			td.handle(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: text, IsOwner: true})
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
			t.Fatalf("handle(%q) blocked the channel's read loop", text)
		}
	}
	send("check the site")
	<-started
	send("also add milk")
	send("and eggs")
	close(release)
	if got := td.ch.next(t); got != "All quiet." {
		t.Fatalf("first reply %q", got)
	}
	// What piled up is answered after, as one thought.
	if got := td.ch.next(t); got != "Heard: also add milk\nand eggs" {
		t.Fatalf("second reply %q", got)
	}
}

func TestQueuedDecisionsAreNotFoldedIntoText(t *testing.T) {
	td := newTestDaemon(t, looker)
	for _, c := range []struct {
		text  string
		plain bool
	}{
		{"also add milk", true},
		{"yes", false},
		{"Yes 12.", false},
		{"/pending", false},
		{"", false},
	} {
		if got := td.plain(channels.Inbound{Text: c.text}); got != c.plain {
			t.Errorf("plain(%q) = %v", c.text, got)
		}
	}
}

func TestAPIConversationsShowOnThePresenceScreen(t *testing.T) {
	for _, c := range []struct {
		channel, text string
		want          []string
	}{
		{"api", "hello", []string{"heard: hello", "state: thinking", "said: Heard: hello", "state: idle"}},
		{"voice", "hello", []string{"heard: hello", "state: thinking", "said: Heard: hello", "state: idle"}}, // the thin voice client
		{"screen", "hello", []string{"state: thinking", "state: idle"}},                                      // it shows its own lines
		{"cli", "/reload", nil}, // a command is not conversation
	} {
		t.Run(c.channel, func(t *testing.T) {
			td := newTestDaemon(t, looker)
			seen := listen(t, td.bus)
			if _, err := td.Message(context.Background(), channels.Inbound{Channel: c.channel, ChatID: "local", Sender: "owner", Text: c.text, IsOwner: true}); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, ev := range seen() {
				got = append(got, ev.Kind+": "+ev.Text)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("events %q, want %q", got, c.want)
			}
		})
	}
}

func TestHeldMessagesKeepTheirOrder(t *testing.T) {
	td := newTestDaemon(t, looker)
	c := td.conv(ownerKey)
	c.begin()
	ctx := context.Background()
	td.record(ctx, ownerKey, llm.Text(llm.RoleAssistant, "first"))
	td.record(ctx, ownerKey, llm.Text(llm.RoleAssistant, "second"))
	if h, _ := td.store.History(ctx, ownerKey, 10); len(h) != 0 {
		t.Fatalf("recorded mid-turn: %+v", h)
	}
	td.end(ownerKey, c)
	h, _ := td.store.History(ctx, ownerKey, 10)
	var texts []string
	for _, m := range h {
		texts = append(texts, m.PlainText())
	}
	if strings.Join(texts, ",") != "first,second" {
		t.Fatalf("history %q", texts)
	}
}
