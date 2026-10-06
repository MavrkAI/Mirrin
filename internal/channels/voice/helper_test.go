package voice

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
)

// On Windows the wake helper crashed at the first follow-up window:
// draining the microphone pipe used fcntl, which Windows doesn't have, so
// listening stopped after the first reply. The helper is run here with
// fcntl missing, as on Windows (numpy and openWakeWord are stubbed; only
// the microphone reader is exercised).
func TestWakeHelperDrainsTheMicrophoneWithoutFcntl(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		if py, err = exec.LookPath("python"); err != nil {
			t.Skip("no Python")
		}
	}
	dir := t.TempDir()
	stubs := filepath.Join(dir, "stubs")
	for name, body := range map[string]string{
		"numpy.py":                 "",
		"soundfile.py":             "",
		"openwakeword/__init__.py": "",
		"openwakeword/model.py":    "class Model:\n    pass\n",
	} {
		p := filepath.Join(stubs, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "wake_helper.py"), wakeScript, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
import os, sys, time
sys.modules["fcntl"] = None  # as on Windows: "import fcntl" fails
sys.path.insert(0, sys.argv[1]); sys.path.insert(0, sys.argv[2])
import wake_helper as wh
n = wh.CHUNK * 2
r, w = os.pipe()
mic = wh.Mic(os.fdopen(r, "rb", buffering=0), n)
os.write(w, b"\x01" * n * 3)
deadline = time.time() + 5
while mic.q.qsize() < 3 and time.time() < deadline:
    time.sleep(0.01)
dropped = mic.drain()
assert dropped == n * 3, dropped
os.write(w, b"\x02" * n)
assert mic.read() == b"\x02" * n
os.close(w)
assert mic.read() == b""
assert mic.drain() == 0
print("ok")
`
	out, err := exec.Command(py, "-c", script, stubs, dir).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("%v\n%s", err, out)
	}
}

// Doctor checked hearing and speaking only for always-on listening, so a
// push-to-talk user (mirrin voice) got no help at all.
func TestVoiceChecksServePushToTalk(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("ELEVENLABS_API_KEY", "")
	dir := t.TempDir()
	// A model setup doesn't download (so its size isn't known).
	v := config.Voice{Mode: "push", WhisperModel: filepath.Join(dir, "ggml-tiny.en.bin"), KokoroDir: dir, WhisperBin: "whisper-cli"}
	listening, started := false, 0
	run := func() map[string]health.Result {
		m := health.New(Checks(Probe{
			Config:    func() config.Voice { return v },
			Listening: func() bool { return listening },
			Start:     func() error { started++; listening = true; return nil },
		})...)
		out := map[string]health.Result{}
		for _, r := range m.Run(context.Background()).Results {
			out[r.Name] = r
		}
		return out
	}

	// Never set up: nothing to fix, and it says how to start.
	for name, r := range run() {
		if r.State != health.Off || !strings.Contains(r.Detail, "mirrin voice setup") {
			t.Errorf("not set up: %s is %s %q", name, r.State, r.Detail)
		}
	}

	// Set up for push-to-talk: every part is checked for real.
	_ = os.WriteFile(v.WhisperModel, []byte("weights"), 0o644)
	v.WhisperBin = "no-such-whisper-cli"
	v.Language = "fr"
	v.Engine = "kokoro"
	got := run()
	if len(got) != 5 {
		t.Fatalf("%d rows, want 5", len(got))
	}
	if r := got["ears.whisper"]; r.State != health.Fail || r.Fix == "" {
		t.Errorf("hearing: %+v", r)
	}
	if r := got["ears.model"]; r.State != health.Warn || !strings.Contains(r.Detail, "French") || !strings.Contains(r.Fix, "mirrin voice setup") {
		t.Errorf("model: %+v", r)
	}
	if r := got["voice.speaking"]; r.State != health.Warn || !strings.Contains(r.Detail, "Kokoro is chosen but not installed") {
		t.Errorf("speaking: %+v", r)
	}
	if r := got["voice.listening"]; r.State != health.Off || !strings.Contains(r.Detail, "push-to-talk") {
		t.Errorf("listening: %+v", r)
	}

	// A download that stopped part way is incomplete, not a working model.
	v.WhisperModel, v.Language = filepath.Join(dir, "ggml-small.en.bin"), ""
	_ = os.WriteFile(v.WhisperModel, []byte("weights"), 0o644)
	if r := run()["ears.model"]; r.State != health.Warn || !strings.Contains(r.Detail, "incomplete") {
		t.Errorf("truncated model: %+v", r)
	}

	// Always-on and not running: a failure the check repairs.
	v.Enabled, v.Mode, v.WakeWord = true, "wake", "juno"
	if r := run()["voice.listening"]; r.State != health.OK || started != 1 || !strings.Contains(r.Detail, `"Hey Juno"`) {
		t.Errorf("listening after repair: %+v (started %d)", r, started)
	}
}

// In a room with a fan running (about 1% background) the follow-up window
// wanted speech at 2.75%, more than a voice across the desk gives, so every
// follow-up after a reply was "nothing heard". A normal voice over that
// room must count as speech; the room itself must not.
func TestFollowupHearsSpeechOverANoisyRoom(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		if py, err = exec.LookPath("python"); err != nil {
			t.Skip("no Python")
		}
	}
	dir := t.TempDir()
	stubs := filepath.Join(dir, "stubs")
	for name, body := range map[string]string{
		"numpy.py":                 "",
		"soundfile.py":             "",
		"openwakeword/__init__.py": "",
		"openwakeword/model.py":    "class Model:\n    pass\n",
	} {
		p := filepath.Join(stubs, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "wake_helper.py"), wakeScript, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
import sys
sys.path.insert(0, sys.argv[1]); sys.path.insert(0, sys.argv[2])
import wake_helper as wh
for noise, voice in ((0.011, 0.022), (0.015, 0.028), (0.004, 0.009)):
    assert voice > wh.speech_floor(noise), (noise, voice, wh.speech_floor(noise))
    assert noise * 1.3 < wh.speech_floor(noise), (noise, wh.speech_floor(noise))
assert wh.speech_floor(0.0) >= 0.005  # a silent room still needs real sound
print("ok")
`
	out, err := exec.Command(py, "-c", script, stubs, dir).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("%v\n%s", err, out)
	}
}
