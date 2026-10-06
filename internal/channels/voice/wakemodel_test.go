package voice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// fakeWakeReady makes dir look like an installed openWakeWord: the helper
// and a Python that imports it (New only asks; nothing is started).
func fakeWakeReady(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in Python is a shell script")
	}
	dir := t.TempDir()
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
	if !wakeReady(dir) {
		t.Fatal("the stand-in openWakeWord isn't ready")
	}
	return dir
}

// newQuiet builds a channel for Mirrin that speaks through a command, so no
// system voice is looked up.
func newQuiet(t *testing.T, v config.Voice) *Channel {
	t.Helper()
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("ELEVENLABS_API_KEY", "")
	v.TTSCommand = "cat >/dev/null"
	return New(v, "Mirrin", t.TempDir())
}

// With no "Hey Mirrin" model in the voice folder, Mirrin used to get the
// on-device detector anyway, running openWakeWord's "hey jarvis" model in
// its place. He is now heard the way the other personas are, in what is
// transcribed, and a model in the folder is still used.
func TestMirrinWithoutAModelIsHeardInTheTranscript(t *testing.T) {
	dir := fakeWakeReady(t)
	for _, engine := range []string{"auto", "openwakeword"} {
		c := newQuiet(t, config.Voice{WakeWord: "mirrin", WakeEngine: engine, KokoroDir: dir})
		if c.wake != nil {
			t.Fatalf("%s: on-device detector with model %q, and no model is here", engine, c.wake.model)
		}
		if !strings.HasPrefix(c.WakeEngine(), "transcribe-and-match") {
			t.Fatalf("%s: wake engine %q", engine, c.WakeEngine())
		}
	}

	model := filepath.Join(dir, heyMirrinFile)
	if err := os.WriteFile(model, []byte("the owner's model"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newQuiet(t, config.Voice{WakeWord: "mirrin", KokoroDir: dir})
	if c.wake == nil || c.wake.model != model {
		t.Fatalf("a model in the voice folder isn't used: %+v", c.wake)
	}
	if b, _ := os.ReadFile(model); string(b) != "the owner's model" {
		t.Fatalf("the model in the folder changed: %q", b)
	}

	// Another name never borrows Mirrin's model.
	if c := newQuiet(t, config.Voice{WakeWord: "juno", WakeEngine: "openwakeword", KokoroDir: dir}); c.wake != nil {
		t.Fatalf("Juno got a detector: %q", c.wake.model)
	}
}

func TestWakeModelIsOnlyClaimedWhenItIsThere(t *testing.T) {
	dir := t.TempDir()
	mirrin := config.Voice{WakeWord: "mirrin", KokoroDir: dir}
	if HasWakeModel(mirrin) || HasWakeModel(config.Voice{WakeWord: "mirrin"}) {
		t.Fatal("a model is claimed with none in the voice folder")
	}
	if why := wakeByTranscript(config.Voice{WakeWord: "juno", KokoroDir: dir}); why != "there's no wake-word model for this name" {
		t.Fatalf("Juno: %q", why)
	}
	if !HasWakeModel(config.Voice{WakeWord: "juno", WakeModel: "/models/hey_juno.onnx"}) {
		t.Fatal("a model set in wake_model is the owner's to use")
	}
	if err := os.WriteFile(filepath.Join(dir, heyMirrinFile), []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasWakeModel(mirrin) || wakeByTranscript(mirrin) != "" {
		t.Fatal("the model in the voice folder isn't used")
	}
	if HasWakeModel(config.Voice{WakeWord: "juno", KokoroDir: dir}) {
		t.Fatal("Juno borrowed Mirrin's model")
	}
	if WakeModelPath(dir, "") != filepath.Join(dir, heyMirrinFile) || WakeModelPath(dir, "hey_jarvis") != "hey_jarvis" {
		t.Fatal("wrong model path")
	}

	// A persona's own model counts once it is in the folder, or given by path.
	if WakeModelAvailable(dir, "hey_nova.onnx") || WakeModelAvailable(dir, "") || WakeModelAvailable("", "hey_nova.onnx") {
		t.Fatal("a missing model counts as here")
	}
	nova := filepath.Join(dir, "hey_nova.onnx")
	if err := os.WriteFile(nova, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !WakeModelAvailable(dir, "hey_nova.onnx") || !WakeModelAvailable("", nova) {
		t.Fatal("a model in the folder doesn't count")
	}
}

// The Listening row says, as an OK row, when the name is heard in what is
// transcribed and why; with a model, or when the owner chose transcribing,
// it says nothing more.
func TestListeningSaysWhenTheNameIsTranscribed(t *testing.T) {
	dir := t.TempDir()
	juno := config.Voice{WakeWord: "juno", KokoroDir: dir}
	if got := listeningFor(juno); got != `listening for "Hey Juno" in what I transcribe, a beat slower: there's no wake-word model for this name` {
		t.Fatalf("Juno: %q", got)
	}
	juno.WakeEngine = "transcribe"
	if got := listeningFor(juno); got != `listening for "Hey Juno"` {
		t.Fatalf("Juno, transcribing by choice: %q", got)
	}
	mirrin := config.Voice{WakeWord: "mirrin", KokoroDir: dir}
	if err := os.WriteFile(filepath.Join(dir, heyMirrinFile), []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listeningFor(mirrin); got != `listening for "Hey Mirrin"` {
		t.Fatalf("Mirrin with his model: %q", got)
	}
}

// runWakeSetup runs voice setup for v with every step succeeding and the
// wake step doing what wake does, and returns the wake step.
func runWakeSetup(t *testing.T, v config.Voice, wake func(ctx context.Context, dir string, w io.Writer) error) SetupStep {
	t.Helper()
	r, _ := runWakeSetupSaid(t, v, wake)
	return r
}

// runWakeSetupSaid is runWakeSetup, with what setup said along the way.
func runWakeSetupSaid(t *testing.T, v config.Voice, wake func(ctx context.Context, dir string, w io.Writer) error) (SetupStep, string) {
	t.Helper()
	t.Setenv("MIRRIN_HOME", t.TempDir())
	steps := SetupSteps{
		LookPath: func(bin string) (string, error) { return "/usr/local/bin/" + bin, nil },
		Language: func() string { return "en" },
		Whisper: func(_ context.Context, dir, model string, _ io.Writer) (string, error) {
			return filepath.Join(dir, "ggml-"+model+".bin"), nil
		},
		Kokoro: func(context.Context, string, io.Writer) error { return nil },
		Wake:   wake,
		OS:     "darwin",
	}
	var said bytes.Buffer
	results, err := RunSetup(context.Background(), &v, "Mirrin", t.TempDir(), &said, steps, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Label == StepWake {
			return r, said.String()
		}
	}
	t.Fatalf("no wake step in %+v", results)
	return SetupStep{}, ""
}

// fakeVenv is a voice folder whose Python only notes how it was called (in
// the file calls), on a PATH without uv, so SetupWake runs for real and
// installs and downloads nothing.
func fakeVenv(t *testing.T) (dir, calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in Python is a shell script")
	}
	dir = t.TempDir()
	calls = filepath.Join(t.TempDir(), "calls")
	py := filepath.Join(dir, "venv", pyBin())
	if err := os.MkdirAll(filepath.Dir(py), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(py, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >>'"+calls+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	return dir, calls
}

// errWakeInstall is how SetupWake fails without the internet.
var errWakeInstall = errors.New("couldn't install openWakeWord; check the internet connection (exit status 1)\nthe whole pip log")

func failWake(context.Context, string, io.Writer) error { return errWakeInstall }

// SetupWake said the name was heard in what is transcribed whenever the
// build had no model, even with the owner's own model set up for the name,
// so the summary row contradicted it. It now only says openWakeWord is
// ready, and the step note says how the name is heard. It also fetched
// openWakeWord's "hey jarvis" example model (CC BY-NC-SA), which nothing
// uses now.
func TestSetupWakeLeavesHowTheNameIsHeardToTheStep(t *testing.T) {
	dir, calls := fakeVenv(t)
	nova := filepath.Join(dir, "hey_nova.onnx")
	if err := os.WriteFile(nova, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, said := runWakeSetupSaid(t, config.Voice{WakeWord: "nova", WakeModel: nova, KokoroDir: dir}, SetupWake)
	if !r.OK || r.Notes != `on-device detector for "Hey Nova"` {
		t.Fatalf("wake step %+v", r)
	}
	if !strings.Contains(said, "openWakeWord is ready") || strings.Contains(said, "transcribe") || strings.Contains(said, "without") {
		t.Fatalf("setup said:\n%s", said)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "jarvis") || !strings.Contains(string(b), "download_models(model_names=['feature-models-only'])") {
		t.Fatalf("Python was asked:\n%s", b)
	}
}

// Nothing listens with openWakeWord while no model is there for the name,
// so an install that fails is an OK step with the plain line. With a model
// that needs it, it is still a failure.
func TestAFailedWakeInstallFailsOnlyWhenAModelNeedsIt(t *testing.T) {
	dir := t.TempDir()
	r, said := runWakeSetupSaid(t, config.Voice{WakeWord: "juno", KokoroDir: dir}, failWake)
	if want := `there's no wake-word model for this name, so I listen for "Hey Juno" in what I transcribe, a beat slower (docs/wake-word.md shows how to train your own)`; !r.OK || r.Notes != want {
		t.Fatalf("Juno's wake step %+v", r)
	}
	if strings.Contains(said, "didn't work") || strings.Contains(said, "pip log") ||
		!strings.Contains(said, "openWakeWord didn't install (couldn't install openWakeWord; check the internet connection (exit status 1)). Nothing uses it until there's a wake-word model") {
		t.Fatalf("setup said:\n%s", said)
	}

	nova := filepath.Join(dir, "hey_nova.onnx")
	if err := os.WriteFile(nova, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, said = runWakeSetupSaid(t, config.Voice{WakeWord: "nova", WakeModel: nova, KokoroDir: dir}, failWake)
	if r.OK || !strings.HasPrefix(r.Notes, "the detector didn't install") || !strings.Contains(said, "That didn't work") {
		t.Fatalf("Nova's wake step %+v, setup said:\n%s", r, said)
	}
}

// A model the owner keeps in the voice folder is the on-device detector
// after setup, in any build.
func TestSetupKeepsAModelAlreadyHere(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, heyMirrinFile), []byte("the owner's model"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runWakeSetup(t, config.Voice{WakeWord: "mirrin", KokoroDir: dir}, func(context.Context, string, io.Writer) error { return nil })
	if !r.OK || r.Notes != `on-device detector for "Hey Mirrin"` {
		t.Fatalf("wake step %+v", r)
	}
}
