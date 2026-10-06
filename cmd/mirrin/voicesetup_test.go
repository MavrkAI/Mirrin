package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// fakeVoiceSteps succeed at everything and record what they were asked.
type fakeVoiceSteps struct {
	model   string
	told    int
	missing map[string]bool
	lang    string
	wakeErr error
}

func (f *fakeVoiceSteps) steps() voiceSteps {
	return voiceSteps{
		lookPath: func(bin string) (string, error) {
			if f.missing[bin] {
				return "", errors.New("not found")
			}
			return "/usr/local/bin/" + bin, nil
		},
		language: func() string { return f.lang },
		whisper: func(_ context.Context, dir, model string, _ io.Writer) (string, error) {
			f.model = model
			return filepath.Join(dir, "ggml-"+model+".bin"), nil
		},
		kokoro: func(context.Context, string, io.Writer) error { return nil },
		wake:   func(context.Context, string, io.Writer) error { return f.wakeErr },
		save:   func(*config.Config) error { return nil },
		tell: func(context.Context, *config.Config) (bool, error) {
			f.told++
			return true, nil
		},
	}
}

// Setup used to finish on "voice ready" whatever happened to the wake word.
func TestVoiceSetupSaysWhatDidntWork(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	// A model of the owner's, which needs the detector that didn't install.
	cfg.Channels.Voice.WakeModel = filepath.Join(t.TempDir(), "hey_mirrin.onnx")
	f := &fakeVoiceSteps{wakeErr: errors.New("pip install openwakeword: no matching distribution\n(the whole pip log)")}
	var out bytes.Buffer
	err := runVoiceSetup(context.Background(), cfg, &out, f.steps())
	var code exitCode
	if !errors.As(err, &code) || code != 1 {
		t.Fatalf("err %v: a failed step must not exit 0", err)
	}
	text := out.String()
	for _, want := range []string{"✓ Hearing", "✓ Voice", "✗ Wake word", "no matching distribution", `listen for "Hey Mirrin" in what I transcribe`, "run mirrin voice setup again"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	for _, wrong := range []string{"All set", "on-device detector for"} {
		if strings.Contains(text, wrong) {
			t.Errorf("output claims %q:\n%s", wrong, text)
		}
	}
	// The whole error where it happened; one line of it in the summary.
	if !strings.Contains(text, "That didn't work: pip install openwakeword: no matching distribution\n(the whole pip log)") {
		t.Errorf("the step's whole error isn't shown:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "✗ Wake word") && strings.Contains(line, "whole pip log") {
			t.Errorf("summary line carries the whole log: %q", line)
		}
	}
	if f.told != 1 {
		t.Fatal("what did install wasn't handed to the running twin")
	}
}

// No model is there for another name; setup says so instead of claiming an
// on-device detector.
func TestVoiceSetupIsHonestAboutOtherWakeWords(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	cfg.Channels.Voice.WakeWord = "ava"
	cfg.Channels.Voice.WakeModel = ""
	f := &fakeVoiceSteps{}
	var out bytes.Buffer
	if err := runVoiceSetup(context.Background(), cfg, &out, f.steps()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "on-device detector for") || !strings.Contains(out.String(), `listen for "Hey Ava" in what I transcribe`) {
		t.Fatalf("output:\n%s", out.String())
	}
}

// With no wake-word model for the name, nothing uses openWakeWord, and
// setup exited 1 with "Something above didn't finish" when it didn't
// install. It is now one plain line, and setup is done.
func TestVoiceSetupDoesntFailOverADetectorNothingUses(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	cfg.Channels.Voice.WakeWord = "ava"
	cfg.Channels.Voice.WakeModel = ""
	f := &fakeVoiceSteps{wakeErr: errors.New("pip install openwakeword: no matching distribution\n(the whole pip log)")}
	var out bytes.Buffer
	if err := runVoiceSetup(context.Background(), cfg, &out, f.steps()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{"✓ Wake word", `listen for "Hey Ava" in what I transcribe`, "openWakeWord didn't install", "All set"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	for _, wrong := range []string{"✗", "Something above didn't finish", "whole pip log"} {
		if strings.Contains(out.String(), wrong) {
			t.Errorf("output says %q:\n%s", wrong, out.String())
		}
	}
}

func TestVoiceSetupNamesMissingTools(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	defer func(s string) { setupOS = s }(setupOS)
	setupOS = "darwin"
	cfg := config.Default()
	f := &fakeVoiceSteps{missing: map[string]bool{"sox": true}}
	var out bytes.Buffer
	if err := runVoiceSetup(context.Background(), cfg, &out, f.steps()); err == nil {
		t.Fatal("setup without sox can't be a success")
	}
	if !strings.Contains(out.String(), "✗ Tools") || !strings.Contains(out.String(), "sox: brew install sox") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// Setup always downloaded the English-only model. On a first setup it now
// follows the computer's language: anything but English gets the
// multilingual model, listening for any language (the owner may still talk
// to the twin in English).
func TestVoiceSetupHearsTheOwnersLanguage(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	cfg.Channels.Voice.Language = ""
	f := &fakeVoiceSteps{lang: "fr"}
	var out bytes.Buffer
	if err := runVoiceSetup(context.Background(), cfg, &out, f.steps()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if f.model != "small" || cfg.Channels.Voice.Language != "auto" || !strings.HasSuffix(cfg.Channels.Voice.WhisperModel, "ggml-small.bin") {
		t.Fatalf("model %q, language %q, file %q", f.model, cfg.Channels.Voice.Language, cfg.Channels.Voice.WhisperModel)
	}
	for _, want := range []string{"French", "any language", "channels.voice.language: fr", "channels.voice.language: en", "All set"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// An English speaker keeps the English model; a language set by hand wins.
	for _, c := range []struct{ set, system, model string }{{"", "en", "small.en"}, {"", "", "small.en"}, {"de", "fr", "small"}, {"en", "fr", "small.en"}, {"auto", "", "small"}} {
		cfg := config.Default()
		cfg.Channels.Voice.Language = c.set
		f := &fakeVoiceSteps{lang: c.system}
		_ = runVoiceSetup(context.Background(), cfg, io.Discard, f.steps())
		if f.model != c.model || cfg.Channels.Voice.Language != c.set {
			t.Errorf("set %q, system %q: model %q (language %q), want %q", c.set, c.system, f.model, cfg.Channels.Voice.Language, c.model)
		}
	}
}

// Running setup again on a working English install, on a computer set to
// German, swapped small.en for the multilingual model and forced whisper to
// hear German: an owner who talks to the twin in English could no longer
// be understood. A re-run keeps the hearing that works, and only says how
// to change it.
func TestVoiceSetupKeepsAWorkingInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	cfg := config.Default()
	model := filepath.Join(home, "models", "ggml-small.en.bin")
	_ = os.MkdirAll(filepath.Dir(model), 0o755)
	if err := os.WriteFile(model, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Channels.Voice.WhisperModel = model
	cfg.Channels.Voice.Language = ""
	f := &fakeVoiceSteps{lang: "de"}
	var out bytes.Buffer
	if err := runVoiceSetup(context.Background(), cfg, &out, f.steps()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if f.model != "small.en" || cfg.Channels.Voice.WhisperModel != model || cfg.Channels.Voice.Language != "" {
		t.Fatalf("model %q, file %q, language %q: a working install was changed", f.model, cfg.Channels.Voice.WhisperModel, cfg.Channels.Voice.Language)
	}
	if !strings.Contains(out.String(), "German") || !strings.Contains(out.String(), "channels.voice.language: de") {
		t.Fatalf("no word on how to switch:\n%s", out.String())
	}

	// A multilingual model chosen by hand is left as it is.
	medium := filepath.Join(home, "models", "ggml-medium.bin")
	_ = os.WriteFile(medium, []byte("weights"), 0o644)
	cfg.Channels.Voice.WhisperModel = medium
	f = &fakeVoiceSteps{lang: "de"}
	if err := runVoiceSetup(context.Background(), cfg, io.Discard, f.steps()); err != nil {
		t.Fatal(err)
	}
	if f.model != "" || cfg.Channels.Voice.WhisperModel != medium || cfg.Channels.Voice.Language != "" {
		t.Fatalf("downloaded %q, file %q, language %q", f.model, cfg.Channels.Voice.WhisperModel, cfg.Channels.Voice.Language)
	}
}

// "mirrin voice" joined to a running twin (the menu bar's) never learned
// whether an approval was waiting, so a bare "yes" to it could be thrown
// away as noise.
func TestJoinedVoiceKnowsWhenAnApprovalWaits(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(api.TokenPath(data), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var pending atomic.Int32
	stops := make(chan api.MessageRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_ = json.NewEncoder(w).Encode(api.Status{Name: "Mirrin", Pending: int(pending.Load())})
		case "/message":
			var m api.MessageRequest
			_ = json.NewDecoder(r.Body).Decode(&m)
			stops <- m
			_ = json.NewEncoder(w).Encode(api.MessageResponse{Reply: "OK, I've stopped."})
		}
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.DataDir = data
	cfg.API.Listen = strings.TrimPrefix(srv.URL, "http://")
	c := api.Connect(cfg.API.Listen, cfg.DataDir)
	if c == nil {
		t.Fatal("no connection to the fake twin")
	}
	ch := joinedVoice(cfg, c)
	if ch.Pending == nil || ch.Pending() {
		t.Fatal("nothing waits yet")
	}
	pending.Store(1)
	if !ch.Pending() {
		t.Fatal("an approval waits")
	}
	// "Hey Mirrin, stop" while the running twin works on the last thing
	// said: sent at once, as its own message, which the twin takes before
	// the turn it stops (stopPresented).
	if ch.OnStop == nil {
		t.Fatal("a joined voice session can't stop the twin")
	}
	ch.OnStop()
	select {
	case m := <-stops:
		if m.Channel != "voice" || m.ChatID != "local" || m.Text != "stop" {
			t.Fatalf("stop sent as %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the spoken stop never reached the twin")
	}
	srv.Close()
	if ch.Pending() {
		t.Fatal("a twin that went away has nothing waiting")
	}
}

// Setup run from the menu bar left the running twin hearing with its old
// settings until a restart; it now asks the twin to pick them up.
func TestVoiceSetupHandsTheResultToTheRunningTwin(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(api.TokenPath(data), []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	fail, refuse := false, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/status":
			_, _ = io.WriteString(w, "{}")
		case "/jobs/run":
			if refuse {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":"not_here","message":"This page only opens on the computer Mirrin runs on.","fix":"Open it there, from Mirrin's menu."}`)
				return
			}
			var req struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			jobs = append(jobs, req.Name)
			if fail {
				http.Error(w, `unknown job "voice"`, http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"ok":"voice"}`)
		}
	}))
	defer srv.Close()
	cfg := &config.Config{DataDir: data}
	cfg.API.Listen = strings.TrimPrefix(srv.URL, "http://")
	running, err := tellRunningTwin(context.Background(), cfg)
	if !running || err != nil || len(jobs) != 1 || jobs[0] != "voice" {
		t.Fatalf("running %v, err %v, jobs %q", running, err, jobs)
	}
	fail = true // an older twin that doesn't know the job
	if _, err := tellRunningTwin(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "unknown job") {
		t.Fatalf("err %v", err)
	}
	// A refusal from devices' checks is told in its words, not as raw JSON.
	refuse = true
	if _, err := tellRunningTwin(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "only opens on the computer") || strings.Contains(err.Error(), "{") {
		t.Fatalf("a refusal: %v", err)
	}
	refuse = false
	srv.Close()
	if running, err := tellRunningTwin(context.Background(), cfg); running || err != nil {
		t.Fatalf("no twin: running %v, err %v", running, err)
	}
}
