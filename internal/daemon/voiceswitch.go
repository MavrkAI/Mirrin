package daemon

import (
	"reflect"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// A voice chosen from the menu or the settings (another voice, engine,
// speed, wake word, …) applies to always-on listening at once: the
// microphone pipeline copies its settings when it starts, so it is
// rebuilt from the new ones. Before, the twin kept speaking in the old
// voice until it was restarted.

// rebuildVoice stops always-on listening and starts it again from the live
// settings. A variable, so tests needn't start a microphone.
var rebuildVoice = func(d *Daemon) error {
	d.StopVoice()
	return d.StartVoice()
}

// switchVoice rebuilds always-on listening when the voice settings it runs
// with changed from was, and reports whether it did. Turning listening on
// or off is updateConfig's own business.
func (d *Daemon) switchVoice(was, now config.Voice) (bool, error) {
	if !d.Listening() || !now.Enabled || now.Mode != "wake" || reflect.DeepEqual(was, now) {
		return false, nil
	}
	return true, rebuildVoice(d)
}
