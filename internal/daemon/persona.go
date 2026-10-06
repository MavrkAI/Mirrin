package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/persona"
)

// A persona decides three voice settings: the wake word, the wake model (a
// detector trained for its word) and the voice. The owner can choose their
// own instead. config.yaml only ever holds the owner's choices: the
// persona's are applied to the live config, never saved, so switching
// persona or editing a persona file changes nothing on disk, and /reload
// picks up a persona file's new voice.

// voiceOverrides are the voice settings the owner chose themselves. nil
// follows the persona.
type voiceOverrides struct {
	WakeWord, WakeModel, Voice *string
}

// legacyVoice is what earlier releases recorded (memory.PersonaVoiceKey) of
// what the persona asked when they saved its picks into config.yaml. A value
// on disk that matches it came from the persona, not the owner. The record
// is removed at the next settings save, which no longer writes such values.
type legacyVoice struct {
	WakeWord  string `json:"wake_word"`
	WakeModel string `json:"wake_model"`
	Voice     string `json:"voice"`
}

// pickPersona resolves cfg's persona from the bundled and installed ones,
// writing back its id: a persona found by name, or by a pack id from before
// pack ids were namespaced, then shows as chosen in the menus. A twin still
// named MAVRK, as the default was before he was Mirrin, takes the persona's
// name, and so does one named for a retired persona that was chosen (Ava):
// she is Mirrin now. Skipped persona files are logged, and the owner hears
// of each once (problemfiles.go).
func (d *Daemon) pickPersona(cfg *config.Config) (persona.Persona, []persona.Persona) {
	ps, skipped := persona.LoadAll(config.Home(), cfg.ProtocolsDir)
	for _, p := range skipped {
		d.log.Warn("persona file skipped", "file", p.File, "why", p.Message)
	}
	d.personaProblems(skipped)
	was := strings.TrimSpace(cfg.Persona)
	pr, ok := persona.Pick(ps, cfg.Persona, cfg.Name)
	if ok {
		cfg.Persona = pr.ID
	} else {
		d.log.Warn("persona not found; using Mirrin's character", "persona", cfg.Persona)
	}
	if persona.FormerDefault(cfg.Name) || (persona.IsRetired(cfg.Name) && (was == "" || persona.IsRetired(was))) {
		cfg.Name = pr.Name
	}
	return pr, ps
}

// voiceCleanKey marks, in the kv store, a config.yaml this release has
// saved: its voice settings hold only the owner's choices, so each value in
// it, other than empty or stock, is one, even a word some persona also uses.
const voiceCleanKey = "persona.voice.clean"

// ownVoiceOf is the owner's choices in vc: every value once config.yaml has
// been saved clean, and otherwise what ownVoice can tell apart from the
// values earlier releases saved for a persona.
func (d *Daemon) ownVoiceOf(vc config.Voice, pr persona.Persona, known []persona.Persona) voiceOverrides {
	if d.voiceClean() {
		return cleanVoice(vc)
	}
	return ownVoice(vc, pr, known, d.legacyVoice())
}

// formerStock reports whether a wake word or wake model is what the
// default persona had before he was Mirrin: "maverick", and the "Hey
// Maverick" detector. Earlier releases wrote them into config.yaml as
// stock or as the persona's, so they are neither the owner's choice now.
func formerStock(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "maverick") || strings.EqualFold(filepath.Base(v), "hey_maverick.onnx")
}

// cleanVoice reads the owner's choices from a config.yaml saved clean:
// every value other than empty or stock.
func cleanVoice(vc config.Voice) voiceOverrides {
	stock := config.Default().Channels.Voice
	chosen := func(cur, stock string) *string {
		if cur == "" || cur == stock || formerStock(cur) {
			return nil
		}
		return &cur
	}
	return voiceOverrides{
		WakeWord:  chosen(vc.WakeWord, stock.WakeWord),
		WakeModel: chosen(vc.WakeModel, stock.WakeModel),
		Voice:     chosen(vc.Voice, stock.Voice),
	}
}

// ownVoice reads the owner's choices from voice settings as config.yaml
// holds them before this release first saves it. Empty and stock values
// aren't choices, and neither are values earlier releases saved for a persona: any persona's wake word, name or
// detector (a retired one's included), the voice of the persona in use or of
// a retired one, or what the legacy record says the persona asked for.
func ownVoice(vc config.Voice, pr persona.Persona, known []persona.Persona, legacy *legacyVoice) voiceOverrides {
	stock := config.Default().Channels.Voice
	var lv legacyVoice
	if legacy != nil {
		lv = *legacy
	}
	chosen := func(cur, stock, recorded string, persona func(string) bool) *string {
		if cur == "" || cur == stock || formerStock(cur) || (recorded != "" && cur == recorded) || persona(cur) {
			return nil
		}
		return &cur
	}
	known = append(append([]persona.Persona{}, known...), persona.Retired...)
	retiredVoice := func(v string) bool {
		for _, p := range persona.Retired {
			if v == p.Voice {
				return true
			}
		}
		return false
	}
	var own voiceOverrides
	own.WakeWord = chosen(vc.WakeWord, stock.WakeWord, lv.WakeWord, func(w string) bool {
		if strings.EqualFold(w, pr.WakeWord) || strings.EqualFold(w, pr.Name) {
			return true
		}
		for _, p := range known {
			if strings.EqualFold(w, p.WakeWord) || strings.EqualFold(w, p.Name) {
				return true
			}
		}
		return false
	})
	own.WakeModel = chosen(vc.WakeModel, stock.WakeModel, lv.WakeModel, func(m string) bool {
		for _, p := range known {
			if pm := modelPath(vc.KokoroDir, p.WakeModel); pm != "" && (m == pm || m == p.WakeModel) {
				return true
			}
		}
		return false
	})
	own.Voice = chosen(vc.Voice, stock.Voice, lv.Voice, func(v string) bool { return v == pr.Voice || retiredVoice(v) })
	return own
}

// resolveVoice points vc's wake word, wake model and voice at the owner's
// choices, and elsewhere at pr's, returning pr as it will sound. A wake
// model is trained for one word, so the persona's only comes with its own
// wake word, and the persona's aliases are misrecognitions of its own word,
// not the owner's.
func resolveVoice(vc *config.Voice, own voiceOverrides, pr persona.Persona) persona.Persona {
	stock := config.Default().Channels.Voice
	pick := func(o *string, persona, stock string) string {
		switch {
		case o != nil:
			return *o
		case persona != "":
			return persona
		}
		return stock
	}
	vc.WakeWord = pick(own.WakeWord, pr.WakeWord, stock.WakeWord)
	model := ""
	if m := modelPath(vc.KokoroDir, pr.WakeModel); m != "" && strings.EqualFold(vc.WakeWord, pr.WakeWord) {
		if _, err := os.Stat(m); err == nil {
			model = m
		}
	}
	if own.WakeModel != nil {
		vc.WakeModel = *own.WakeModel
	} else {
		vc.WakeModel = model
	}
	vc.Voice = pick(own.Voice, pr.Voice, stock.Voice)
	// A character voice's pitch and pace come with it, and only with it.
	vc.Pitch = 0
	if own.Voice == nil && pr.Voice != "" {
		vc.Pitch = pr.VoicePitch
		if pr.VoiceSpeed > 0 && (vc.Speed == 0 || vc.Speed == stock.Speed) {
			vc.Speed = pr.VoiceSpeed
		}
	}
	if !strings.EqualFold(vc.WakeWord, pr.WakeWord) {
		pr.WakeWord, pr.WakeAliases = vc.WakeWord, nil
	}
	return pr
}

// saved writes the owner's choices into vc as config.yaml should hold them:
// their own values, and the stock ones (which a save leaves out) elsewhere.
func (o voiceOverrides) saved(vc *config.Voice) {
	stock := config.Default().Channels.Voice
	set := func(dst *string, o *string, stock string) {
		if o != nil {
			*dst = *o
		} else {
			*dst = stock
		}
	}
	set(&vc.WakeWord, o.WakeWord, stock.WakeWord)
	set(&vc.WakeModel, o.WakeModel, stock.WakeModel)
	set(&vc.Voice, o.Voice, stock.Voice)
}

// changedVoice is own after a settings change took the voice settings from
// base (as config.yaml holds them) to next: a setting the change moved away
// from both base and the live, resolved value is the owner's choice now;
// setting it back to empty or stock follows the persona again.
func changedVoice(own voiceOverrides, base, next, live config.Voice) voiceOverrides {
	stock := config.Default().Channels.Voice
	upd := func(o *string, b, n, l, stock string) *string {
		if n == b || n == l {
			return o
		}
		if n == "" || n == stock {
			return nil
		}
		return &n
	}
	own.WakeWord = upd(own.WakeWord, base.WakeWord, next.WakeWord, live.WakeWord, stock.WakeWord)
	own.WakeModel = upd(own.WakeModel, base.WakeModel, next.WakeModel, live.WakeModel, stock.WakeModel)
	own.Voice = upd(own.Voice, base.Voice, next.Voice, live.Voice, stock.Voice)
	return own
}

// voiceToSave is the persona next speaks as and the owner's voice choices
// after a settings change from base (config.yaml as it was) to next, with
// next's voice settings set to what config.yaml should hold: those choices,
// and nothing of the persona's.
func (d *Daemon) voiceToSave(base, next *config.Config) (persona.Persona, voiceOverrides) {
	pr, known := d.pickPersona(next)
	was, _ := persona.Pick(known, base.Persona, base.Name) // whose picks an old config.yaml may hold
	own := changedVoice(d.ownVoiceOf(base.Channels.Voice, was, known), base.Channels.Voice, next.Channels.Voice, d.cfg.Channels.Voice)
	own.saved(&next.Channels.Voice)
	dropInheritedAddress(base, next, was, pr)
	return pr, own
}

// dropInheritedAddress lets a new persona address the owner its own way: a
// form of address that is only the outgoing persona's (Mirrin's "sir") is
// cleared, in what is saved too, unless this change set it itself.
func dropInheritedAddress(base, next *config.Config, was, now persona.Persona) {
	h := strings.TrimSpace(next.User.Honorific)
	if was.ID == now.ID || was.Address == "" || next.User.Honorific != base.User.Honorific || !strings.EqualFold(h, was.Address) {
		return
	}
	next.User.Honorific = ""
}

// modelPath is a wake model as a path: a bare name is in the Kokoro folder.
func modelPath(kokoroDir, m string) string {
	if m == "" || filepath.IsAbs(m) {
		return m
	}
	return filepath.Join(kokoroDir, m)
}

// applyPersona resolves cfg's persona and applies it to cfg's voice settings
// and the agent. cfg holds the owner's choices, as config.yaml does.
func (d *Daemon) applyPersona(cfg *config.Config) {
	pr, known := d.pickPersona(cfg)
	own := d.ownVoiceOf(cfg.Channels.Voice, pr, known)
	d.voiceOwn = own
	d.usePersona(resolveVoice(&cfg.Channels.Voice, own, pr))
}

// reloadPersona re-reads the persona files (/reload), so an edited persona
// file's voice and character apply without a restart. Nothing is saved.
func (d *Daemon) reloadPersona() {
	d.cmu.Lock()
	defer d.cmu.Unlock()
	live := *d.cfg
	pr, _ := d.pickPersona(&live)
	pr = resolveVoice(&live.Channels.Voice, d.voiceOwn, pr)
	*d.cfg = live
	d.agent.SetConfig(live)
	d.usePersona(pr)
}

// usePersona makes pr the persona the agent and voice speak as. Its form of
// address applies at once; one inherited from the persona before it was
// cleared before the switch was saved (dropInheritedAddress).
func (d *Daemon) usePersona(pr persona.Persona) {
	d.persona = pr
	d.agent.SetPersona(pr.Name, pr.Spoken(), pr.Character, pr.Address, pr.Style)
	d.voiceAddress(helloName(d.cfg), addressOf(d.cfg, pr)) // address.go
}

// legacyVoice is the record earlier releases kept, or nil.
func (d *Daemon) legacyVoice() *legacyVoice {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := d.store.Get(ctx, memory.PersonaVoiceKey)
	if err != nil || raw == "" {
		return nil
	}
	var lv legacyVoice
	if json.Unmarshal([]byte(raw), &lv) != nil {
		return nil
	}
	return &lv
}

// voiceClean reports whether config.yaml has been saved clean (voiceCleanKey).
func (d *Daemon) voiceClean() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, err := d.store.Get(ctx, voiceCleanKey)
	return err == nil && v != ""
}

// dropLegacyVoice removes the old record once config.yaml has been saved
// without the persona's values it described, and notes that it has been:
// from then on each value in it is the owner's (voiceCleanKey).
func (d *Daemon) dropLegacyVoice() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.store.Unset(ctx, memory.PersonaVoiceKey); err != nil {
		d.log.Warn("forget the old persona voice record", "err", err)
		return
	}
	if err := d.store.Set(ctx, voiceCleanKey, "1"); err != nil {
		d.log.Warn("note config.yaml's voice settings as clean", "err", err)
	}
}
