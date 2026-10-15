package voice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
)

// Always-on listening is the default, so a twin whose owner hasn't run
// `mirrin voice setup` wants to listen and can't. That is "not set up", not
// a failure; once the hearing model is there and listening still isn't
// running, it is one.
func TestListeningWantedButNotSetUpIsNotAFailure(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("ELEVENLABS_API_KEY", "")
	dir := t.TempDir()
	v := config.Voice{Enabled: true, Mode: "wake", WhisperModel: filepath.Join(dir, "ggml-base.en.bin"), KokoroDir: dir, WhisperBin: "whisper-cli"}
	listening := false
	row := func() health.Result {
		t.Helper()
		m := health.New(Checks(Probe{Config: func() config.Voice { return v }, Listening: func() bool { return listening }})...)
		for _, r := range m.Run(context.Background()).Results {
			if r.Name == "voice.listening" {
				return r
			}
		}
		t.Fatal("no voice.listening row")
		return health.Result{}
	}
	if SetUp(v) {
		t.Fatal("wanting to listen is not being set up")
	}
	if r := row(); r.State != health.Off || !strings.Contains(r.Detail, "mirrin voice setup") {
		t.Fatalf("wanted but not set up: %s %q", r.State, r.Detail)
	}
	if err := os.WriteFile(v.WhisperModel, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := row(); r.State != health.Fail {
		t.Fatalf("set up and wanted but not running: %s %q", r.State, r.Detail)
	}
	listening = true
	if r := row(); r.State != health.OK {
		t.Fatalf("listening: %s %q", r.State, r.Detail)
	}
}
