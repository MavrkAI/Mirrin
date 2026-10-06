package voice

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// fakeWakeDetector makes dir look like an installed openWakeWord: the
// helper script and a Python that imports anything (New doesn't start it).
func fakeWakeDetector(t *testing.T, dir string) {
	t.Helper()
	py := filepath.Join(dir, "venv", pyBin())
	if err := os.MkdirAll(filepath.Dir(py), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(py, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wake_helper.py"), wakeScript, 0o644); err != nil {
		t.Fatal(err)
	}
}

// With hey_maverick.onnx missing from the voice folder, the detector fell
// back to openWakeWord's stock "hey_jarvis" model, so Mirrin woke to another
// name. Without its model, "mirrin" is heard in the transcript instead.
func TestMirrinWithoutItsModelIsHeardInTheTranscript(t *testing.T) {
	stubSayVoices(t, sayList)
	dir := t.TempDir()
	v := config.Voice{WakeWord: "mirrin", KokoroDir: dir}
	if HasWakeModel(v) {
		t.Error("with no model in the voice folder, the detector still claims one for mirrin")
	}
	if got := WakeModelPath(dir, ""); got != "" {
		t.Errorf("with no model in the voice folder, the detector listens with %q", got)
	}
	if runtime.GOOS != "windows" { // the fake Python is a shell script
		fakeWakeDetector(t, dir)
		for _, engine := range []string{"auto", "openwakeword"} {
			v.WakeEngine = engine
			if c := New(v, "Mirrin", t.TempDir()); c.wake != nil {
				t.Errorf("wake_engine %s started the detector with %q and no model", engine, c.wake.model)
			}
		}
	}

	model := filepath.Join(dir, "hey_mirrin.onnx")
	if err := os.WriteFile(model, []byte("onnx"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasWakeModel(v) || WakeModelPath(dir, "") != model {
		t.Errorf("with the model installed: HasWakeModel %v, path %q", HasWakeModel(v), WakeModelPath(dir, ""))
	}
	if runtime.GOOS != "windows" {
		v.WakeEngine = "auto"
		switch c := New(v, "Mirrin", t.TempDir()); {
		case c.wake == nil:
			t.Error("with the model installed, the detector didn't start")
		case c.wake.model != model:
			t.Errorf("with the model installed, the detector listens with %q", c.wake.model)
		}
	}

	// Other names need a model of their own; one named in wake_model is used as given.
	if HasWakeModel(config.Voice{WakeWord: "juno", KokoroDir: dir}) {
		t.Error("the mirrin model was taken as a detector for juno")
	}
	if !HasWakeModel(config.Voice{WakeWord: "juno", WakeModel: "hey_juno.onnx", KokoroDir: t.TempDir()}) || WakeModelPath(dir, "hey_juno.onnx") != "hey_juno.onnx" {
		t.Error("a model set in wake_model isn't used")
	}
}

// The helper itself listened with openWakeWord's stock "hey_jarvis" model
// when it was given none. Now it says it has no model, and stops.
func TestWakeHelperWithoutAModelSaysSo(t *testing.T) {
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
		"openwakeword/model.py":    "class Model:\n    def __init__(self, wakeword_models, **kw):\n        print('listening with', wakeword_models)\n        raise SystemExit(0)\n",
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
	cmd := exec.Command(py, filepath.Join(dir, "wake_helper.py"))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "WAKE_MODEL=") && !strings.HasPrefix(kv, "PYTHONPATH=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "PYTHONPATH="+stubs)
	out, _ := cmd.CombinedOutput()
	if got := strings.TrimSpace(string(out)); got != "err no wake-word model; run `mirrin voice setup`" {
		t.Fatalf("the helper with no model printed:\n%s", got)
	}
}

// Regression: Nyra's own voice set off her detector while she talked (it
// runs at 0.1 then), cutting her off and hearing her words as the owner's.
// wake_threshold_speaking now reaches the helper.
func TestSpeakingThresholdReachesTheHelper(t *testing.T) {
	w := &wakeHelper{model: "m.onnx", thresh: 0.25, out: "/tmp/x.wav"}
	for _, e := range w.env() {
		if strings.HasPrefix(e, "WAKE_THRESHOLD_SPEAKING=") {
			t.Fatalf("unset, but the helper was told %s", e)
		}
	}
	w.threshSpeaking = 0.6
	found := false
	for _, e := range w.env() {
		found = found || e == "WAKE_THRESHOLD_SPEAKING=0.60"
	}
	if !found {
		t.Fatalf("env %v", w.env())
	}
}
