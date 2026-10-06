package daemon

import (
	"context"
	"errors"

	"github.com/MavrkAI/Mirrin/internal/channels/voice"
)

// errVoiceNotListening is ListenNow's answer when always-on voice isn't running.
var errVoiceNotListening = errors.New("voice isn't listening on this computer")

// ListenNow has always-on voice listen at once, as if its name had been
// said: the owner clicked the orb.
func (d *Daemon) ListenNow(context.Context) error {
	ch, ok := d.channel("voice")
	if !ok {
		return errVoiceNotListening
	}
	if vc, isVoice := ch.(*voice.Channel); isVoice && vc.ListenNow() {
		return nil
	}
	return errVoiceNotListening
}
