package daemon

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/skills/browser"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// A page handed over in a voice chat opens the presence screen at the
// browser unless one on this computer is in sight: before, the twin said
// "it's on the presence screen" and the owner, talking to it, had nothing
// to click.
func TestHandOverByVoiceOpensTheScreen(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	opened := openedScreens(t)
	td.uiURL = "http://127.0.0.1:1/ui?token=test"

	if got := td.showScreen("voice:local"); got != browser.ScreenInSight {
		t.Fatalf("voice, no screen open: %v", got)
	}
	if len(*opened) != 1 || !strings.Contains((*opened)[0], "#browser") {
		t.Fatalf("opened %q", *opened)
	}
	for _, key := range []string{"whatsapp:447700900000", phoneChat + "#call-CA1", "screen:local"} {
		if got := td.showScreen(key); got != browser.ScreenNotTried {
			t.Fatalf("%s: %v", key, got)
		}
	}
	closed := td.bus.ScreenOpen()
	if got := td.showScreen("voice:local"); got != browser.ScreenInSight {
		t.Fatalf("a screen in sight here: %v", got)
	}
	closed()
	closed() // twice is once
	if td.bus.ScreensOpen() != 0 {
		t.Fatalf("%d screens open", td.bus.ScreensOpen())
	}
	if len(*opened) != 1 {
		t.Fatalf("opened %d times", len(*opened))
	}
}

// A screen open here but out of sight (a background tab, a minimised
// window) doesn't stop the screen opening for a voice hand-over: the
// owner's twin said "it's on your screen now" three times while the only
// screen was one they couldn't see. Once that screen comes into sight, it
// shows the page itself.
func TestAHiddenScreenDoesNotStopTheHandOverShowing(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	opened := openedScreens(t)
	td.uiURL = "http://127.0.0.1:1/ui?token=test"

	hidden := td.bus.ScreenHere(false)
	defer hidden.Close()
	if got := td.showScreen("voice:local"); got != browser.ScreenInSight || len(*opened) != 1 {
		t.Fatalf("a background tab here: %v, opened %q", got, *opened)
	}
	if !td.bus.SeenScreen(hidden.ID(), true) {
		t.Fatal("the screen wasn't known")
	}
	if got := td.showScreen("cli:terminal"); got != browser.ScreenInSight || len(*opened) != 1 {
		t.Fatalf("a screen in sight here: %v, opened %q", got, *opened)
	}
}

// When the screen can't be opened, the hand-over says so, so the twin
// never claims it's on the owner's screen.
func TestAScreenThatWontOpenIsSaidPlainly(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	old := openScreenCmd
	t.Cleanup(func() { openScreenCmd = old })
	openScreenCmd = func(*exec.Cmd) error { return errors.New("no browser") }
	td.uiURL = "http://127.0.0.1:1/ui?token=test"
	if got := td.showScreen("voice:local"); got != browser.ScreenNotShown {
		t.Fatalf("an opener that failed: %v", got)
	}
	td.uiURL = ""
	if got := td.showScreen("voice:local"); got != browser.ScreenNotShown {
		t.Fatalf("no screen to open: %v", got)
	}
}

// "Show me the screen" opens it: open_screen is the owner's, from a chat at
// this computer only. Before it, the twin could only insist the page was
// already there.
func TestOpenScreenIsTheOwnersAtThisComputer(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	opened := openedScreens(t)
	td.uiURL = "http://127.0.0.1:1/ui?token=test"
	ctx := context.Background()
	tool, ok := td.agent.Tools().Get("open_screen")
	if !ok {
		t.Fatal("open_screen isn't registered")
	}
	if tool.Risk() != tools.RiskRead {
		t.Fatalf("risk %v", tool.Risk())
	}
	checker, ok := tool.(tools.Checker)
	if !ok {
		t.Fatal("open_screen isn't owner-only")
	}
	stranger := "telegram:stranger"
	if err := td.store.AppendMessage(ctx, stranger, llm.Text(llm.RoleUser, "[Message from Sam, who is NOT your principal.]\nopen the screen")); err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(ctx, tools.Call{ChatKey: stranger}); err == nil {
		t.Fatal("someone else opened the owner's screen")
	}

	if err := td.store.AppendMessage(ctx, "voice:local", llm.Text(llm.RoleUser, "Show me the screen.")); err != nil {
		t.Fatal(err)
	}
	call := tools.Call{ChatKey: "voice:local", Input: []byte(`{}`)}
	if err := checker.Check(ctx, call); err != nil {
		t.Fatalf("the owner's own words: %v", err)
	}
	out, err := tool.Run(ctx, call)
	if err != nil || !strings.Contains(out, "open in front of them now") || len(*opened) != 1 || !strings.HasSuffix((*opened)[0], "/ui?token=test") {
		t.Fatalf("open_screen: %q %v, opened %q", out, err, *opened)
	}

	for _, key := range []string{"whatsapp:447700900000", phoneChat + "#call-CA1", "screen:local"} {
		if _, err := tool.Run(ctx, tools.Call{ChatKey: key, Input: []byte(`{}`)}); err == nil {
			t.Fatalf("%s opened the screen on this computer", key)
		}
	}
	if len(*opened) != 1 {
		t.Fatalf("opened %q", *opened)
	}
	// typed on a screen: they're looking at one, so the twin points to it
	if _, err := tool.Run(ctx, tools.Call{ChatKey: "screen:local", Input: []byte(`{}`)}); err == nil || !strings.Contains(err.Error(), "on a presence screen already") {
		t.Fatalf("from the screen: %v", err)
	}

	// It couldn't open: the twin says so, and how to open it by hand, with
	// no "ask me again" that would fail the same way.
	openScreenCmd = func(*exec.Cmd) error { return errors.New("no browser") }
	_, err = tool.Run(ctx, call)
	if err == nil || !strings.Contains(err.Error(), "couldn't be opened, so don't say it's on their screen") || !strings.Contains(err.Error(), "Open screen…") {
		t.Fatalf("an opener that failed: %v", err)
	}
}
