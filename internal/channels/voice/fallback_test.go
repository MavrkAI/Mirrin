package voice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// When ElevenLabs failed (out of quota, offline) every sentence was
// skipped: the twin went silent and nobody was told.
func TestBackupVoiceSpeaksWhenTheChosenOneFails(t *testing.T) {
	var hits atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"detail":{"status":"quota_exceeded"}}`)
			return
		}
		_, _ = io.WriteString(w, "ID3-mp3-bytes")
	}))
	defer srv.Close()
	dir := t.TempDir()
	c := &Channel{dataDir: dir, out: io.Discard, cfg: config.Voice{KokoroDir: dir}}
	c.eleven = newElevenLabs("key", "AbCdEfGhIjKlMnOpQrStUv", "") // an id: no lookup
	c.eleven.base = srv.URL
	var mu sync.Mutex
	var problems, said []string
	c.OnProblem = func(m string) { mu.Lock(); problems = append(problems, m); mu.Unlock() }
	defer func(f func(*Channel, context.Context, string, string) (string, error)) { systemSynthTo = f }(systemSynthTo)
	systemSynthTo = func(_ *Channel, _ context.Context, text, base string) (string, error) {
		mu.Lock()
		said = append(said, text)
		mu.Unlock()
		p := base + ".aiff"
		return p, os.WriteFile(p, []byte(text), 0o644)
	}
	ctx := context.Background()
	seq := 0

	clips := c.synthesize(ctx, "Your train is at six.", &seq)
	if len(clips) != 2 || clips[0].text != backupNote || clips[1].text != "Your train is at six." {
		t.Fatalf("clips %+v: want the backup's heads-up, then the sentence", clips)
	}
	for _, cl := range clips {
		if cl.err != nil || !strings.HasSuffix(cl.path, ".aiff") {
			t.Fatalf("clip %+v not from the backup voice", cl)
		}
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "ElevenLabs") || !strings.Contains(problems[0], "backup voice") {
		t.Fatalf("problems %q", problems)
	}

	// Next sentence: straight to the backup, no second note, no second
	// ElevenLabs timeout while it rests.
	before := hits.Load()
	clips = c.synthesize(ctx, "It's on time.", &seq)
	if len(clips) != 1 || clips[0].text != "It's on time." || hits.Load() != before || len(problems) != 1 {
		t.Fatalf("clips %+v, %d more requests, problems %q", clips, hits.Load()-before, problems)
	}

	// Once it rests long enough and works again, it speaks; a later failure is told again.
	failing.Store(false)
	c.elevenDown = time.Time{}
	clips = c.synthesize(ctx, "Back to normal.", &seq)
	if len(clips) != 1 || !strings.HasSuffix(clips[0].path, ".mp3") {
		t.Fatalf("ElevenLabs didn't come back: %+v", clips)
	}
	failing.Store(true)
	c.synthesize(ctx, "Oops.", &seq)
	if len(problems) != 2 {
		t.Fatalf("a new failure after recovering wasn't told: %q", problems)
	}
}

// When even the backup can't write a clip, the clip carries the error and
// the playback stage says the text with the system voice directly.
func TestNoBackupFileMeansSayItDirectly(t *testing.T) {
	dir := t.TempDir()
	c := &Channel{dataDir: dir, out: io.Discard, cfg: config.Voice{KokoroDir: dir}}
	c.kokoro = newKokoro(dir, "bm_george", 1) // not installed: its helper can't start
	defer func(f func(*Channel, context.Context, string, string) (string, error)) { systemSynthTo = f }(systemSynthTo)
	systemSynthTo = func(*Channel, context.Context, string, string) (string, error) {
		return "", os.ErrNotExist
	}
	seq := 0
	clips := c.synthesize(context.Background(), "Hello there.", &seq)
	last := clips[len(clips)-1]
	if last.err == nil || last.text != "Hello there." {
		t.Fatalf("clip %+v", last)
	}
}

// When the system voice stood in for one sentence, it kept the player slot:
// a wake word then paused the finished system voice, not the rest of the
// reply playing through the PCM player.
func TestSystemVoiceGivesThePlayerBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("speaks with a shell command")
	}
	c := &Channel{dataDir: t.TempDir(), out: io.Discard, cfg: config.Voice{TTSCommand: "exit 0"}}
	pcm := &exec.Cmd{}
	c.player = pcm
	c.speakSystem(context.Background(), "One sentence the system voice says.")
	c.smu.Lock()
	defer c.smu.Unlock()
	if c.player != pcm {
		t.Fatal("the player slot still points at the system voice")
	}
}

// The player-failure notice told everyone to brew install sox, on Windows
// and Linux too.
func TestPlayerTroubleNamesThisSystemsFix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("speaks with a shell command")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir) // no sox
	c := &Channel{dataDir: dir, out: io.Discard, speakMu: make(chan struct{}, 1), cfg: config.Voice{KokoroDir: dir, TTSCommand: "exit 0"}}
	c.kokoro = newKokoro(dir, "bm_george", 1) // not installed: the backup writes the clip
	defer func(f func(*Channel, context.Context, string, string) (string, error)) { systemSynthTo = f }(systemSynthTo)
	systemSynthTo = func(_ *Channel, _ context.Context, text, base string) (string, error) {
		return base + ".wav", os.WriteFile(base+".wav", []byte(text), 0o644)
	}
	var problems []string
	var mu sync.Mutex
	c.OnProblem = func(m string) { mu.Lock(); problems = append(problems, m); mu.Unlock() }
	c.Speak(context.Background(), "Hello there.")
	mu.Lock()
	defer mu.Unlock()
	var player string
	for _, p := range problems {
		if strings.Contains(p, "can't play my voice") {
			player = p
		}
	}
	if player == "" || !strings.Contains(player, installHint("sox")) || strings.Contains(player, "on a Mac") {
		t.Fatalf("problems %q", problems)
	}
}

// A task's question said unprompted ("Shall I go to checkout?") listens
// for the answer; a statement, or a reply inside a conversation (which
// listens afterwards anyway), doesn't.
func TestAnUnpromptedQuestionListensForTheAnswer(t *testing.T) {
	c := &Channel{dataDir: t.TempDir(), out: io.Discard, cfg: config.Voice{TTSCommand: "exit 0"},
		speakMu: make(chan struct{}, 1), listenNow: make(chan struct{}, 1)}
	c.wakeLive.Store(true)
	asked := func() bool {
		select {
		case <-c.listenNow:
			return true
		default:
			return false
		}
	}
	_ = c.Send(context.Background(), "local", "Shop for men's clothing: Cart's open on screen, sir. Shall I go to checkout?")
	if !asked() {
		t.Fatal("asked unprompted, it didn't listen")
	}
	_ = c.Send(context.Background(), "local", "Your Uniqlo order went through.")
	if asked() {
		t.Fatal("a statement opened the microphone")
	}
	c.handling.Store(true)
	_ = c.Send(context.Background(), "local", "Which size?")
	if asked() {
		t.Fatal("a reply in a conversation opened a second window")
	}
}
