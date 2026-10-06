package voice

import (
	"context"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Option is one selectable voice.
type Option struct {
	Engine string // kokoro | system | elevenlabs
	ID     string // value for config voice
	Label  string
	Group  string // menu section
	Lang   string // language code the voice speaks ("en", "fr"); "" for many
}

var kokoroVoices = []Option{
	{"kokoro", "bm_george", "George", "Kokoro · British", "en"},
	{"kokoro", "bm_lewis", "Lewis", "Kokoro · British", "en"},
	{"kokoro", "bm_daniel", "Daniel", "Kokoro · British", "en"},
	{"kokoro", "bm_fable", "Fable", "Kokoro · British", "en"},
	{"kokoro", "bf_emma", "Emma", "Kokoro · British", "en"},
	{"kokoro", "bf_isabella", "Isabella", "Kokoro · British", "en"},
	{"kokoro", "bf_alice", "Alice", "Kokoro · British", "en"},
	{"kokoro", "bf_lily", "Lily", "Kokoro · British", "en"},
	{"kokoro", "am_michael", "Michael", "Kokoro · American", "en"},
	{"kokoro", "am_fenrir", "Fenrir", "Kokoro · American", "en"},
	{"kokoro", "am_puck", "Puck", "Kokoro · American", "en"},
	{"kokoro", "am_adam", "Adam", "Kokoro · American", "en"},
	{"kokoro", "am_liam", "Liam", "Kokoro · American", "en"},
	{"kokoro", "am_onyx", "Onyx", "Kokoro · American", "en"},
	{"kokoro", "am_eric", "Eric", "Kokoro · American", "en"},
	{"kokoro", "af_heart", "Heart", "Kokoro · American", "en"},
	{"kokoro", "af_bella", "Bella", "Kokoro · American", "en"},
	{"kokoro", "af_nova", "Nova", "Kokoro · American", "en"},
	{"kokoro", "af_sarah", "Sarah", "Kokoro · American", "en"},
	{"kokoro", "af_sky", "Sky", "Kokoro · American", "en"},
	{"kokoro", "af_nicole", "Nicole", "Kokoro · American", "en"},
	{"kokoro", "ff_siwis", "Siwis", "Kokoro · French", "fr"},
	{"kokoro", "ef_dora", "Dora", "Kokoro · Spanish", "es"},
	{"kokoro", "em_alex", "Alex", "Kokoro · Spanish", "es"},
	{"kokoro", "if_sara", "Sara", "Kokoro · Italian", "it"},
	{"kokoro", "im_nicola", "Nicola", "Kokoro · Italian", "it"},
	{"kokoro", "pf_dora", "Dora", "Kokoro · Portuguese", "pt"},
	{"kokoro", "pm_alex", "Alex", "Kokoro · Portuguese", "pt"},
	{"kokoro", "hf_alpha", "Alpha", "Kokoro · Hindi", "hi"},
	{"kokoro", "hm_omega", "Omega", "Kokoro · Hindi", "hi"},
	{"kokoro", "jf_alpha", "Alpha", "Kokoro · Japanese", "ja"},
	{"kokoro", "jm_kumo", "Kumo", "Kokoro · Japanese", "ja"},
	{"kokoro", "zf_xiaoxiao", "Xiaoxiao", "Kokoro · Chinese", "zh"},
	{"kokoro", "zm_yunxi", "Yunxi", "Kokoro · Chinese", "zh"},
}

// ElevenLabs premade voices worth offering; names resolve to ids at runtime.
var elevenVoices = []Option{
	{"elevenlabs", "Charlie", "Charlie (Australian)", "ElevenLabs", ""},
	{"elevenlabs", "Daniel", "Daniel (British)", "ElevenLabs", ""},
	{"elevenlabs", "George", "George (British)", "ElevenLabs", ""},
	{"elevenlabs", "Lily", "Lily (British)", "ElevenLabs", ""},
	{"elevenlabs", "Brian", "Brian (American)", "ElevenLabs", ""},
	{"elevenlabs", "Rachel", "Rachel (American)", "ElevenLabs", ""},
	{"elevenlabs", "Sarah", "Sarah (American)", "ElevenLabs", ""},
}

// Catalog lists the voices available on this machine for the given config.
func Catalog(cfg config.Voice) []Option {
	var out []Option
	if kokoroReady(cfg.KokoroDir) {
		out = append(out, kokoroVoices...)
	}
	out = append(out, systemVoices()...)
	if ElevenLabsKey(cfg) != "" {
		out = append(out, elevenVoices...)
	}
	return out
}

// ElevenLabsKey is the ElevenLabs key to speak with: the config's own, else
// ELEVENLABS_API_KEY from the environment or the saved secrets file. It is
// read each time, so a key saved while the twin runs is found.
func ElevenLabsKey(cfg config.Voice) string {
	if cfg.ElevenLabsAPIKey != "" {
		return cfg.ElevenLabsAPIKey
	}
	return config.Secret("ELEVENLABS_API_KEY")
}

// KokoroInstalled reports whether the offline voice is set up.
func KokoroInstalled(cfg config.Voice) bool { return kokoroReady(cfg.KokoroDir) }

// systemVoices lists the OS synthesiser's English voices.
func systemVoices() []Option {
	if runtime.GOOS != "darwin" {
		return []Option{{"system", "", "Default system voice", "System", ""}}
	}
	out, err := exec.Command("say", "-v", "?").Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var opts []Option
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		rest := strings.Fields(strings.TrimSpace(parts[1]))
		if len(rest) == 0 || !strings.HasPrefix(rest[0], "en_") {
			continue
		}
		base := strings.TrimSpace(strings.SplitN(name, " (", 2)[0])
		if seen[base] {
			continue
		}
		seen[base] = true
		region := map[string]string{"en_AU": "Australian", "en_GB": "British", "en_US": "American", "en_IE": "Irish", "en_IN": "Indian", "en_ZA": "South African", "en_SC": "Scottish"}[rest[0]]
		if region == "" {
			region = rest[0]
		}
		label := base + " (" + region + ")"
		if strings.Contains(name, "(") {
			label += " ★" // Enhanced/Premium installed
		}
		opts = append(opts, Option{"system", base, label, "macOS", "en"})
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].Label < opts[j].Label })
	return opts
}

// PreviewLine is what "Preview voice" says, addressing the owner the way
// the twin does ("" for no form of address).
func PreviewLine(address string) string {
	greeting := "Good afternoon"
	if a := strings.TrimSpace(address); a != "" {
		greeting += ", " + a
	}
	return greeting + ". Your three o'clock has moved to Thursday, and I've sent Priya the updated deck."
}

// Preview speaks a sample line with the given settings, addressing the
// owner as address.
func Preview(ctx context.Context, cfg config.Voice, name, dataDir, address string) {
	Say(ctx, cfg, name, dataDir, PreviewLine(address))
}

// Say speaks one line with the given settings, start to finish.
func Say(ctx context.Context, cfg config.Voice, name, dataDir, line string) {
	c := New(cfg, name, dataDir)
	defer func() {
		if c.kokoro != nil {
			c.kokoro.stop()
		}
	}()
	c.Speak(ctx, line)
}
