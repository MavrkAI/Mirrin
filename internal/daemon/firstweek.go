package daemon

import (
	"context"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
)

// The first week: one short tip a day for seven days, in character, that
// works wherever the owner hears from the twin, the screen included. A day
// with nothing for this owner is skipped rather than padded: the calendar
// and inbox tip once both are connected, the wake word before voice is set
// up. A tip card's "No more tips" on the screen ends the tour, as the words
// do in a chat.

// Connected is what the twin can reach: a calendar, mail, and voice set up
// on this computer. The screen picks its first suggestion by it, and says
// "Calendar not connected" rather than an empty day.
type Connected struct {
	Calendar bool `json:"calendar"`
	Mail     bool `json:"mail"`
	Voice    bool `json:"voice"`
}

// connected is what the twin can reach now. A calendar or Gmail turned on
// counts only while Google is signed in.
func (d *Daemon) connected() Connected {
	cfg := d.Config()
	google := d.google != nil && d.google.Connected()
	return Connected{
		Calendar: google && d.calendar.Load() != nil,
		Mail:     (google && cfg.Skills.Gmail.Enabled) || (d.email != nil && cfg.Skills.Email.Enabled),
		Voice:    voice.SetUp(cfg.Channels.Voice),
	}
}

// nudgeTask is what the model is asked for on day (1 to 7) of the tour, or
// false when the day has nothing for this owner.
func (d *Daemon) nudgeTask(day int) (string, bool) {
	tip := nudges[day-1]
	c := d.connected()
	switch day {
	case 3:
		var missing []string
		if !c.Calendar {
			missing = append(missing, "their calendar")
		}
		if !c.Mail {
			missing = append(missing, "their email")
		}
		if len(missing) == 0 {
			return "", false
		}
		tip += " Not connected yet: " + strings.Join(missing, " and ") + ". Offer to connect what's missing, from Accounts in the menu."
	case 5:
		if !c.Voice {
			return "", false
		}
		// The persona's own wake word ("Hey Nyra"), or the owner's.
		tip = strings.ReplaceAll(tip, "{wake}", voice.WakePhrase(d.Config().Channels.Voice.WakeWord))
	}
	return "First-week tour. " + tip + " One message, short, in character.", true
}

// StopTips ends the first-week tour (a tip card's "No more tips").
func (d *Daemon) StopTips(ctx context.Context) error {
	return d.store.Set(ctx, "nudges_off", "1")
}

var _ api.TipsStopper = (*Daemon)(nil)
