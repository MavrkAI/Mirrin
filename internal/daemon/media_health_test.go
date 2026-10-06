package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/transcribe"
)

func TestMediaHealth(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("MIRRIN_WHISPER_URL", "off")
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	old := canHear
	t.Cleanup(func() { canHear = old })
	var checkErr error
	calls := 0
	canHear = func(context.Context, transcribe.Options) error { calls++; return checkErr }
	d := &Daemon{cfg: &config.Config{}}
	if state, _, fix := d.mediaHealth(context.Background()); state != health.Off || fix == "" || calls != 0 {
		t.Fatal(state, fix, calls)
	}
	d.cfg.Channels.Telegram.Enabled = true
	for _, tt := range []struct {
		err   error
		state health.State
		want  string
	}{
		{&transcribe.UnavailableError{Missing: "whisper-cli"}, health.Off, "whisper"},
		{&transcribe.UnavailableError{Missing: "the whisper model"}, health.Off, "mirrin voice setup"},
		{errors.New("private details"), health.Warn, "check again"},
		{nil, health.Warn, "ffmpeg"},
	} {
		checkErr = tt.err
		state, detail, fix := d.mediaHealth(context.Background())
		if state != tt.state || !strings.Contains(fix, tt.want) || strings.Contains(detail, "private details") {
			t.Fatalf("%s %s %s", state, detail, fix)
		}
	}
	for _, name := range []string{"sox", "ffmpeg"} {
		bin := name
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		if err := os.WriteFile(filepath.Join(dir, bin), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		checkErr = nil
		state, _, _ := d.mediaHealth(context.Background())
		if state != health.OK {
			t.Fatalf("%s installed: %s", name, state)
		}
	}
	d.cfg.Channels.Voice.Enabled = true
	checkErr = &transcribe.UnavailableError{Missing: "the whisper model"}
	if state, _, _ := d.mediaHealth(context.Background()); state != health.Warn {
		t.Fatalf("broken configured voice: %s", state)
	}
	d.cfg.Channels.Voice.Enabled = false
	t.Setenv("MIRRIN_WHISPER_URL", "http://127.0.0.1:8080")
	if state, _, _ := d.mediaHealth(context.Background()); state != health.Warn {
		t.Fatalf("broken configured server: %s", state)
	}
}

func TestMediaHealthRegistered(t *testing.T) {
	d := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	report := d.newHealth().Run(context.Background())
	for _, r := range report.Results {
		if r.Name == "voicenotes" {
			if r.Label != "Voice notes" || r.State != health.Off {
				t.Fatalf("%+v", r)
			}
			return
		}
	}
	t.Fatal("newHealth omitted the voice-note check")
}
