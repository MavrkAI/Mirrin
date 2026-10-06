package daemon

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// A settings change starts from config.yaml (configBase), so the persona's
// part of the voice settings is told apart by the record saved with that
// file, not by what the persona asked of the live config. Otherwise, after
// `persona use` while stopped, the old persona's values on disk read as the
// user's own at the first change from the tray.
func TestSettingsChangeAfterPersonaUseKeepsThePersonasVoice(t *testing.T) {
	r := newVoiceRig(t, func(c *config.Config, _ string) { c.Persona, c.Name = "nyra", "Nyra" })
	r.restart()
	r.update(func(c *config.Config) {}) // saved with Nyra's voice
	r.want("Nyra", "nyra", "nyra", "", "af_nova")
	r.cliUse("mirrin", "Mirrin")
	r.restart()
	r.want("after persona use", "mirrin", "mirrin", r.model, "bm_george")
	r.update(func(c *config.Config) { c.Autonomy.Read = "auto" })
	r.want("after a tray change", "mirrin", "mirrin", r.model, "bm_george")
	r.restart()
	r.want("after a restart", "mirrin", "mirrin", r.model, "bm_george")
}
