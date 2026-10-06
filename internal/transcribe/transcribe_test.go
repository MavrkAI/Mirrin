package transcribe

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// wav16k is a short silent 16 kHz mono WAV, what whisper reads directly.
func wav16k(samples int) []byte {
	b := make([]byte, 44+2*samples)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*samples))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*samples))
	return b
}

// script writes an executable shell script and returns its path.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses shell scripts as stand-in tools")
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// noServer keeps a test off whatever listens on the default port.
func noServer(o Options) Options {
	o.Server = "off"
	return o
}

func TestNothingInstalledSaysWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	o := noServer(Options{Bin: filepath.Join(dir, "no-whisper-cli"), Model: filepath.Join(dir, "model.bin"), TempDir: dir})
	var un *UnavailableError
	if err := Check(context.Background(), o); !errors.As(err, &un) || un.Missing != "whisper-cli" {
		t.Fatalf("no whisper-cli: %v", err)
	}
	o.Bin = script(t, dir, "whisper-cli", "echo hi\n")
	if err := Check(context.Background(), o); !errors.As(err, &un) || un.Missing != "the whisper model" {
		t.Fatalf("no model: %v", err)
	}
	if _, err := Transcribe(context.Background(), wav16k(10), "audio/wav", o); !errors.As(err, &un) {
		t.Fatalf("transcribe without a model: %v", err)
	}
	if !strings.Contains(un.Error(), "isn't set up") {
		t.Fatalf("error reads %q", un.Error())
	}
}

func TestWhisperCLI(t *testing.T) {
	dir, tmp := t.TempDir(), t.TempDir()
	model := filepath.Join(dir, "ggml-small.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := filepath.Join(dir, "args")
	bin := script(t, dir, "whisper-cli", `echo "$@" > `+args+`
for a in "$@"; do case "$a" in *.wav) head -c 4 "$a" >> `+args+`;; esac; done
printf '[BLANK_AUDIO]\n Remind me to call Mum\n at five. (laughs)\n'
`)
	o := noServer(Options{Bin: bin, Model: model, Language: "en", Prompt: "Mum, Mirrin.", TempDir: tmp})
	got, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Remind me to call Mum at five." {
		t.Fatalf("transcript %q", got)
	}
	seen, _ := os.ReadFile(args)
	for _, want := range []string{"-m " + model, "-nt", "-l en", "--prompt Mum, Mirrin.", "RIFF"} {
		if !strings.Contains(string(seen), want) {
			t.Errorf("whisper-cli got %q, missing %q", seen, want)
		}
	}
	if !strings.Contains(string(seen), tmp) {
		t.Errorf("the audio was not kept in the data folder's temp dir: %q", seen)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("temporary audio left behind: %v", left)
	}
}

func TestVoiceNotesAreConvertedFirst(t *testing.T) {
	dir, tmp := t.TempDir(), t.TempDir()
	model := filepath.Join(dir, "model.bin")
	_ = os.WriteFile(model, []byte("model"), 0o600)
	fixture := filepath.Join(dir, "fixture.wav")
	_ = os.WriteFile(fixture, wav16k(1600), 0o600)
	bin := script(t, dir, "whisper-cli", "echo ' Hello.'\n")
	calls := filepath.Join(dir, "calls")
	// Both converters write the fixture to their output argument.
	ffmpeg := script(t, dir, "ffmpeg", `echo "ffmpeg $@" >> `+calls+`
for last in "$@"; do :; done
cp `+fixture+` "$last"
`)
	sox := script(t, dir, "sox", `echo "sox $@" >> `+calls+`
for a in "$@"; do case "$a" in *.wav) cp `+fixture+` "$a";; esac; done
`)
	ogg := []byte("OggS\x00\x02opus voice note")
	o := noServer(Options{Bin: bin, Model: model, TempDir: tmp, FFmpeg: ffmpeg, Sox: sox, MaxSeconds: 900})
	if got, err := Transcribe(context.Background(), ogg, "audio/ogg; codecs=opus", o); err != nil || got != "Hello." {
		t.Fatalf("with ffmpeg: %q %v", got, err)
	}
	o.FFmpeg = filepath.Join(dir, "no-ffmpeg")
	if got, err := Transcribe(context.Background(), ogg, "audio/ogg", o); err != nil || got != "Hello." {
		t.Fatalf("with sox: %q %v", got, err)
	}
	seen, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(seen)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "-ar 16000 -ac 1") || !strings.Contains(lines[0], "-t 900") ||
		!strings.Contains(lines[1], ".opus -r 16000 -c 1") || !strings.Contains(lines[1], "trim 0 900") {
		t.Fatalf("converter calls: %q", lines)
	}
	o.Sox = filepath.Join(dir, "no-sox")
	if _, err := Transcribe(context.Background(), ogg, "audio/ogg", o); !errors.Is(err, ErrNoConverter) {
		t.Fatalf("no converter: %v", err)
	}
	failing := script(t, dir, "ffmpeg-broken", "echo 'Invalid data found when processing input' >&2\nexit 1\n")
	o.FFmpeg = failing
	if _, err := Transcribe(context.Background(), ogg, "audio/ogg", o); err == nil || !strings.Contains(err.Error(), "Invalid data") {
		t.Fatalf("broken audio: %v", err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("temporary audio left behind: %v", left)
	}
}

// whisperPage is the home page whisper.cpp's server shows (abridged).
const whisperPage = `<html><head><title>Whisper.cpp Server</title></head><body><h1>Whisper.cpp Server</h1>
<h2>/inference</h2><pre>curl 127.0.0.1:8080/inference -F response_format="json"</pre></body></html>`

// A whisper server that is running is used before whisper-cli, and only
// one that says it is whisper.cpp's.
func TestRunningWhisperServerIsPreferred(t *testing.T) {
	var posted []byte
	var fields = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, whisperPage)
		case "/inference":
			f, _, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			posted, _ = io.ReadAll(f)
			for _, k := range []string{"response_format", "language", "prompt"} {
				fields[k] = r.FormValue(k)
			}
			_, _ = io.WriteString(w, `{"text":" Book a table for two.\n"}`)
		}
	}))
	defer srv.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inference" {
			t.Error("audio was posted to a server that isn't whisper")
		}
		_, _ = io.WriteString(w, "my dev server")
	}))
	defer other.Close()
	// A page that only talks about whisper (a web app for it, say) is not
	// the server either.
	webUI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inference" {
			t.Error("audio was posted to a page that only mentions whisper")
		}
		_, _ = io.WriteString(w, "<title>Whisper Web UI</title> transcribe with OpenAI Whisper")
	}))
	defer webUI.Close()

	dir, tmp := t.TempDir(), t.TempDir()
	model := filepath.Join(dir, "model.bin")
	_ = os.WriteFile(model, []byte("model"), 0o600)
	bin := script(t, dir, "whisper-cli", "echo ' from the cli'\n")
	o := Options{Server: srv.URL, Bin: bin, Model: model, Language: "en", Prompt: "Nobu.", TempDir: tmp}
	got, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o)
	if err != nil || got != "Book a table for two." {
		t.Fatalf("server: %q %v", got, err)
	}
	if !strings.HasPrefix(string(posted), "RIFF") || fields["response_format"] != "json" || fields["language"] != "en" || fields["prompt"] != "Nobu." {
		t.Fatalf("posted %d bytes, fields %v", len(posted), fields)
	}
	for _, notWhisper := range []string{other.URL, webUI.URL} {
		o.Server = notWhisper
		if got, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o); err != nil || got != "from the cli" {
			t.Fatalf("not whisper, so the cli: %q %v", got, err)
		}
	}
	srv.Close()
	forget(srv.URL)
	o.Server = srv.URL
	if got, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o); err != nil || got != "from the cli" {
		t.Fatalf("server gone, so the cli: %q %v", got, err)
	}
}

func TestServerURL(t *testing.T) {
	for in, want := range map[string]string{"": DefaultServer, "off": "", " http://10.0.0.2:9000/ ": "http://10.0.0.2:9000"} {
		if got := serverURL(in); got != want {
			t.Errorf("serverURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeCLI is a whisper-cli that says words, with a model file, and records
// its arguments in args.
func fakeCLI(t *testing.T, words string) (bin, model, args string) {
	t.Helper()
	dir := t.TempDir()
	model = filepath.Join(dir, "ggml-large-v3.bin")
	_ = os.WriteFile(model, []byte("model"), 0o600)
	args = filepath.Join(dir, "args")
	bin = script(t, dir, "whisper-cli", `echo "$@" >> `+args+"\necho ' "+words+"'\n")
	return bin, model, args
}

// A whisper server that is running but can't transcribe (it fails, or
// something else answers on its port) is set aside and whisper-cli answers
// instead, so voice notes keep working.
func TestAFailingWhisperServerFallsBackToTheCLI(t *testing.T) {
	var inference atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inference" {
			inference.Add(1)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, whisperPage)
	}))
	defer srv.Close()
	bin, model, _ := fakeCLI(t, "Pick up milk.")
	o := Options{Server: srv.URL, Bin: bin, Model: model, TempDir: t.TempDir()}
	for i := 0; i < 2; i++ {
		got, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o)
		if err != nil || got != "Pick up milk." {
			t.Fatalf("voice note %d: %q %v", i+1, got, err)
		}
	}
	if n := inference.Load(); n != 1 {
		t.Fatalf("the failing server was asked %d times; once, then set aside", n)
	}
	if err := Check(context.Background(), o); err != nil {
		t.Fatalf("whisper-cli still works: %v", err)
	}

	// With no whisper-cli, the server's failure is what is reported.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "whisper.cpp")
		if r.URL.Path == "/inference" {
			http.Error(w, "failed to process audio", http.StatusInternalServerError)
		}
	}))
	defer down.Close()
	o = Options{Server: down.URL, Bin: filepath.Join(t.TempDir(), "no-whisper-cli"), TempDir: t.TempDir()}
	if _, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o); err == nil || !strings.Contains(err.Error(), "whisper server") {
		t.Fatalf("server failed, no cli: %v", err)
	}
}

// Audio posted to the local server is never sent on by a redirect.
func TestAudioIsNeverRedirectedOffTheComputer(t *testing.T) {
	var away atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		away.Add(1)
		_, _ = io.WriteString(w, `{"text":"stolen"}`)
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "whisper.cpp")
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	bin, model, _ := fakeCLI(t, "Hello.")
	o := Options{Server: srv.URL, Bin: bin, Model: model, TempDir: t.TempDir()}
	if got, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o); err != nil || got != "Hello." {
		t.Fatalf("%q %v", got, err)
	}
	if away.Load() != 0 {
		t.Fatal("audio followed a redirect off the whisper server")
	}
}

// Without a language set, whisper detects it (it would assume English), except
// with an English-only model.
func TestTheSpokenLanguageIsDetected(t *testing.T) {
	bin, model, args := fakeCLI(t, "Hola.")
	o := noServer(Options{Bin: bin, Model: model, TempDir: t.TempDir()})
	if _, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o); err != nil {
		t.Fatal(err)
	}
	english := filepath.Join(filepath.Dir(model), "ggml-small.en.bin")
	_ = os.WriteFile(english, []byte("model"), 0o600)
	o.Model = english
	if _, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o); err != nil {
		t.Fatal(err)
	}
	seen, _ := os.ReadFile(args)
	lines := strings.Split(strings.TrimSpace(string(seen)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "-l auto") || strings.Contains(lines[1], "-l ") {
		t.Fatalf("whisper-cli calls: %q", lines)
	}
	for model, want := range map[string]bool{"ggml-base.en.bin": true, "ggml-medium.en-q5_0.bin": true, "ggml-large-v3-turbo.bin": false, "ggml-tiny.bin": false} {
		if englishOnly("/models/"+model) != want {
			t.Errorf("englishOnly(%s) = %v", model, !want)
		}
	}

	var lang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "whisper.cpp")
		if r.URL.Path == "/inference" {
			lang = r.FormValue("language")
			_, _ = io.WriteString(w, `{"text":"Hola."}`)
		}
	}))
	defer srv.Close()
	o.Server = srv.URL
	if _, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o); err != nil || lang != "auto" {
		t.Fatalf("server asked for language %q: %v", lang, err)
	}
}

// ffmpeg may open a voice note only as audio, never as a playlist or a list
// of other files; sox is used only for audio it can read.
func TestConvertersOnlyOpenAudio(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	fixture := filepath.Join(dir, "fixture.wav")
	_ = os.WriteFile(fixture, wav16k(1600), 0o600)
	ffmpeg := script(t, dir, "ffmpeg", `echo "ffmpeg $@" >> `+calls+`
for last in "$@"; do :; done
cp `+fixture+` "$last"
`)
	sox := script(t, dir, "sox", `echo "sox $@" >> `+calls+`
for a in "$@"; do case "$a" in *.wav) cp `+fixture+` "$a";; esac; done
`)
	bin, model, _ := fakeCLI(t, "Hello.")
	o := noServer(Options{Bin: bin, Model: model, TempDir: t.TempDir(), FFmpeg: ffmpeg, Sox: sox})
	if _, err := Transcribe(context.Background(), []byte("OggS voice"), "audio/ogg", o); err != nil {
		t.Fatal(err)
	}
	seen, _ := os.ReadFile(calls)
	if !strings.Contains(string(seen), "-format_whitelist "+audioFormats+" -protocol_whitelist file -i ") {
		t.Fatalf("ffmpeg may open anything: %q", seen)
	}
	_ = os.Remove(calls)
	o.FFmpeg = filepath.Join(dir, "no-ffmpeg")
	if _, err := Transcribe(context.Background(), []byte("ftypM4A voice"), "audio/mp4", o); !errors.Is(err, ErrNoConverter) {
		t.Fatalf("sox can't read m4a, so ffmpeg is needed: %v", err)
	}
	if seen, _ := os.ReadFile(calls); len(seen) != 0 {
		t.Fatalf("sox was asked to read m4a: %q", seen)
	}
	if got, err := Transcribe(context.Background(), []byte("OggS voice"), "audio/ogg", o); err != nil || got != "Hello." {
		t.Fatalf("sox reads Opus: %q %v", got, err)
	}
}

// A whisper server that answers it couldn't hear one clip ({"error": …})
// works, and the voice channel keeps using it for that; voice notes now
// agree: that clip goes to whisper-cli, and the next one to the server
// again, instead of setting a healthy server aside for ten minutes.
func TestAServerThatCouldntHearOneClipIsKept(t *testing.T) {
	var inference atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "whisper.cpp")
		if r.URL.Path == "/inference" {
			if inference.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"error":"failed to read WAV file"}`)
				return
			}
			_, _ = io.WriteString(w, `{"text":" From the server."}`)
			return
		}
		_, _ = io.WriteString(w, whisperPage)
	}))
	defer srv.Close()
	bin, model, _ := fakeCLI(t, "From the CLI.")
	o := Options{Server: srv.URL, Bin: bin, Model: model, TempDir: t.TempDir()}
	if got, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o); err != nil || got != "From the CLI." {
		t.Fatalf("the clip the server couldn't hear: %q %v", got, err)
	}
	if got, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o); err != nil || got != "From the server." {
		t.Fatalf("the next clip: %q %v (the server was set aside)", got, err)
	}
}

// Each whisper-cli run loads its own copy of the model, so voice notes are
// transcribed one at a time, however many arrive together (the microphone
// doesn't wait on this: it has its own server and CLI runs).
func TestVoiceNotesAreTranscribedOneAtATime(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "ggml-small.en.bin")
	_ = os.WriteFile(model, []byte("model"), 0o600)
	running := filepath.Join(dir, "running")
	// mkdir is atomic: a second run while one is going can't make it.
	bin := script(t, dir, "whisper-cli", `if mkdir `+running+` 2>/dev/null; then
  sleep 0.2
  rmdir `+running+`
else
  echo overlap >> `+filepath.Join(dir, "overlaps")+`
  sleep 0.2
fi
echo ' Heard.'
`)
	o := noServer(Options{Bin: bin, Model: model, TempDir: t.TempDir()})
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := Transcribe(context.Background(), wav16k(1600), "audio/wav", o); err != nil || got != "Heard." {
				t.Errorf("%q %v", got, err)
			}
		}()
	}
	wg.Wait()
	if b, err := os.ReadFile(filepath.Join(dir, "overlaps")); err == nil {
		t.Fatalf("whisper-cli ran more than once at a time: %q", b)
	}
}
