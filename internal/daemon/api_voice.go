package daemon

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/persona"
)

// One-click voice setup from the page (api.VoiceSetupBackend): the same
// steps as `mirrin voice setup` (voice.RunSetup), then the twin hears with
// what they installed at once.

// voiceSetupSteps are the downloads and installers; reloadVoice has the
// twin pick up the new settings. Tests replace both.
var (
	voiceSetupSteps = voice.DefaultSetupSteps
	reloadVoice     = (*Daemon).ReloadVoice
)

// sayPreview speaks a persona's greeting for the welcome page; tests
// replace it. previewMu keeps two previews from talking over each other.
var (
	sayPreview = voice.Say
	previewMu  sync.Mutex
)

// PreviewPersona says a bundled persona's greeting in its own voice, pitch
// and pace (api.WelcomePreviewer: the welcome page's "Hear Nyra"), as
// PreviewVoice does with the current one. It reports whether it spoke: not
// until voice is set up, nor for a persona that isn't bundled.
func (d *Daemon) PreviewPersona(ctx context.Context, id string) bool {
	c := d.Config()
	p, ok := persona.Find(persona.Bundled(), id)
	if !ok || !voice.SetUp(c.Channels.Voice) || strings.TrimSpace(p.Greeting) == "" {
		return false
	}
	v := c.Channels.Voice
	if p.Voice != "" {
		v.Voice = p.Voice
	}
	v.Pitch, v.Speed = p.VoicePitch, p.VoiceSpeed
	if v.Speed <= 0 {
		v.Speed = config.Default().Channels.Voice.Speed
	}
	previewMu.Lock()
	defer previewMu.Unlock()
	sayPreview(ctx, v, p.Spoken(), c.DataDir, persona.Render(p.Greeting, "", p.Address)) // as the welcome page shows it
	return true
}

// HasWakeModel reports whether a persona's wake model is in the voice
// folder or comes with this build (api.WelcomeWakeChecker): the welcome page
// doesn't promise Mirrin hears his name first when the build has no model.
func (d *Daemon) HasWakeModel(model string) bool {
	return voice.WakeModelAvailable(d.Config().Channels.Voice.KokoroDir, model)
}

// VoiceReady reports whether voice is set up.
func (d *Daemon) VoiceReady(context.Context) bool { return voice.SetUp(d.Config().Channels.Voice) }

// SetupVoice runs voice setup, saves what it set and reloads voice.
func (d *Daemon) SetupVoice(ctx context.Context, progress func(api.VoiceStep)) ([]api.VoiceStep, error) {
	cfg := d.Config()
	v := cfg.Channels.Voice
	conv := func(s voice.SetupStep) api.VoiceStep {
		return api.VoiceStep{Label: s.Label, Running: s.Running, OK: s.OK, Notes: s.Notes}
	}
	steps, err := voice.RunSetup(ctx, &v, cfg.Name, filepath.Join(config.Home(), "models"), io.Discard, voiceSetupSteps(), func(s voice.SetupStep) {
		if progress != nil {
			progress(conv(s))
		}
	})
	if err != nil {
		return nil, err
	}
	// Only what setup sets: anything else edited meanwhile stays.
	if err := d.UpdateConfig(func(c *config.Config) {
		cv := &c.Channels.Voice
		cv.WhisperModel, cv.Language, cv.Engine, cv.Voice = v.WhisperModel, v.Language, v.Engine, v.Voice
	}); err != nil {
		return nil, err
	}
	out := make([]api.VoiceStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, conv(s))
	}
	if err := reloadVoice(d); err != nil {
		out = append(out, api.VoiceStep{Label: "Switching over", Notes: "the new settings are saved, but I couldn't switch to them (" + err.Error() + "); choose Restart from my menu"})
	}
	return out, nil
}

var (
	_ api.VoiceSetupBackend  = (*Daemon)(nil)
	_ api.WelcomePreviewer   = (*Daemon)(nil)
	_ api.WelcomeWakeChecker = (*Daemon)(nil)
)
