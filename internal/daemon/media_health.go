package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"

	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/transcribe"
)

// Speech-to-text for chat attachments is useful even with the microphone off.
func (d *Daemon) mediaHealth(ctx context.Context) (health.State, string, string) {
	cfg := d.Config()
	c := cfg.Channels
	if !(c.WhatsApp.Enabled || c.Telegram.Enabled || c.Discord.Enabled || c.Slack.Enabled || c.Matrix.Enabled || c.Mattermost.Enabled || c.Signal.Enabled || (c.IMessage.Enabled && runtime.GOOS == "darwin")) {
		return health.Off, "No voice-note channels enabled.", "Enable a messaging channel in Channels to receive voice notes."
	}
	options := transcribeOptions(cfg)
	if err := canHear(ctx, options); err != nil {
		var missing *transcribe.UnavailableError
		if errors.As(err, &missing) {
			fix := "Install whisper.cpp and ffmpeg, then run mirrin voice setup."
			if runtime.GOOS == "darwin" {
				fix = "Run brew install whisper-cpp ffmpeg, then mirrin voice setup."
			}
			if missing.Missing == "the whisper model" {
				fix = "Run mirrin voice setup to download the speech-to-text model."
			}
			state, detail := health.Off, "Voice notes aren't set up yet."
			_, modelErr := os.Stat(cfg.Channels.Voice.WhisperModel)
			if cfg.Channels.Voice.Enabled || modelErr == nil || (options.Server != "" && options.Server != "off") {
				state, detail = health.Warn, "Speech-to-text needs attention."
			}
			return state, detail, fix
		}
		return health.Warn, "Speech-to-text couldn't be checked.", "Run mirrin voice setup, then check again."
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		if _, soxErr := exec.LookPath("sox"); soxErr == nil {
			return health.OK, "Ready for voice notes supported by sox.", "Some audio formats also need ffmpeg. " + ffmpegSetup(runtime.GOOS)
		}
		// iMessage converts its own audio to WAV with the built-in macOS tools.
		if runtime.GOOS == "darwin" && c.IMessage.Enabled && !(c.WhatsApp.Enabled || c.Telegram.Enabled || c.Discord.Enabled || c.Slack.Enabled || c.Matrix.Enabled || c.Mattermost.Enabled || c.Signal.Enabled) {
			return health.OK, "Ready for iMessage voice notes.", ""
		}
		return health.Warn, "Speech-to-text is ready, but an audio converter is missing.", ffmpegSetup(runtime.GOOS)
	}
	return health.OK, "Ready to transcribe voice notes with whisper-server or whisper-cli.", ""
}
