package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/transcribe"
)

// TestMain lets the test binary stand in for whisper.cpp: with FAKE_WHISPER
// set it is whisper-server when given --port, else whisper-cli.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_WHISPER") == "1" {
		fakeWhisper(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func fakeWhisper(args []string) {
	port, prefix := "", ""
	for i, a := range args {
		if a == "--port" && i+1 < len(args) {
			port = args[i+1]
		}
		if a == "--request-path" && i+1 < len(args) {
			prefix = args[i+1]
		}
	}
	if port == "" {
		fmt.Println(" heard by cli [BLANK_AUDIO]")
		os.Exit(0)
	}
	// Like whisper-server, every route sits under --request-path.
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix+"/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("POST "+prefix+"/inference", func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("FAKE_WHISPER_HANG") == "1" {
			<-r.Context().Done() // loaded, but never answers
			return
		}
		if os.Getenv("FAKE_WHISPER_REFUSE") == "1" {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to read WAV file"})
			return
		}
		if _, _, err := r.FormFile("file"); err != nil {
			http.Error(w, "no file", http.StatusBadRequest)
			return
		}
		text := fmt.Sprintf(" heard by server %d, prompt %s\n", os.Getpid(), r.FormValue("prompt"))
		_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
	})
	_ = http.ListenAndServe("127.0.0.1:"+port, mux)
	os.Exit(0)
}

// fakeWhisperBins puts whisper-cli and whisper-server (both this test
// binary) in a directory, as Homebrew does.
func fakeWhisperBins(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, n := range []string{"whisper-cli", "whisper-server"} {
		if err := os.Symlink(self, filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FAKE_WHISPER", "1")
	return dir
}

// whisper-cli loaded the model from disk for every utterance. Now a
// whisper-server keeps it loaded; until it is ready, or when it dies, an
// utterance goes to whisper-cli instead of waiting or failing.
func TestWhisperServerKeepsTheModelLoaded(t *testing.T) {
	bins := fakeWhisperBins(t)
	data := t.TempDir()
	model := filepath.Join(data, "ggml-small.en.bin")
	_ = os.WriteFile(model, []byte("weights"), 0o644)
	wav := filepath.Join(data, "voice-in.wav")
	_ = os.WriteFile(wav, []byte("RIFF-audio"), 0o644)
	c := New(config.Voice{WhisperBin: filepath.Join(bins, "whisper-cli"), WhisperModel: model, KokoroDir: data, TTSCommand: "true"}, "Mirrin", data)
	c.out = io.Discard
	if c.whisper == nil || c.whisper.bin != filepath.Join(bins, "whisper-server") {
		t.Fatalf("whisper-server next to whisper-cli wasn't found: %+v", c.whisper)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Not started yet: whisper-cli answers, and the server starts meanwhile.
	if got, err := c.Transcribe(ctx, wav); err != nil || got != "heard by cli" {
		t.Fatalf("before the server: %q %v", got, err)
	}
	life, stop := context.WithCancel(ctx)
	c.whisper.warm(life)
	waitFor(t, "the server", c.whisper.running)
	first, err := c.Transcribe(ctx, wav)
	if err != nil || !strings.HasPrefix(first, "heard by server") || !strings.HasSuffix(first, "prompt Hey Mirrin.") {
		t.Fatalf("server: %q %v", first, err)
	}
	second, _ := c.Transcribe(ctx, wav)
	if second != first {
		t.Fatalf("a second process answered: %q then %q", first, second)
	}

	// Only the twin knows where it listens: the routes sit under a random
	// path, so another program on this computer can't swap the model out.
	c.whisper.mu.Lock()
	base := c.whisper.base
	c.whisper.mu.Unlock()
	u, err := url.Parse(base)
	if err != nil || len(strings.Trim(u.Path, "/")) < 16 {
		t.Fatalf("server at %q: no private path", base)
	}
	for _, p := range []string{"/load", "/inference"} {
		resp, err := http.Post("http://"+u.Host+p, "text/plain", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s answered %d without the private path", p, resp.StatusCode)
		}
	}

	// The server dies: this utterance goes to whisper-cli, and a new server
	// is started for the next ones.
	c.whisper.mu.Lock()
	_ = c.whisper.cmd.Process.Kill()
	exited := c.whisper.exited
	c.whisper.mu.Unlock()
	<-exited
	if got, err := c.Transcribe(ctx, wav); err != nil || got != "heard by cli" {
		t.Fatalf("after the server died: %q %v", got, err)
	}
	waitFor(t, "a new server", c.whisper.running)
	if got, _ := c.Transcribe(ctx, wav); !strings.HasPrefix(got, "heard by server") || got == first {
		t.Fatalf("new server: %q", got)
	}

	// It ends with the channel, leaving nothing behind.
	c.whisper.mu.Lock()
	exited = c.whisper.exited
	c.whisper.mu.Unlock()
	stop()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the server outlived the channel")
	}
	waitFor(t, "the pid file removed", func() bool {
		left, _ := filepath.Glob(filepath.Join(data, "whisper-server-*.pid"))
		return len(left) == 0
	})
}

// A twin that crashed left its whisper-server running; the next one ends it
// before starting its own, so a crash loop never piles up loaded models. A
// server whose twin is alive (the menu bar's, while a terminal session
// starts) is left alone.
func TestStaleWhisperServerIsEnded(t *testing.T) {
	bins := fakeWhisperBins(t)
	data := t.TempDir()
	// A process that has ended stands in for the crashed twin.
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Skip("no true(1)")
	}
	launch := func(owner int) (*whisperServer, chan struct{}) {
		s := newWhisperServer(filepath.Join(bins, "whisper-server"), "m", "", data)
		s.pidFile = filepath.Join(data, fmt.Sprintf("whisper-server-%d-1.pid", owner))
		if _, err := s.launch(context.Background()); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s, s.exited
	}
	orphan, orphanExited := launch(dead.Process.Pid)
	defer orphan.stop()
	inUse, inUseExited := launch(os.Getppid()) // a live twin's
	defer inUse.stop()

	reapStale(data) // what the next twin does first
	select {
	case <-orphanExited:
	case <-time.After(5 * time.Second):
		t.Fatal("the crashed twin's server is still running")
	}
	select {
	case <-inUseExited:
		t.Fatal("a live twin's server was ended")
	case <-time.After(300 * time.Millisecond):
	}
	// An id that now belongs to something else is left alone.
	_ = os.WriteFile(filepath.Join(data, fmt.Sprintf("whisper-server-%d-2.pid", dead.Process.Pid)), []byte(fmt.Sprint(os.Getpid())), 0o644)
	reapStale(data)
}

func TestWhisperServerCanBeTurnedOff(t *testing.T) {
	bins := fakeWhisperBins(t)
	t.Setenv("MIRRIN_WHISPER_SERVER", "0")
	c := New(config.Voice{WhisperBin: filepath.Join(bins, "whisper-cli"), WhisperModel: "m", KokoroDir: t.TempDir(), TTSCommand: "true"}, "Mirrin", t.TempDir())
	if c.whisper != nil {
		t.Fatal("MIRRIN_WHISPER_SERVER=0 still ran a server")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The server answered but couldn't hear this clip (internal/transcribe's
// AnswerError): whisper-cli tries it, and the server keeps running, since it
// did nothing wrong.
func TestWhisperServerThatCantHearAClipIsKept(t *testing.T) {
	bins := fakeWhisperBins(t)
	t.Setenv("FAKE_WHISPER_REFUSE", "1")
	data := t.TempDir()
	wav := filepath.Join(data, "voice-in.wav")
	_ = os.WriteFile(wav, []byte("RIFF-audio"), 0o644)
	c := New(config.Voice{WhisperBin: filepath.Join(bins, "whisper-cli"), WhisperModel: filepath.Join(data, "m.bin"), KokoroDir: data, TTSCommand: "true"}, "Mirrin", data)
	c.out = io.Discard
	life, stop := context.WithCancel(context.Background())
	defer stop()
	c.whisper.warm(life)
	waitFor(t, "the server", c.whisper.running)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if got, err := c.Transcribe(ctx, wav); err != nil || got != "heard by cli" {
		t.Fatalf("got %q, %v", got, err)
	}
	if !c.whisper.running() {
		t.Fatal("a server that only couldn't hear one clip was stopped")
	}
}

// A whisper-server that hung made every utterance wait a minute and then
// fail, with no whisper-cli fallback and no warning. Now the server gets a
// short deadline of its own; when it misses it, whisper-cli hears this
// utterance and the server is rested.
func TestHungWhisperServerFallsBackToCLI(t *testing.T) {
	bins := fakeWhisperBins(t)
	t.Setenv("FAKE_WHISPER_HANG", "1")
	data := t.TempDir()
	wav := filepath.Join(data, "voice-in.wav")
	_ = os.WriteFile(wav, []byte("RIFF-audio"), 0o644)
	c := New(config.Voice{WhisperBin: filepath.Join(bins, "whisper-cli"), WhisperModel: filepath.Join(data, "m.bin"), KokoroDir: data, TTSCommand: "true"}, "Mirrin", data)
	c.out = io.Discard
	life, stop := context.WithCancel(context.Background())
	defer stop()
	c.whisper.requestTimeout = 200 * time.Millisecond
	c.whisper.warm(life)
	waitFor(t, "the server", c.whisper.running)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	got, err := c.Transcribe(ctx, wav)
	if err != nil || got != "heard by cli" {
		t.Fatalf("got %q, %v after %s", got, err, time.Since(start).Round(time.Millisecond))
	}
	c.whisper.mu.Lock()
	rested := time.Now().Before(c.whisper.downUntil) && c.whisper.cmd == nil
	c.whisper.mu.Unlock()
	if !rested {
		t.Fatal("the hung server wasn't stopped and rested")
	}
}

// The microphone and chat voice notes read channels.voice.language the same
// way (transcribe.SpokenLanguage): unset means English, anything set is
// used as it is.
func TestTheMicrophoneHearsTheLanguageVoiceNotesDo(t *testing.T) {
	for _, lang := range []string{"", "auto", "fr"} {
		if got := hearing(config.Voice{Language: lang}, "").Language; got != transcribe.SpokenLanguage(lang) {
			t.Errorf("%q: the microphone hears %q, voice notes %q", lang, got, transcribe.SpokenLanguage(lang))
		}
	}
	if hearing(config.Voice{}, "").Language != "en" {
		t.Fatal("unset should mean English")
	}
}

// MIRRIN_WHISPER_SERVER=0 works for an installed service too, which gets no
// environment of the owner's: saved in secrets.env, like MIRRIN_WHISPER_URL.
func TestTheWhisperServerCanBeTurnedOffInTheSecretsFile(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("MIRRIN_WHISPER_SERVER", "")
	if serverDisabled() {
		t.Fatal("nothing set")
	}
	if err := config.SaveSecrets(map[string]string{"MIRRIN_WHISPER_SERVER": "0"}); err != nil {
		t.Fatal(err)
	}
	if !serverDisabled() {
		t.Fatal("MIRRIN_WHISPER_SERVER=0 in secrets.env was ignored")
	}
}
