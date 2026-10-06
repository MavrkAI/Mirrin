package daemon

import (
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// What the presence screen and orb show of the twin's work. Every piece of
// work holds its own part (events.Presence) and the screen shows the busiest,
// so a Telegram reply finishing while she speaks out loud can't stop her
// mouth or turn the pill back to idle.

// turnShow is what the screen shows of one channel turn. A nil one shows
// nothing.
type turnShow struct {
	bus  *events.Bus
	busy *events.Presence  // the character thinking; nil for the microphone's own turns
	from map[string]string // each line's data: its channel
}

// showTurn starts showing a channel turn: what was heard, and the character
// thinking until end (the voice channel shows its own states). Someone
// else's chat shows nothing: it mustn't animate the owner's character or
// appear as the owner's own lines. The owner still hears about what waits on
// them (strangers.go). Each line says its channel, so a wall screen that
// only looks shows only what was said out loud (api's screen_private.go).
func (d *Daemon) showTurn(in channels.Inbound) *turnShow {
	if !in.IsOwner {
		return nil
	}
	from := map[string]string{"channel": in.Channel}
	d.bus.Publish(events.Event{Kind: "heard", Text: in.Text, Data: from})
	s := &turnShow{bus: d.bus, from: from}
	if in.Channel != "voice" {
		s.busy = d.bus.Begin("thinking")
	}
	return s
}

// say shows a line of the turn: a "note" while it works, or what it "said".
func (s *turnShow) say(kind, text string) {
	if s != nil && text != "" {
		s.bus.Publish(events.Event{Kind: kind, Text: text, Data: s.from})
	}
}

// end stops showing the character thinking. Ending again does nothing.
func (s *turnShow) end() {
	if s != nil {
		s.busy.End()
	}
}

// voiceState gives a new voice channel the microphone's part in what the
// screens show, for it to move from then on. The part of the channel it
// replaces ends, so one still winding down can't show over the new one.
func (d *Daemon) voiceState() func(state string) {
	p := d.bus.Begin("idle")
	if old := d.voicePresence.Swap(p); old != nil {
		old.End()
	}
	return p.Set
}

// endVoiceState ends the microphone's part when its channel stops, so a
// channel stopped mid-listen can't leave the screens saying Listening. A
// new voice channel gets a part of its own (voiceState).
func (d *Daemon) endVoiceState() {
	if p := d.voicePresence.Swap(nil); p != nil {
		p.End()
	}
}
