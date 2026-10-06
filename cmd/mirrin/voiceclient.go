package main

import (
	"context"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// joinedVoice is the voice channel of a session joined to a running twin
// (the menu bar's, or a paired one): it asks the twin whether an approval
// is waiting, so a bare "yes" heard in the follow-up window is kept as the
// answer rather than thrown away as noise. Trouble is told on the terminal.
func joinedVoice(cfg *config.Config, c *api.Client) *voice.Channel {
	ch := voice.New(cfg.Channels.Voice, cfg.Name, cfg.DataDir)
	ch.Pending = func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s, err := c.Status(ctx)
		return err == nil && s.Pending > 0
	}
	// A spoken stop while the twin works: sent as a message of its own, which
	// the twin takes at once rather than behind the turn it stops.
	ch.OnStop = func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = c.Message(ctx, "voice", "local", "stop")
		}()
	}
	return ch
}
