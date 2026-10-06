package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/persona"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// The twin addresses the owner the way they chose (user.honorific), and
// otherwise the way its persona does: "sir" for Mirrin, by first name for
// Nyra and Pickoo. The fixed lines (on it, paused, no more tips, the
// voice's answer to a bare wake) follow it, so none of them says "sir" to
// someone who never chose it.

// addressCheckedKey marks, in the kv store, that fixDefaultAddress has run.
const addressCheckedKey = "address.checked"

// address is how the twin addresses the owner now, "" when it has no way to.
func (d *Daemon) address() string {
	d.cmu.RLock()
	defer d.cmu.RUnlock()
	return addressOf(d.cfg, d.persona)
}

// addressOf is how pr addresses cfg's owner: the form they chose, else the
// persona's own, else (a persona that uses names) their first name.
func addressOf(cfg *config.Config, pr persona.Persona) string {
	if h := strings.TrimSpace(cfg.User.Honorific); h != "" {
		return h
	}
	if pr.Address != "" {
		return pr.Address
	}
	return firstName(cfg.User.Name)
}

// helloName is the owner as the hellos and the voice's greeting name them
// ({name}): the form of address they chose, so Nyra says "Morning,
// ma'am." to someone who chose ma'am, else their first name. "By my name"
// saves the first name as that choice.
func helloName(cfg *config.Config) string {
	if h := strings.TrimSpace(cfg.User.Honorific); h != "" {
		return h
	}
	return firstName(cfg.User.Name)
}

// firstName is the first word of a name, "" for none.
func firstName(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return ""
}

// withAddress puts ", address" into s before its final punctuation, if it
// has any: withAddress("Understood.", "sir") is "Understood, sir.". With no
// address s is unchanged.
func withAddress(s, address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return s
	}
	body := strings.TrimRightFunc(s, func(r rune) bool { return strings.ContainsRune(".?!…", r) })
	return body + ", " + address + s[len(body):]
}

// voiceAddress gives the live voice the name its hellos use (helloName)
// and the owner's form of address, for its answer to a bare wake and its
// hellos. It takes chmu, so it may be called with cmu held.
func (d *Daemon) voiceAddress(name, address string) {
	if ch, ok := d.channel("voice"); ok {
		if v, ok := ch.(*voice.Channel); ok {
			v.SetName(name)
			v.SetAddress(address)
		}
	}
}

// setHonorific saves how the twin addresses the owner ("" for the
// persona's own way) and applies it at once: the chat model's character,
// the fixed lines and the voice. Nothing else is reloaded.
func (d *Daemon) setHonorific(h string) error {
	d.cmu.Lock()
	err := config.Edit(func() error { // one edit of config.yaml, as UpdateConfig
		base, err := d.configBase() // the file as it stands, hand edits included
		if err != nil {
			return err
		}
		next := *base
		next.User.Honorific = h
		if err := next.Validate(); err != nil {
			return err
		}
		if err := next.Save(); err != nil {
			return err
		}
		live := *d.cfg
		live.User.Honorific = h
		*d.cfg = live
		d.agent.SetConfig(live)
		return nil
	})
	name, address := helloName(d.cfg), addressOf(d.cfg, d.persona)
	d.cmu.Unlock()
	if err != nil {
		return err
	}
	d.voiceAddress(name, address)
	return nil
}

// fixDefaultAddress mends, once, what earlier releases left behind: "sir"
// was the default form of address whatever the persona, so personas that
// use first names called owners who had never chosen it "sir". Where the persona
// addresses people by name, that "sir" goes; Mirrin's owners keep theirs.
func (d *Daemon) fixDefaultAddress(ctx context.Context) {
	if v, _ := d.store.Get(ctx, addressCheckedKey); v != "" {
		return
	}
	d.cmu.RLock()
	stale := strings.EqualFold(strings.TrimSpace(d.cfg.User.Honorific), "sir") && d.persona.Address == ""
	d.cmu.RUnlock()
	if stale {
		if err := d.setHonorific(""); err != nil {
			d.log.Warn("default form of address", "err", err)
			return // tried again at the next start
		}
		d.store.Audit(ctx, "address.default_fixed", "", "the stock \"sir\" gave way to the persona's own form of address")
	}
	if err := d.store.Set(ctx, addressCheckedKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		d.log.Warn("note the form of address as checked", "err", err)
	}
}

// maxAddress is the longest form of address set_address keeps, in characters.
const maxAddress = 24

// setAddressTool lets the owner change how the twin addresses them.
func (d *Daemon) setAddressTool() *tools.Func {
	return tools.New("set_address",
		"Change how you address the owner when they ask (‘call me Akshaya’, ‘ma'am, please’).",
		tools.Schema(map[string]tools.Prop{
			"address": {Type: "string", Description: "\"sir\", \"ma'am\", \"name\" for their first name, or the words they asked for (up to 24 characters)", Required: true},
		}), tools.RiskRead,
		func(ctx context.Context, call tools.Call) (string, error) {
			var in struct{ Address string }
			if err := tools.Decode(call, &in); err != nil {
				return "", err
			}
			h, err := d.chosenAddress(in.Address)
			if err != nil {
				return "", err
			}
			if err := d.setHonorific(h); err != nil {
				return "", fmt.Errorf("I couldn't save that: %w", err)
			}
			d.store.Audit(ctx, "address.set", call.ChatKey, h)
			return fmt.Sprintf("Saved. From now on you address them as %q.", h), nil
		})
}

// chosenAddress reads set_address's input: "sir", "ma'am", "name" for the
// owner's first name, or their own words, kept short and on one line.
func (d *Daemon) chosenAddress(s string) (string, error) {
	s = strings.Trim(strings.TrimSpace(s), "\"'‘’“”")
	switch strings.ToLower(s) {
	case "sir":
		return "sir", nil
	case "ma'am", "maam", "ma’am":
		return "ma'am", nil
	case "name":
		d.cmu.RLock()
		name := firstName(d.cfg.User.Name)
		d.cmu.RUnlock()
		if name == "" {
			return "", errors.New("you don't know the owner's name yet: ask what they'd like to be called")
		}
		return name, nil
	}
	s = strings.Join(strings.Fields(s), " ")
	switch {
	case s == "":
		return "", errors.New("say how they'd like to be addressed")
	case utf8.RuneCountInString(s) > maxAddress:
		return "", fmt.Errorf("that's longer than %d characters: ask for something shorter", maxAddress)
	case strings.ContainsFunc(s, unicode.IsControl):
		return "", errors.New("that isn't a form of address")
	}
	return s, nil
}

// stopNudgingPhrases are the whole messages that end the first-week tour.
// Anything longer ("can you stop nudging Priya about the invoice") is a
// request for the model, not this.
var stopNudgingPhrases = map[string]bool{
	"stop nudging": true, "stop nudging me": true, "no more tips": true, "stop the tips": true, "stop the tour": true,
}

// stopsNudging reports whether text, as a whole, asks to end the tour.
func stopsNudging(text string) bool {
	t := strings.Trim(strings.ToLower(strings.TrimSpace(text)), " .,!?;:…")
	return stopNudgingPhrases[strings.Join(strings.Fields(t), " ")]
}
