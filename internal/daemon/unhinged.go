package daemon

import (
	"context"
	"fmt"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Unhinged mode (config.Unhinged) turns the persona's character all the way
// up, from the Persona menu or from chat. It changes the prompt's manner
// and nothing else: the voice, wake word, approvals and safety floor are as
// they were. The owner's switch is saved, so it lasts until they turn it
// off, restarts included.

// setUnhinged saves the switch and applies it to the next turn. Nothing
// else is reloaded.
func (d *Daemon) setUnhinged(on bool) error {
	d.cmu.Lock()
	defer d.cmu.Unlock()
	return config.Edit(func() error { // one edit of config.yaml, as UpdateConfig
		base, err := d.configBase() // the file as it stands, hand edits included
		if err != nil {
			return err
		}
		next := *base
		next.Unhinged = on
		if err := next.Validate(); err != nil {
			return err
		}
		if err := next.Save(); err != nil {
			return err
		}
		live := *d.cfg
		live.Unhinged = on
		*d.cfg = live
		d.agent.SetConfig(live)
		return nil
	})
}

// setUnhingedTool lets the owner switch unhinged mode on and off in chat.
func (d *Daemon) setUnhingedTool() *tools.Func {
	return tools.New("set_unhinged",
		"Switch unhinged mode on or off when the owner asks: on for \"go unhinged\", \"unhinged mode\", \"go feral\"; off for \"calm down\", \"normal mode\", \"enough\". It turns your character all the way up and changes nothing about what you may do.",
		tools.Schema(map[string]tools.Prop{
			"on": {Type: "boolean", Description: "true to switch it on, false to switch it off", Required: true},
		}), tools.RiskRead,
		func(ctx context.Context, call tools.Call) (string, error) {
			var in struct{ On bool }
			if err := tools.Decode(call, &in); err != nil {
				return "", err
			}
			if err := d.setUnhinged(in.On); err != nil {
				return "", fmt.Errorf("I couldn't save that: %w", err)
			}
			d.store.Audit(ctx, "unhinged.set", call.ChatKey, fmt.Sprint(in.On))
			if in.On {
				return "Unhinged mode is on from your next reply. Say so in character; the rules about money, privacy and asking first are unchanged.", nil
			}
			return "Unhinged mode is off from your next reply. Back to your usual self.", nil
		})
}
