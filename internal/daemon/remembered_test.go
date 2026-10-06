package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// noted is the "remembered" events the screens were sent.
func noted(evs []events.Event) []events.Event {
	var out []events.Event
	for _, ev := range evs {
		if ev.Kind == "remembered" {
			out = append(out, ev)
		}
	}
	return out
}

// keep runs a memory tool as the twin would in chatKey, and returns the new fact's id.
func keep(t *testing.T, td *testDaemon, tool, chatKey, subject, fact string) int64 {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"subject": subject, "fact": fact})
	if _, err := td.agent.Tools().Run(context.Background(), tool, tools.Call{ChatKey: chatKey, Input: in}); err != nil {
		t.Fatalf("%s in %s: %v", tool, chatKey, err)
	}
	fs, err := td.store.Recall(context.Background(), "", 1)
	if err != nil || len(fs) == 0 || fs[0].Content != fact {
		t.Fatalf("%q wasn't kept: %v %+v", fact, err, fs)
	}
	return fs[0].ID
}

// Something the owner says on the screen that the twin keeps shows there,
// under the reply, with what Undo needs.
func TestRememberFromTheScreenIsNoted(t *testing.T) {
	td := newTestDaemon(t, func(last string, _ llm.Request) llm.Response {
		switch {
		case strings.Contains(last, "don't eat meat"):
			return call("m1", "remember", `{"subject":"preferences","fact":"Akshay doesn't eat meat."}`)
		case strings.Contains(last, "remembered (#"):
			return say("Good to know. I'll keep that in mind for dinner plans.")
		}
		return say("Heard: " + last)
	})
	got := listen(t, td.bus)
	in := channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "I don't eat meat, by the way.", IsOwner: true}
	if _, err := td.MessageEvents(api.WithClient(context.Background(), "page1"), in, agent.Events{}); err != nil {
		t.Fatal(err)
	}
	fs, _ := td.store.Recall(context.Background(), "meat", 5)
	if len(fs) != 1 {
		t.Fatalf("facts kept: %+v", fs)
	}
	evs := noted(got())
	if len(evs) != 1 || evs[0].Text != "Akshay doesn't eat meat." {
		t.Fatalf("remembered events: %+v", evs)
	}
	b, _ := json.Marshal(evs[0].Data)
	var data struct {
		ID      int64  `json:"id"`
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(b, &data); err != nil || data.ID != fs[0].ID || data.Subject != "preferences" {
		t.Fatalf("event data %s, fact #%d", b, fs[0].ID)
	}
	// Then and there only: a screen opened later, or /screen's recent
	// lines after an Undo, don't find it.
	if kept := noted(td.bus.Recent()); len(kept) != 0 {
		t.Fatalf("the bus kept %+v", kept)
	}
}

// Only what is fine to show, learned in a chat on this Mac, is noted on a
// screen: not a private fact, nor one from WhatsApp, a routine or a task.
func TestOnlyThisMacsOwnPlainFactsAreNoted(t *testing.T) {
	td := newTestDaemon(t, butler)
	got := listen(t, td.bus)
	keep(t, td, "remember_sensitive", "screen:local", "health", "Akshay is allergic to penicillin.") // as it runs once approved
	keep(t, td, "remember", "whatsapp:61400000000", "preferences", "Akshay likes window seats.")
	keep(t, td, "remember", "screen:local#task-0927", "work", "The report is due on Friday.")
	keep(t, td, "remember", "telegram:owner", "people", "Priya is Akshay's sister.")
	if evs := noted(got()); len(evs) != 0 {
		t.Fatalf("noted on the screen: %+v", evs)
	}
	keep(t, td, "remember", "voice:local", "preferences", "Akshay takes his coffee black.")
	keep(t, td, "remember", "cli:terminal", "work", "Akshay's standup is at 9:30.")
	evs := noted(got())
	if len(evs) != 2 || evs[0].Text != "Akshay takes his coffee black." || evs[1].Text != "Akshay's standup is at 9:30." {
		t.Fatalf("noted from this Mac: %+v", evs)
	}
}

// Undo forgets a fact the screen just showed, everywhere, while it is
// fresh. A private fact, one from elsewhere, or one older than ten minutes
// is forgotten by asking instead.
func TestUndoForgetsOnlyWhatWasJustNotedHere(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	forgot := 0
	td.store.OnForgot(func() { forgot++ })

	fresh := keep(t, td, "remember", "screen:local", "preferences", "Akshay doesn't eat meat.")
	if err := td.UndoFact(ctx, fresh); err != nil {
		t.Fatalf("undo of a fresh fact: %v", err)
	}
	if _, err := td.store.FactByID(ctx, fresh); !errors.Is(err, memory.ErrNoFact) || forgot != 1 {
		t.Fatalf("after Undo: %v, forget hooks ran %d times", err, forgot)
	}
	if err := td.UndoFact(ctx, fresh); !errors.Is(err, api.ErrNotUndoable) {
		t.Fatalf("undo twice: %v", err)
	}

	spoken := keep(t, td, "remember", "voice:local", "preferences", "Akshay takes his coffee black.")
	private := keep(t, td, "remember_sensitive", "screen:local", "health", "Akshay is allergic to penicillin.")
	terminal := keep(t, td, "remember", "cli:terminal", "work", "Akshay's standup is at 9:30.")
	away := keep(t, td, "remember", "whatsapp:61400000000", "preferences", "Akshay likes window seats.")
	for name, id := range map[string]int64{"private": private, "terminal": terminal, "whatsapp": away} {
		if err := td.UndoFact(ctx, id); !errors.Is(err, api.ErrNotUndoable) {
			t.Fatalf("undo of the %s fact: %v", name, err)
		}
	}
	withClock(t, 11*time.Minute)
	if err := td.UndoFact(ctx, spoken); !errors.Is(err, api.ErrNotUndoable) {
		t.Fatalf("undo after 11 minutes: %v", err)
	}
	for _, id := range []int64{spoken, private, terminal, away} {
		if _, err := td.store.FactByID(ctx, id); err != nil {
			t.Fatalf("fact #%d went with a refused Undo: %v", id, err)
		}
	}
	if forgot != 1 {
		t.Fatalf("forget hooks ran %d times", forgot)
	}
}
