package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/persona"
)

func TestVoiceFor(t *testing.T) {
	kokoro := t.TempDir()
	model := filepath.Join(kokoro, "hey_mirrin.onnx")
	if err := os.WriteFile(model, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	known := persona.Bundled()
	mirrin, _ := persona.Pick(known, "mirrin", "")
	nyra, _ := persona.Pick(known, "nyra", "")
	jeeves, _ := persona.Pick(known, "mirrin", "Jeeves")
	// asked is what earlier releases recorded of a persona's picks.
	asked := func(p persona.Persona) *legacyVoice {
		v := config.Voice{KokoroDir: kokoro}
		resolveVoice(&v, voiceOverrides{}, p)
		return &legacyVoice{WakeWord: v.WakeWord, WakeModel: v.WakeModel, Voice: v.Voice}
	}
	cases := []struct {
		name                   string
		cur                    config.Voice // as config.yaml holds it
		was, pr                persona.Persona
		legacy                 *legacyVoice
		wakeWord, model, voice string
	}{
		{"fresh config, Mirrin", config.Voice{WakeWord: "mirrin", Voice: "bm_george"}, mirrin, mirrin, nil, "mirrin", model, "bm_george"},
		{"fresh config, Nyra", config.Voice{WakeWord: "mirrin", Voice: "bm_george"}, nyra, nyra, nil, "nyra", "", "af_nova"},
		{"Mirrin to Nyra drops the mirrin detector and voice",
			config.Voice{WakeWord: "mirrin", WakeModel: model, Voice: "bm_george"}, mirrin, nyra, asked(mirrin), "nyra", "", "af_nova"},
		{"Nyra back to Mirrin brings them back",
			config.Voice{WakeWord: "nyra", Voice: "af_nova"}, nyra, mirrin, asked(nyra), "mirrin", model, "bm_george"},
		{"a renamed twin is woken by its new name",
			config.Voice{WakeWord: "jeeves", Voice: "bm_george"}, jeeves, func() persona.Persona { p, _ := persona.Pick(known, "mirrin", "Alfred"); return p }(), asked(jeeves), "alfred", "", "bm_george"},
		{"the user's own wake word stays, without Mirrin's detector",
			config.Voice{WakeWord: "computer", WakeModel: model, Voice: "bm_george"}, mirrin, mirrin, asked(mirrin), "computer", "", "bm_george"},
		{"the user's own voice stays across a switch",
			config.Voice{WakeWord: "mirrin", WakeModel: model, Voice: "bm_lewis"}, mirrin, nyra, asked(mirrin), "nyra", "", "bm_lewis"},
		{"the user's own detector stays",
			config.Voice{WakeWord: "nyra", WakeModel: "my_nyra.onnx", Voice: "af_nova"}, nyra, nyra, asked(nyra), "nyra", "my_nyra.onnx", "af_nova"},
		{"upgrading from a config the old code left broken",
			config.Voice{WakeWord: "nyra", WakeModel: model, Voice: "af_nova"}, nyra, nyra, nil, "nyra", "", "af_nova"},
		{"upgrading keeps a wake word no persona uses",
			config.Voice{WakeWord: "computer", Voice: "bm_george"}, nyra, nyra, nil, "computer", "", "af_nova"},
		{"a persona switch made while stopped, from a config the old code saved",
			config.Voice{WakeWord: "nyra", Voice: "af_nova"}, mirrin, mirrin, asked(nyra), "mirrin", model, "bm_george"},
	}
	for _, c := range cases {
		vc := c.cur
		vc.KokoroDir = kokoro
		got := resolveVoice(&vc, ownVoice(vc, c.was, known, c.legacy), c.pr)
		if vc.WakeWord != c.wakeWord || vc.WakeModel != c.model || vc.Voice != c.voice {
			t.Errorf("%s: got %q %q %q, want %q %q %q", c.name, vc.WakeWord, vc.WakeModel, vc.Voice, c.wakeWord, c.model, c.voice)
		}
		if got.WakeWord != vc.WakeWord {
			t.Errorf("%s: persona wake word %q, config %q", c.name, got.WakeWord, vc.WakeWord)
		}
		if vc.WakeWord != c.pr.WakeWord && got.WakeAliases != nil {
			t.Errorf("%s: the persona's aliases should not wake a different word", c.name)
		}
	}
}

// voiceRig is a Mirrin home with the hey_mirrin detector installed. Each
// restart starts a daemon on what config.yaml holds, as a real restart does.
type voiceRig struct {
	t     *testing.T
	d     *Daemon
	model string
}

func newVoiceRig(t *testing.T, edit func(c *config.Config, model string)) *voiceRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Skills.Browser.Enabled = false
	cfg.DataDir = filepath.Join(home, "data")
	cfg.ProtocolsDir = filepath.Join(home, "protocols")
	cfg.Channels.Voice.KokoroDir = filepath.Join(home, "tts")
	cfg.LLM.APIKey = "test-key"
	r := &voiceRig{t: t, model: filepath.Join(cfg.Channels.Voice.KokoroDir, "hey_mirrin.onnx")}
	if err := os.MkdirAll(cfg.Channels.Voice.KokoroDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.model, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(cfg, r.model)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.stop)
	return r
}

func (r *voiceRig) stop() {
	if r.d != nil {
		r.d.Close()
		r.d = nil
	}
}

func (r *voiceRig) restart() {
	r.t.Helper()
	r.stop()
	cfg, err := config.Load()
	if err != nil {
		r.t.Fatal(err)
	}
	if r.d, err = New(cfg, Options{Headless: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *voiceRig) update(mutate func(c *config.Config)) {
	r.t.Helper()
	if err := r.d.UpdateConfig(mutate); err != nil {
		r.t.Fatal(err)
	}
}

// cliUse is `mirrin persona use` while the daemon is stopped: only
// config.yaml changes.
func (r *voiceRig) cliUse(id, name string) {
	r.t.Helper()
	r.stop()
	cfg, err := config.Load()
	if err != nil {
		r.t.Fatal(err)
	}
	cfg.Persona, cfg.Name = id, name
	if err := cfg.Save(); err != nil {
		r.t.Fatal(err)
	}
}

func (r *voiceRig) want(step, id, wakeWord, wakeModel, voice string) {
	r.t.Helper()
	v := r.d.Config().Channels.Voice
	if r.d.Persona().ID != id || v.WakeWord != wakeWord || v.WakeModel != wakeModel || v.Voice != voice {
		r.t.Fatalf("%s: persona %q voice %q %q %q, want %q %q %q %q", step, r.d.Persona().ID, v.WakeWord, v.WakeModel, v.Voice, id, wakeWord, wakeModel, voice)
	}
}

func choosePersona(id, name string) func(c *config.Config) {
	return func(c *config.Config) { c.Persona, c.Name = id, name }
}

// Switching persona from the tray used to keep the last persona's wake model
// and voice, and a wake_word set in config was always overwritten.
func TestPersonaSwitchThroughUpdateConfig(t *testing.T) {
	r := newVoiceRig(t, nil)
	// a broken persona file must not cost the chosen persona
	home := config.Home()
	if err := os.MkdirAll(persona.Dir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persona.Dir(home), "broken.yaml"), []byte("name: Broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.want("start", "mirrin", "mirrin", r.model, "bm_george")
	r.update(choosePersona("nyra", "Nyra"))
	r.want("to Nyra", "nyra", "nyra", "", "af_nova")
	r.update(choosePersona("mirrin", "Mirrin"))
	r.want("back to Mirrin", "mirrin", "mirrin", r.model, "bm_george")

	r.update(func(c *config.Config) { c.Channels.Voice.WakeWord = "computer" })
	r.want("own wake word", "mirrin", "computer", "", "bm_george")
	if p := r.d.Persona(); p.WakeWord != "computer" || p.WakeAliases != nil {
		t.Fatalf("persona should carry the user's wake word: %+v", p)
	}
	r.update(choosePersona("nyra", "Nyra"))
	r.want("own wake word survives a switch", "nyra", "computer", "", "af_nova")
	if b, _ := os.ReadFile(config.Path()); !strings.Contains(string(b), "wake_word: computer") {
		t.Fatalf("config.yaml lost the user's wake word:\n%s", b)
	}
}

// The default persona was MAVRK (said Maverick) before he was Mirrin. A
// config.yaml from then names him the old way and holds what was his: the
// wake word "maverick" and the "Hey Maverick" detector. It still gets him,
// as Mirrin, answering to "Hey Mirrin", and never on the old detector.
func TestAConfigFromBeforeMirrinGetsMirrin(t *testing.T) {
	r := newVoiceRig(t, func(c *config.Config, model string) {
		c.Persona, c.Name = "mavrk", "MAVRK"
		c.Channels.Voice.WakeWord = "maverick"
		c.Channels.Voice.WakeModel = filepath.Join(filepath.Dir(model), "hey_maverick.onnx")
	})
	if err := os.WriteFile(filepath.Join(filepath.Dir(r.model), "hey_maverick.onnx"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.want("start", "mirrin", "mirrin", r.model, "bm_george")
	if p := r.d.Persona(); p.Name != "Mirrin" || p.Spoken() != "Mirrin" || len(p.WakeAliases) == 0 || r.d.Config().Name != "Mirrin" {
		t.Fatalf("persona %+v", p)
	}
	r.update(func(c *config.Config) {}) // a save writes him by his new name
	b, _ := os.ReadFile(config.Path())
	if s := string(b); !strings.Contains(s, "persona: mirrin") || !strings.Contains(s, "name: Mirrin") || strings.Contains(s, "maverick") || strings.Contains(s, "MAVRK") {
		t.Fatalf("config.yaml after a save:\n%s", s)
	}
	r.restart()
	r.want("after a restart", "mirrin", "mirrin", r.model, "bm_george")

	// without a "Hey Mirrin" model he is heard by transcription
	if err := os.Remove(r.model); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.want("no model", "mirrin", "mirrin", "", "bm_george")
}

// Ava (persona plain) retired. A config.yaml that chose her, saved by a
// release that wrote her picks into it, still starts: as Mirrin, by his
// name, answering to "Hey Mirrin" in his voice, never a Mirrin called Ava
// who answers to "ava" in hers.
func TestAConfigThatChoseAvaGetsMirrin(t *testing.T) {
	r := newVoiceRig(t, func(c *config.Config, _ string) {
		c.Persona, c.Name = "plain", "Ava"
		c.Channels.Voice.WakeWord, c.Channels.Voice.Voice = "ava", "af_heart"
	})
	r.restart()
	r.want("start", "mirrin", "mirrin", r.model, "bm_george")
	if p := r.d.Persona(); p.Name != "Mirrin" || r.d.Config().Name != "Mirrin" {
		t.Fatalf("persona %+v, name %q", p, r.d.Config().Name)
	}
	r.update(func(c *config.Config) {})
	b, _ := os.ReadFile(config.Path())
	if s := string(b); !strings.Contains(s, "persona: mirrin") || !strings.Contains(s, "name: Mirrin") || strings.Contains(s, "af_heart") {
		t.Fatalf("config.yaml after a save:\n%s", s)
	}
	r.restart()
	r.want("after a restart", "mirrin", "mirrin", r.model, "bm_george")
}

// The persona's voice picks were applied after config.yaml was saved, while
// the record of them was written for the live config, so after a restart the
// last persona's values on disk read as the user's own: Nyra woke on Mirrin's
// detector, and Mirrin woke on "nyra" and spoke with Nyra's voice.
func TestPersonaVoiceSurvivesRestart(t *testing.T) {
	restarts := func(r *voiceRig, id, wakeWord, wakeModel, voice string) {
		r.t.Helper()
		for i := 1; i <= 2; i++ {
			r.restart()
			r.want(fmt.Sprintf("restart %d", i), id, wakeWord, wakeModel, voice)
		}
	}
	t.Run("Mirrin to Nyra", func(t *testing.T) {
		r := newVoiceRig(t, nil)
		r.restart()
		r.want("start", "mirrin", "mirrin", r.model, "bm_george")
		r.update(choosePersona("nyra", "Nyra"))
		r.want("switched", "nyra", "nyra", "", "af_nova")
		restarts(r, "nyra", "nyra", "", "af_nova")
	})
	t.Run("Nyra to Mirrin", func(t *testing.T) {
		r := newVoiceRig(t, func(c *config.Config, _ string) { c.Persona, c.Name = "nyra", "Nyra" })
		r.restart()
		r.want("start", "nyra", "nyra", "", "af_nova")
		r.update(choosePersona("mirrin", "Mirrin"))
		r.want("switched", "mirrin", "mirrin", r.model, "bm_george")
		restarts(r, "mirrin", "mirrin", r.model, "bm_george")
	})
	t.Run("upgrade from a config older versions left", func(t *testing.T) {
		r := newVoiceRig(t, func(c *config.Config, model string) {
			c.Persona, c.Name = "nyra", "Nyra"
			c.Channels.Voice.WakeWord, c.Channels.Voice.WakeModel, c.Channels.Voice.Voice = "nyra", model, "af_nova"
		})
		restarts(r, "nyra", "nyra", "", "af_nova")
		r.update(func(c *config.Config) {})
		if b, _ := os.ReadFile(config.Path()); strings.Contains(string(b), "hey_mirrin") {
			t.Fatalf("the repair was not saved:\n%s", b)
		}
		restarts(r, "nyra", "nyra", "", "af_nova")
	})
	t.Run("persona use while stopped", func(t *testing.T) {
		r := newVoiceRig(t, nil)
		r.restart()
		r.update(func(c *config.Config) {})
		r.cliUse("nyra", "Nyra")
		restarts(r, "nyra", "nyra", "", "af_nova")
		r.update(choosePersona("mirrin", "Mirrin"))
		r.want("switched back", "mirrin", "mirrin", r.model, "bm_george")
		restarts(r, "mirrin", "mirrin", r.model, "bm_george")
	})
	t.Run("the user's wake word and voice stay", func(t *testing.T) {
		r := newVoiceRig(t, nil)
		r.restart()
		r.update(func(c *config.Config) { c.Channels.Voice.WakeWord, c.Channels.Voice.Voice = "computer", "bm_lewis" })
		r.update(choosePersona("nyra", "Nyra"))
		r.want("switched", "nyra", "computer", "", "bm_lewis")
		restarts(r, "nyra", "computer", "", "bm_lewis")
		r.update(choosePersona("mirrin", "Mirrin"))
		restarts(r, "mirrin", "computer", "", "bm_lewis")
	})
}

// A config naming a pack persona by its id from before pack ids were
// namespaced still resolved, but the menus showed nothing as chosen.
func TestLegacyPackPersonaIDIsNormalised(t *testing.T) {
	r := newVoiceRig(t, func(c *config.Config, _ string) {
		c.Persona, c.Name = "butler", ""
		dir := filepath.Join(c.ProtocolsDir, "packs", "starter", "personas")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "butler.yaml"), []byte("name: Jeeves\ncharacter: very correct\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	r.restart()
	if got := r.d.Config().Persona; got != "starter/butler" || r.d.Persona().ID != got {
		t.Fatalf("persona %q, active %q, want starter/butler", got, r.d.Persona().ID)
	}
	r.update(func(c *config.Config) {})
	if b, _ := os.ReadFile(config.Path()); !strings.Contains(string(b), "persona: starter/butler") {
		t.Fatalf("config.yaml keeps the old id:\n%s", b)
	}
}

// A twin running on a config that isn't on disk (`mirrin chat` before any
// save, or config.yaml deleted) judges a switch by what its persona asked
// of that live config, not by the record, which describes a file that is
// gone.
func TestPersonaSwitchWithoutAConfigFile(t *testing.T) {
	r := newVoiceRig(t, nil)
	r.restart()
	r.update(func(c *config.Config) {}) // the record now says what Mirrin asked
	r.stop()
	live, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config.Path()); err != nil {
		t.Fatal(err)
	}
	live.Persona, live.Name = "nyra", "Nyra"
	if r.d, err = New(live, Options{Headless: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatal(err)
	}
	r.want("started as Nyra with no file", "nyra", "nyra", "", "af_nova")
	r.update(choosePersona("mirrin", "Mirrin"))
	r.want("switched to Mirrin", "mirrin", "mirrin", r.model, "bm_george")
}

// The regression: once the old record was gone, a wake word equal to any
// persona's word or name always read as that persona's, so an owner under
// Mirrin who chose "nyra" lost it at the next restart and the next save.
func TestOwnersPersonaWordSurvivesRestart(t *testing.T) {
	r := newVoiceRig(t, nil)
	r.restart()
	r.update(func(c *config.Config) { c.Channels.Voice.WakeWord = "nyra" })
	r.want("chosen", "mirrin", "nyra", "", "bm_george")
	r.restart()
	r.want("after a restart", "mirrin", "nyra", "", "bm_george")
	r.update(func(c *config.Config) { c.LLM.Effort = "low" })
	r.restart()
	r.want("after an unrelated save", "mirrin", "nyra", "", "bm_george")
	if b, _ := os.ReadFile(config.Path()); !strings.Contains(string(b), "wake_word: nyra") {
		t.Fatalf("config.yaml lost the owner's wake word:\n%s", b)
	}
	r.update(choosePersona("nyra", "Nyra"))
	r.update(choosePersona("mirrin", "Mirrin"))
	r.restart()
	r.want("after switching away and back", "mirrin", "nyra", "", "bm_george")
}

// A character voice's pitch and pace come with it; the owner's own voice
// leaves the voice as it is.
func TestACharacterVoiceBringsItsPitch(t *testing.T) {
	penguin := persona.Persona{Name: "Pickoo", Voice: "am_puck", VoicePitch: 520, VoiceSpeed: 1.06, WakeWord: "pickoo"}
	vc := config.Default().Channels.Voice
	resolveVoice(&vc, voiceOverrides{}, penguin)
	if vc.Voice != "am_puck" || vc.Pitch != 520 || vc.Speed != 1.06 {
		t.Fatalf("penguin's voice: %+v", vc)
	}
	mine := "af_bella"
	vc = config.Default().Channels.Voice
	resolveVoice(&vc, voiceOverrides{Voice: &mine}, penguin)
	if vc.Voice != "af_bella" || vc.Pitch != 0 {
		t.Fatalf("owner's own voice: %+v", vc)
	}
	butler := persona.Persona{Name: "Mirrin", Voice: "bm_george", WakeWord: "mirrin"}
	vc = config.Default().Channels.Voice
	resolveVoice(&vc, voiceOverrides{}, butler)
	if vc.Voice != "bm_george" || vc.Pitch != 0 {
		t.Fatalf("butler: %+v", vc)
	}
}
