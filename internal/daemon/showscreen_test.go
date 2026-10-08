package daemon

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// A page handed over in a voice chat opens the presence screen at the
// browser when none is open on this computer: before, the twin said "it's on
// the presence screen" and the owner, talking to it, had nothing to click.
func TestHandOverByVoiceOpensTheScreen(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	var opened []string
	old := openScreenCmd
	t.Cleanup(func() { openScreenCmd = old })
	openScreenCmd = func(c *exec.Cmd) error { opened = append(opened, strings.Join(c.Args, " ")); return nil }
	td.uiURL = "http://127.0.0.1:1/ui?token=test"

	if !td.showScreen("voice:local") {
		t.Fatal("voice, no screen open: nothing opened")
	}
	if len(opened) != 1 || !strings.Contains(opened[0], "#browser") {
		t.Fatalf("opened %q", opened)
	}
	if td.showScreen("whatsapp:447700900000") || td.showScreen(phoneChat+"#call-CA1") {
		t.Fatal("a chat away from this computer opened the screen")
	}
	closed := td.bus.ScreenOpen()
	if td.showScreen("voice:local") {
		t.Fatal("a screen already open here opened another")
	}
	closed()
	closed() // twice is once
	if td.bus.ScreensOpen() != 0 {
		t.Fatalf("%d screens open", td.bus.ScreensOpen())
	}
	if len(opened) != 1 {
		t.Fatalf("opened %d times", len(opened))
	}
}
