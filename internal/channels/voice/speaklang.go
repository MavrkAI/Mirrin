package voice

import (
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// Speaking replies in the language the owner speaks. The language is the
// one voice is set to hear (channels.voice.language, which setup takes from
// the computer's own language). Kokoro speaks a handful of languages; for
// the others the system voice for that language answers instead.

// kokoroLangCodes are Kokoro's one-letter language codes (LANG_CODE), by
// language. English is left to the voice's own accent (a* or b*).
var kokoroLangCodes = map[string]string{
	"es": "e", "fr": "f", "hi": "h", "it": "i", "ja": "j", "pt": "p", "zh": "z",
}

// speechLanguage is the language replies are spoken in: "" for English, or
// when the setting says to detect it or is empty.
func speechLanguage(setting string) string {
	l := strings.ToLower(strings.TrimSpace(setting))
	if l == "" || l == "auto" || l == "en" {
		return ""
	}
	if c := langCode(l); c != "" {
		return c
	}
	return l
}

// kokoroVoiceFor is the Kokoro voice to speak lang with: the configured one
// when it speaks lang, else the first voice tagged with lang. "" when Kokoro
// has none.
func kokoroVoiceFor(lang, configured string) string {
	if lang == "" {
		return configured
	}
	first := ""
	for _, o := range kokoroVoices {
		if o.Lang != lang {
			continue
		}
		if o.ID == configured {
			return configured
		}
		if first == "" {
			first = o.ID
		}
	}
	return first
}

// sayVoices lists the macOS voices ("say -v ?"), replaceable in tests.
var sayVoices = func() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", exec.ErrNotFound
	}
	out, err := exec.Command("say", "-v", "?").Output()
	return string(out), err
}

// systemVoiceFor is an installed system voice that speaks lang: the
// configured one when it does, else the first listed. "" when none does or
// the system can't say.
func systemVoiceFor(lang, configured string) string {
	list, err := sayVoices()
	if err != nil {
		return ""
	}
	first := ""
	for _, line := range strings.Split(list, "\n") {
		// The locale token ends the name, however many spaces precede it:
		// "Eddy (German (Germany)) de_DE    # Hallo!" has only one.
		m := sayLine.FindStringSubmatch(line)
		if m == nil || !strings.HasPrefix(strings.ToLower(m[2]), lang+"_") {
			continue
		}
		base := strings.TrimSpace(strings.SplitN(strings.TrimSpace(m[1]), " (", 2)[0])
		if strings.EqualFold(base, configured) {
			return base
		}
		if first == "" {
			first = base
		}
	}
	return first
}

// sayLine splits a "say -v ?" line into the voice's name and its locale.
var sayLine = regexp.MustCompile(`^(.+?)\s+([a-z]{2,3}_[A-Za-z0-9]+)\s+#`)

// speechFor settles the speech-out engine and voice for lang, given the
// engine already chosen and whether Kokoro is installed: Kokoro keeps
// speaking when it has a voice for the language, otherwise the system voice
// for it answers. ElevenLabs is multilingual and a custom command is the
// owner's own, so both stay.
func speechFor(lang, engine, voice string, kokoroOK bool) (string, string) {
	if lang == "" {
		return engine, voice
	}
	switch engine {
	case "kokoro":
		if !kokoroOK {
			return engine, systemVoiceFor(lang, "") // New falls back to the system voice
		}
		if v := kokoroVoiceFor(lang, voice); v != "" {
			return engine, v
		}
		return "system", systemVoiceFor(lang, "")
	case "system":
		return engine, systemVoiceFor(lang, voice)
	}
	return engine, voice
}

// kokoroEnv is the helper's environment for a voice and language.
func kokoroEnv(base []string, voice, lang string) []string {
	env := append(base, "VOICE="+voice)
	if code := kokoroLangCodes[lang]; code != "" {
		env = append(env, "LANG_CODE="+code)
	}
	return env
}
