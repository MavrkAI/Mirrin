package voice

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// fakeKokoro makes dir look like an installed Kokoro (New doesn't start it).
func fakeKokoro(t *testing.T) string {
	dir := t.TempDir()
	for _, f := range []string{"kokoro-v1.0.onnx", "voices-v1.0.bin", "kokoro_tts.py", filepath.Join("venv", pyBin())} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func stubSayVoices(t *testing.T, list string) {
	old := sayVoices
	sayVoices = func() (string, error) { return list, nil }
	t.Cleanup(func() { sayVoices = old })
}

const sayList = `Daniel              en_GB    # Hello! My name is Daniel.
Anna                de_DE    # Hallo! Ich heiße Anna.
Anna (Enhanced)     de_DE    # Hallo! Ich heiße Anna.
Thomas              fr_FR    # Bonjour, je m’appelle Thomas.
Eddy (German (Germany)) de_DE    # Hallo! Ich heiße Eddy.
`

func TestFrenchRepliesUseAFrenchKokoroVoice(t *testing.T) {
	stubSayVoices(t, sayList)
	engine, v := speechFor("fr", "kokoro", "bm_george", true)
	if engine != "kokoro" || v != "ff_siwis" {
		t.Fatalf("got %s %s", engine, v)
	}
	if env := kokoroEnv(nil, v, "fr"); !slices.Contains(env, "LANG_CODE=f") || !slices.Contains(env, "VOICE=ff_siwis") {
		t.Fatalf("env %v", env)
	}
	if env := kokoroEnv(nil, "bm_george", ""); slices.ContainsFunc(env, func(s string) bool { return strings.HasPrefix(s, "LANG_CODE=") }) {
		t.Fatalf("English keeps the voice's own accent: %v", env)
	}

	c := New(config.Voice{Language: "fr", Engine: "kokoro", Voice: "bm_george", KokoroDir: fakeKokoro(t), TTSCommand: ""}, "Mirrin", t.TempDir())
	if c.kokoro == nil || c.kokoro.voice != "ff_siwis" || c.kokoro.lang != "fr" {
		t.Fatalf("kokoro %+v", c.kokoro)
	}
}

func TestGermanRepliesFallBackToAGermanSystemVoice(t *testing.T) {
	stubSayVoices(t, sayList)
	engine, v := speechFor("de", "kokoro", "bm_george", true)
	if engine != "system" || v != "Anna" {
		t.Fatalf("got %s %s", engine, v)
	}
	c := New(config.Voice{Language: "de", Engine: "kokoro", Voice: "bm_george", KokoroDir: fakeKokoro(t)}, "Mirrin", t.TempDir())
	if c.kokoro != nil || c.cfg.Voice != "Anna" {
		t.Fatalf("kokoro %+v, voice %q", c.kokoro, c.cfg.Voice)
	}
}

func TestEnglishSpeechIsUnchanged(t *testing.T) {
	for _, lang := range []string{"", "en", "auto"} {
		if engine, v := speechFor(speechLanguage(lang), "kokoro", "bm_george", true); engine != "kokoro" || v != "bm_george" {
			t.Errorf("%q: got %s %s", lang, engine, v)
		}
	}
	if got := kokoroVoiceFor("fr", "ff_siwis"); got != "ff_siwis" {
		t.Errorf("a configured voice that speaks the language stays: %s", got)
	}
}

// A name long enough to fill its column leaves one space before the locale;
// that voice still counts.
func TestSystemVoiceWithALongName(t *testing.T) {
	stubSayVoices(t, sayList)
	if v := systemVoiceFor("de", "Eddy"); v != "Eddy" {
		t.Fatalf("got %q", v)
	}
	if v := systemVoiceFor("de", ""); v != "Anna" {
		t.Fatalf("got %q", v)
	}
}

// Health names the voice that will actually speak: a French Kokoro voice,
// not the English one in the settings.
func TestHealthReportsTheVoiceThatSpeaks(t *testing.T) {
	stubSayVoices(t, sayList)
	dir, bin := fakeKokoro(t), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sox"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	_, msg, _ := speakingState(config.Voice{Engine: "kokoro", KokoroDir: dir, Voice: "bm_george", Language: "fr"})
	if !strings.Contains(msg, "ff_siwis") {
		t.Fatalf("got %q", msg)
	}
}
