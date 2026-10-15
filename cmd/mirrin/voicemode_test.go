package main

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Always-on listening is the config's default, but `mirrin voice` is a
// conversation: push to talk unless --wake asks for always-on.
func TestVoiceSessionIsPushToTalkUnlessAskedToWake(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	if cfg.Channels.Voice.Mode != "wake" {
		t.Fatalf("the default should be always-on, got %q", cfg.Channels.Voice.Mode)
	}
	sessionVoice(cfg, false)
	if cfg.Channels.Voice.Mode != "push" {
		t.Fatalf("mirrin voice: mode %q, want push", cfg.Channels.Voice.Mode)
	}
	sessionVoice(cfg, true)
	if cfg.Channels.Voice.Mode != "wake" {
		t.Fatalf("mirrin voice --wake: mode %q, want wake", cfg.Channels.Voice.Mode)
	}
}
