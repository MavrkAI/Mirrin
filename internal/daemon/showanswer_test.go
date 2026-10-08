package daemon

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

const longDraft = "Here's a draft for Sam.\n\nSubject: Friday\n\nHi Sam, are we still on for eight?"

// openedScreens records the presence screens opened instead of opening them.
func openedScreens(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	old := openScreenCmd
	t.Cleanup(func() { openScreenCmd = old })
	openScreenCmd = func(c *exec.Cmd) error { opened = append(opened, strings.Join(c.Args, " ")); return nil }
	return &opened
}

// A long answer by voice at this computer goes on the presence screen,
// opened at the card when no screen is open here, and the screen can ask
// for it after.
func TestALongVoiceAnswerOpensTheScreenAtIt(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	opened := openedScreens(t)
	td.uiURL = "http://127.0.0.1:1/ui?token=test"
	feed, stop := td.bus.Subscribe()
	defer stop()

	show := td.screenAnswers().For("local")
	if show == nil {
		t.Fatal("voice at this computer can't show an answer")
	}
	if !show(longDraft) {
		t.Fatal("not shown")
	}
	if len(*opened) != 1 || !strings.Contains((*opened)[0], "#show") {
		t.Fatalf("opened %q", *opened)
	}
	if got, ok := td.ShownAnswer(context.Background()); !ok || got.Text != longDraft {
		t.Fatalf("the screen asks and gets %q %v", got.Text, ok)
	}
	select {
	case ev := <-feed:
		if ev.Kind != "show" || ev.Text != longDraft {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no show event")
	}

	// A screen open here shows it itself: no second one.
	closed := td.bus.ScreenOpen()
	defer closed()
	if !show(longDraft) || len(*opened) != 1 {
		t.Fatalf("with a screen open: opened %q", *opened)
	}
}

// A call's caller is someone else, and a messaging chat's owner may be
// anywhere: neither ever opens a screen on this computer.
func TestCallsAndMessagingChatsNeverOpenALocalScreen(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	opened := openedScreens(t)
	td.uiURL = "http://127.0.0.1:1/ui?token=test"
	if td.screenAnswers().For("phone") != nil {
		t.Fatal("a phone call may put answers on the screen")
	}
	for _, key := range []string{phoneChat, phoneChat + "#call-CA1", "whatsapp:447700900000", "telegram:555", "screen:abc"} {
		if td.mayShowAnswer(key) || td.showAnswer(key, longDraft) {
			t.Errorf("%s put an answer on the screen", key)
		}
	}
	if len(*opened) != 0 {
		t.Fatalf("opened %q", *opened)
	}
	if _, ok := td.ShownAnswer(context.Background()); ok {
		t.Fatal("kept an answer that wasn't shown")
	}
}

// "Just read it to me" turns it off, and it stays off until turned on.
func TestReadLongAnswersAloudTurnsTheScreenOff(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	openedScreens(t)
	td.uiURL = "http://127.0.0.1:1/ui?token=test"
	hooks := td.screenAnswers()
	hooks.ReadAloud(true)
	if hooks.For("local") != nil {
		t.Fatal("still shown after \"just read it to me\"")
	}
	hooks.ReadAloud(false)
	if hooks.For("local") == nil {
		t.Fatal("not shown after \"put long answers on my screen\"")
	}
}

// A voice reply that may have gone on the screen is marked so, and a wall
// screen (which shows what was said out loud) leaves it out.
func TestAShownAnswerIsMarkedForWalls(t *testing.T) {
	voiceFrom := map[string]string{"channel": "voice"}
	if got := onScreenOnly("said", longDraft, voiceFrom); got["shown"] != "screen" || got["channel"] != "voice" {
		t.Fatalf("a draft by voice: %v", got)
	}
	if got := onScreenOnly("said", "Eighteen degrees.", voiceFrom); got["shown"] != "" {
		t.Fatalf("a short reply: %v", got)
	}
	if got := onScreenOnly("said", longDraft, map[string]string{"channel": "telegram"}); got["shown"] != "" {
		t.Fatalf("a typed chat: %v", got)
	}
	if voiceFrom["shown"] != "" {
		t.Fatal("changed the turn's own data")
	}
}
