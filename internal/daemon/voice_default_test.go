package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Always-on listening is the default, so a twin whose voice isn't set up
// wants to listen and can't. Saving any setting then must not complain
// about it; once the hearing model is there, a save does start listening,
// and says so when it can't.
func TestSavingBeforeVoiceSetupDoesNotTryToListen(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	if v := td.Config().Channels.Voice; !v.Enabled || v.Mode != "wake" {
		t.Fatalf("the default should want to listen: %+v", v)
	}
	if err := td.UpdateConfig(func(c *config.Config) { c.User.Name = "Akshay" }); err != nil {
		t.Fatalf("saving before voice setup: %v", err)
	}
	if td.Listening() {
		t.Fatal("nothing to listen with yet")
	}
	model := filepath.Join(t.TempDir(), "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := td.UpdateConfig(func(c *config.Config) {
		c.Channels.Voice.WhisperModel = model
		c.Channels.Voice.WhisperBin = filepath.Join(t.TempDir(), "no-whisper")
	})
	if err == nil || !strings.Contains(err.Error(), "could not start listening") {
		t.Fatalf("set up but unable to start, and quiet about it: %v", err)
	}
}

// Listening is the default, so a test twin whose hearing model a test puts
// on disk would start the real microphone on the next save. The test
// daemon's whisper binary must therefore never exist.
func TestTheTestDaemonCanNeverHear(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	if _, err := exec.LookPath(td.Config().Channels.Voice.WhisperBin); err == nil {
		t.Fatalf("the test daemon could start the microphone: %s is installed", td.Config().Channels.Voice.WhisperBin)
	}
}

// A terminal session (`mirrin chat` with no twin running) is a conversation
// in a terminal, not the twin under the service: it never opens the
// microphone, however the settings read. The service and the menu bar do.
func TestATerminalChatNeverStartsListening(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	for _, tc := range []struct {
		name        string
		interactive bool
		want        bool
	}{{"service", false, true}, {"chat in a terminal", true, false}} {
		d := &Daemon{cfg: cfg, interactive: tc.interactive}
		if got := d.listensOnStart(); got != tc.want {
			t.Errorf("%s: listens on start %v, want %v", tc.name, got, tc.want)
		}
	}
	cfg.Channels.Voice.Mode = "push"
	if d := (&Daemon{cfg: cfg}); d.listensOnStart() {
		t.Error("push to talk listens on start")
	}
}
