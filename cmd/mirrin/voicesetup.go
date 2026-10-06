package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// voiceSetup installs the offline ears (whisper, in the owner's language),
// a natural offline voice (Kokoro) and the on-device wake word, points the
// config at them, and hands the result to a running twin so it hears with
// them at once. It says plainly what worked and what didn't, and exits 1
// when anything didn't, so it never claims a success it didn't have.
func voiceSetup(ctx context.Context, cfg *config.Config) error {
	return runVoiceSetup(ctx, cfg, os.Stdout, voiceSteps{
		lookPath: exec.LookPath,
		language: voice.SystemLanguage,
		whisper:  voice.DownloadWhisperModel,
		kokoro:   voice.SetupKokoro,
		wake:     voice.SetupWake,
		save:     func(c *config.Config) error { return c.Save() },
		tell:     tellRunningTwin,
	})
}

// voiceSteps are the parts of voice setup, replaceable in tests.
type voiceSteps struct {
	lookPath func(string) (string, error)
	language func() string
	whisper  func(ctx context.Context, dir, model string, w io.Writer) (string, error)
	kokoro   func(ctx context.Context, dir string, w io.Writer) error
	wake     func(ctx context.Context, dir string, w io.Writer) error
	save     func(*config.Config) error
	// tell hands the new settings to a running twin; running is false when
	// there is none.
	tell func(ctx context.Context, cfg *config.Config) (running bool, err error)
}

func runVoiceSetup(ctx context.Context, cfg *config.Config, w io.Writer, s voiceSteps) error {
	fmt.Fprintf(w, "Setting up %s's voice: ears (whisper), a natural offline voice (Kokoro) and the wake word.\n", cfg.Name)
	fmt.Fprintln(w, "The first time, this downloads about 850 MB. If it stops, run it again and it picks up where it left off.")
	fmt.Fprintln(w)
	// The steps themselves are the voice package's, shared with the Set up
	// voice page (POST /voice/setup).
	results, err := voice.RunSetup(ctx, &cfg.Channels.Voice, cfg.Name, filepath.Join(config.Home(), "models"), w, voice.SetupSteps{
		LookPath: s.lookPath, Language: s.language, Whisper: s.whisper, Kokoro: s.kokoro, Wake: s.wake, OS: setupOS,
	}, nil)
	if err != nil {
		return interrupted(w)
	}
	if err := s.save(cfg); err != nil {
		return fmt.Errorf("couldn't save the voice settings: %w", err)
	}

	fmt.Fprintln(w)
	failed := false
	for _, r := range results {
		mark := "✓"
		if !r.OK {
			mark, failed = "✗", true
		}
		fmt.Fprintf(w, " %s %-10s %s\n", mark, r.Label, r.Notes)
	}
	fmt.Fprintln(w)

	phrase := voice.WakePhrase(cfg.Channels.Voice.WakeWord)
	running, terr := s.tell(ctx, cfg)
	switch {
	case terr != nil:
		fmt.Fprintf(w, "Your running %s couldn't switch over (%s). Choose Restart from its menu.\n", cfg.Name, firstLine(terr))
	case running:
		fmt.Fprintf(w, "Your running %s is using the new voice settings now.\n", cfg.Name)
	}
	if failed {
		fmt.Fprintln(w, "Something above didn't finish. Fix what it says and run mirrin voice setup again; it keeps what's already done.")
		return exitCode(1)
	}
	if !running {
		fmt.Fprintf(w, "All set. Talk to %s with mirrin voice (press Enter, then speak), or mirrin voice --wake and just say %q.\n", cfg.Name, phrase)
	} else {
		fmt.Fprintf(w, "All set. Say %q.\n", phrase)
	}
	return nil
}

func interrupted(w io.Writer) error {
	fmt.Fprintln(w, "\nStopped. Run mirrin voice setup again to pick up where it left off.")
	return exitCode(130)
}

// setupOS is the system voice setup gives install hints for.
var setupOS = runtime.GOOS

// firstLine is an error's first line, without a trailing full stop.
func firstLine(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, ". ")
}

// tellRunningTwin asks a twin running on this machine to pick up the voice
// settings just saved and rebuild its microphone pipeline from them, so
// setup (often run from the menu bar) never leaves it hearing with the old
// ones. running is false when no twin answers.
func tellRunningTwin(ctx context.Context, cfg *config.Config) (bool, error) {
	if cfg.API.Listen == "" {
		return false, nil
	}
	c := api.Connect(cfg.API.Listen, cfg.DataDir)
	if c == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := c.RunJob(ctx, "voice"); err != nil {
		// The twin's refusal in its own words ({error, message, fix}), not
		// the raw body.
		var refused *api.Error
		if errors.As(err, &refused) {
			if msg := strings.TrimSpace(refused.Message + " " + refused.Fix); msg != "" {
				return true, errors.New(msg)
			}
		}
		return true, err
	}
	return true, nil
}
