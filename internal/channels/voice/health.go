package voice

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
)

// The voice rows of the self-checks (mirrin doctor, the Health page). They
// are always there, because push-to-talk users (`mirrin voice`) need them as
// much as always-on ones: until voice is set up they read "not set up",
// which is not a problem; after that each part is checked for real. Each
// check reads the config when it runs, so setting voice up while the twin
// runs shows at the next check.

// Probe is what the checks need from the twin.
type Probe struct {
	Config    func() config.Voice
	Listening func() bool  // always-on listening is running
	Start     func() error // starts always-on listening (the repair)
}

// SetUp reports whether voice is set up, or turned on, on this machine.
func SetUp(v config.Voice) bool {
	if v.Enabled || v.Mode == "wake" {
		return true
	}
	st, err := os.Stat(v.WhisperModel)
	return err == nil && st.Size() > 0
}

const notSetUp = "not set up (run mirrin voice setup to talk out loud)"

// Checks are the voice rows of the self-checks.
func Checks(p Probe) []health.Check {
	gate := func(run func(v config.Voice) (health.State, string, string)) func(context.Context) (health.State, string, string) {
		return func(context.Context) (health.State, string, string) {
			v := p.Config()
			if !SetUp(v) {
				return health.Off, notSetUp, ""
			}
			return run(v)
		}
	}
	var start func(context.Context) error
	if p.Start != nil {
		start = func(context.Context) error { return p.Start() }
	}
	return []health.Check{
		health.Func("ears.whisper", "Hearing (whisper)", gate(hearingState), nil),
		health.Func("ears.recorder", "Hearing (microphone)", gate(recorderState), nil),
		health.Func("ears.model", "Hearing (model)", gate(modelState), nil),
		health.Func("voice.speaking", "Speaking", gate(speakingState), nil),
		health.Func("voice.listening", "Listening", func(context.Context) (health.State, string, string) {
			v := p.Config()
			if !(v.Enabled && v.Mode == "wake") {
				if !SetUp(v) {
					return health.Off, notSetUp, ""
				}
				return health.Off, "always-on listening is off; talk with mirrin voice (push-to-talk)", ""
			}
			if p.Listening != nil && !p.Listening() {
				return health.Fail, "always-on listening isn't running", "turn Listening on from the menu, or restart me; after allowing microphone access a restart is needed"
			}
			return health.OK, listeningFor(v), ""
		}, start),
	}
}

// listeningFor is the Listening row's detail: the wake phrase, and, when no
// model lets the detector hear it, that it is heard in what is transcribed
// and why. That isn't a fault, so the row stays OK rather than warning.
func listeningFor(v config.Voice) string {
	s := fmt.Sprintf("listening for \"Hey %s\"", titleWords(orDefault(v.WakeWord, "mirrin")))
	if why := wakeByTranscript(v); why != "" && v.WakeEngine != "transcribe" {
		s += " in what I transcribe, a beat slower: " + why
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func hearingState(v config.Voice) (health.State, string, string) {
	bin := orDefault(v.WhisperBin, "whisper-cli")
	if _, err := exec.LookPath(bin); err != nil {
		return health.Fail, bin + " not found", installHint("whisper")
	}
	detail := bin + " present"
	if !serverDisabled() && findWhisperServer(bin) != "" {
		detail += "; whisper-server keeps the model loaded between questions"
	}
	return health.OK, detail, ""
}

func recorderState(v config.Voice) (health.State, string, string) {
	if v.RecordCommand != "" {
		return health.OK, "custom record_command", ""
	}
	if _, err := exec.LookPath("sox"); err != nil {
		return health.Fail, "sox not found", installHint("sox")
	}
	return health.OK, "sox present", ""
}

func modelState(v config.Voice) (health.State, string, string) {
	if v.WhisperModel == "" {
		return health.Fail, "no hearing model is set", "run mirrin voice setup"
	}
	st, err := os.Stat(v.WhisperModel)
	if err != nil || st.Size() == 0 {
		return health.Fail, v.WhisperModel + " is missing", "run mirrin voice setup"
	}
	if want, ok := knownFiles[filepath.Base(v.WhisperModel)]; ok && st.Size() != want.size {
		return health.Warn, v.WhisperModel + " is incomplete", "run mirrin voice setup to finish the download"
	}
	if lang := v.Language; lang != "" && lang != "en" && lang != "auto" && EnglishOnly(v.WhisperModel) {
		return health.Warn, fmt.Sprintf("set to hear %s, but the %s model only knows English", LanguageName(lang), WhisperModelName(v.WhisperModel)),
			"run mirrin voice setup to get the multilingual model"
	}
	return health.OK, fmt.Sprintf("%s (%d MB)", WhisperModelName(v.WhisperModel), st.Size()>>20), ""
}

// speakingState checks the voice that will speak, without starting it.
func speakingState(v config.Voice) (health.State, string, string) {
	key := ElevenLabsKey(v)
	engine := v.Engine
	if engine == "" || engine == "auto" {
		switch {
		case v.TTSCommand != "":
			engine = "command"
		case key != "":
			engine = "elevenlabs"
		case kokoroReady(v.KokoroDir):
			engine = "kokoro"
		default:
			engine = "system"
		}
	}
	_, soxErr := exec.LookPath("sox")
	switch engine {
	case "elevenlabs":
		if key == "" {
			return health.Warn, "ElevenLabs is chosen but there's no key, so the system voice speaks", "set ELEVENLABS_API_KEY, or pick another voice from the menu"
		}
		if soxErr != nil {
			return health.Warn, "ElevenLabs, but sox is missing, so the plain system voice speaks", installHint("sox")
		}
		return health.OK, "ElevenLabs (" + orDefault(v.Voice, "Daniel") + ")", ""
	case "kokoro":
		if !kokoroReady(v.KokoroDir) {
			return health.Warn, "Kokoro is chosen but not installed, so the system voice speaks", "run mirrin voice setup"
		}
		if soxErr != nil {
			return health.Warn, "Kokoro, but sox is missing, so the plain system voice speaks", installHint("sox")
		}
		// Report the voice that will actually speak the reply's language.
		eng, vc := speechFor(speechLanguage(v.Language), "kokoro", newKokoro(v.KokoroDir, v.Voice, v.Speed).voice, true)
		if eng == "system" {
			return health.OK, "system voice (" + orDefault(vc, "default") + "), as Kokoro doesn't speak " + v.Language, ""
		}
		return health.OK, "Kokoro offline voice (" + vc + ")", ""
	case "command":
		if v.TTSCommand == "" {
			return health.Warn, "a custom voice command is chosen but tts_command is empty", "set channels.voice.tts_command, or pick a voice from the menu"
		}
		return health.OK, "custom command", ""
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		return health.OK, "system voice", ""
	}
	for _, b := range []string{"spd-say", "espeak"} {
		if _, err := exec.LookPath(b); err == nil {
			return health.OK, "system voice (" + b + ")", ""
		}
	}
	return health.Warn, "no system voice found", "install espeak, or run mirrin voice setup for the Kokoro voice"
}

// installHint says how to get a missing tool on this system.
func installHint(tool string) string {
	h := InstallHint(runtime.GOOS, tool)
	if tool == "whisper" && runtime.GOOS == "darwin" {
		h += ", then run mirrin voice setup"
	}
	return h
}

// InstallHint says how to install a tool voice needs ("whisper" or "sox")
// on an operating system (a GOOS value).
func InstallHint(goos, tool string) string {
	switch goos {
	case "darwin":
		if tool == "whisper" {
			return "brew install whisper-cpp"
		}
		return "brew install sox"
	case "windows":
		if tool == "whisper" {
			return "download whisper.cpp from github.com/ggml-org/whisper.cpp/releases and put whisper-cli on your PATH"
		}
		return "install SoX (sourceforge.net/projects/sox) and put it on your PATH"
	}
	if tool == "whisper" {
		return "install whisper.cpp (your distribution's whisper-cpp package, or github.com/ggml-org/whisper.cpp)"
	}
	return "install sox (for example: sudo apt install sox)"
}
