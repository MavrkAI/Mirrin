package transcribe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The voice channel runs whisper-cli through CLI, as voice notes do: the
// same arguments, the same cleaning, and whisper-cli itself (not a look
// first) says when the model is missing.
func TestCLIIsTheOneWhisperRun(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	bin := script(t, dir, "whisper-cli", `echo "$@" > `+args+`
printf ' Hey Mirrin,\n what time is it? [BLANK_AUDIO]\n'
`)
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, wav16k(1600), 0o600); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "not-downloaded-yet.bin")
	got, err := CLI(context.Background(), wav, Options{Bin: bin, Model: model, Language: "en", Prompt: "Hey Mirrin, Mirrin."})
	if err != nil || got != "Hey Mirrin, what time is it?" {
		t.Fatalf("CLI: %q %v", got, err)
	}
	seen, _ := os.ReadFile(args)
	for _, want := range []string{"-m " + model, "-f " + wav, "-l en", "--prompt Hey Mirrin, Mirrin."} {
		if !strings.Contains(string(seen), want) {
			t.Errorf("whisper-cli got %q, missing %q", seen, want)
		}
	}
}

// A server that answers but couldn't hear the clip says so as an
// AnswerError (the voice channel doesn't rest its server for that); a
// server that fails does not.
func TestPostTellsTheServersRefusalFromItsFailure(t *testing.T) {
	var refuse atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/private/inference" {
			http.NotFound(w, r)
			return
		}
		if refuse.Load() {
			_, _ = io.WriteString(w, `{"error":"failed to read WAV file"}`)
			return
		}
		http.Error(w, "busy", http.StatusInternalServerError)
	}))
	defer srv.Close()
	wav := filepath.Join(t.TempDir(), "in.wav")
	if err := os.WriteFile(wav, wav16k(1600), 0o600); err != nil {
		t.Fatal(err)
	}
	var answer *AnswerError
	if _, err := Post(context.Background(), srv.URL+"/private", wav, Options{}); err == nil || errors.As(err, &answer) {
		t.Fatalf("a failing server: %v", err)
	}
	refuse.Store(true)
	if _, err := Post(context.Background(), srv.URL+"/private", wav, Options{}); !errors.As(err, &answer) || answer.Msg != "failed to read WAV file" {
		t.Fatalf("the server's own refusal: %v", err)
	}
}
