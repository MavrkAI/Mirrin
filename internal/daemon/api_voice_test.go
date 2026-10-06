package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// The Set up voice page's POST streams each step as it starts and ends,
// finishes with done, saves what setup set, and has the twin switch over
// once.
func TestVoiceSetupStreamsItsSteps(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	oldSteps, oldReload := voiceSetupSteps, reloadVoice
	t.Cleanup(func() { voiceSetupSteps, reloadVoice = oldSteps, oldReload })
	voiceSetupSteps = func() voice.SetupSteps {
		return voice.SetupSteps{
			LookPath: func(bin string) (string, error) { return "/usr/local/bin/" + bin, nil },
			Language: func() string { return "en" },
			Whisper: func(_ context.Context, dir, model string, _ io.Writer) (string, error) {
				return filepath.Join(dir, "ggml-"+model+".bin"), nil
			},
			Kokoro: func(context.Context, string, io.Writer) error { return nil },
			Wake:   func(context.Context, string, io.Writer) error { return nil },
			OS:     "darwin",
		}
	}
	reloads := 0
	reloadVoice = func(*Daemon) error { reloads++; return nil }

	const master = "0123456789abcdef0123456789abcdef0123456789abcdef"
	srv := api.New("127.0.0.1:7742", master, td.Daemon)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln, api.LoopbackOnly, "loopback"); close(served) }()
	t.Cleanup(func() { cancel(); <-served })

	req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/voice/setup", nil)
	req.Header.Set("Authorization", "Bearer "+master)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var events []string
	var steps []api.VoiceStep
	var done struct {
		OK    bool            `json:"ok"`
		Steps []api.VoiceStep `json:"steps"`
	}
	sc := bufio.NewScanner(resp.Body)
	ev := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev = strings.TrimPrefix(line, "event: ")
			events = append(events, ev)
		case strings.HasPrefix(line, "data: ") && ev == "step":
			var s api.VoiceStep
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &s); err != nil {
				t.Fatal(err)
			}
			steps = append(steps, s)
		case strings.HasPrefix(line, "data: ") && ev == "done":
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &done); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Each step twice (running, then how it went), then done.
	if len(events) != 7 || events[6] != "done" {
		t.Fatalf("events %v", events)
	}
	for i, label := range []string{voice.StepHearing, voice.StepVoice, voice.StepWake} {
		if s := steps[2*i]; s.Label != label || !s.Running {
			t.Errorf("step %d starts as %+v", i, s)
		}
		if s := steps[2*i+1]; s.Label != label || s.Running || !s.OK {
			t.Errorf("step %d ends as %+v", i, s)
		}
	}
	if !done.OK || len(done.Steps) != 3 {
		t.Fatalf("done %+v", done)
	}
	if reloads != 1 {
		t.Fatalf("ReloadVoice called %d times", reloads)
	}
	v := td.Config().Channels.Voice
	if !strings.HasSuffix(v.WhisperModel, "ggml-small.en.bin") || v.Engine != "auto" {
		t.Fatalf("voice settings not saved: %+v", v)
	}
}
