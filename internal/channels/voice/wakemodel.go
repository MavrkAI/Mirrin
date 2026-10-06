package voice

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// The "Hey Mirrin" wake-word model. None has been trained yet, so the
// default persona's name is heard in what is transcribed, as every other
// persona's is, until hey_mirrin.onnx is in the voice folder; then the
// on-device detector listens for it.
//
// A build made with -tags wakemodel embeds assets/hey_maverick.onnx
// (wakemodel_embed.go), the model the maintainers trained when the default
// persona had another name. Its training data (openWakeWord's ACAV100M
// features, CC BY-NC-SA 4.0, and macOS voices) isn't cleared for commercial
// use, so no release carries it (docs/wake-word.md). It listens for "Hey
// Maverick", so setup puts it in the voice folder under that name, for a
// wake_model that names it, and never for "Hey Mirrin".

// heyMirrinFile is the "Hey Mirrin" model's name in the voice folder.
const heyMirrinFile = "hey_mirrin.onnx"

// bundledWakeFile is the name the model a -tags wakemodel build carries
// gets in the voice folder.
const bundledWakeFile = "hey_maverick.onnx"

// mirrinWord reports whether a wake word is Mirrin's, the one the "Hey
// Mirrin" model listens for.
func mirrinWord(word string) bool {
	w := strings.ToLower(strings.TrimSpace(word))
	return w == "" || w == "mirrin" || w == "hey mirrin"
}

// HasWakeModel reports whether the on-device detector has a model for the
// configured wake word: one set in wake_model, or "Hey Mirrin" in the
// voice folder. Other names, and Mirrin's when no model is there, are
// matched in the transcript instead.
func HasWakeModel(cfg config.Voice) bool {
	if cfg.WakeModel != "" {
		return true
	}
	return mirrinWord(cfg.WakeWord) && cfg.KokoroDir != "" && fileHere(filepath.Join(cfg.KokoroDir, heyMirrinFile))
}

// WakeModelPath returns the model to use: a configured name or path, else
// "Hey Mirrin" in the voice folder, else "" when there is none. It never
// falls back to one of openWakeWord's stock models: each answers to another
// name.
func WakeModelPath(dir, configured string) string {
	if configured != "" {
		return configured
	}
	if p := filepath.Join(dir, heyMirrinFile); fileHere(p) {
		return p
	}
	return ""
}

// WakeModelAvailable reports whether a persona's wake model (a file in the
// voice folder dir, or a path) is on this computer, or comes with this
// build for voice setup to put there.
func WakeModelAvailable(dir, model string) bool {
	switch {
	case model == "":
		return false
	case model == bundledWakeFile && len(bundledWakeModel) > 0:
		return true
	case !filepath.IsAbs(model):
		if dir == "" {
			return false
		}
		model = filepath.Join(dir, model)
	}
	return fileHere(model)
}

// installWakeModel puts the model this build carries in the voice folder,
// and reports whether it did. A build without one leaves the folder as it
// is, so a model already there (from an earlier version, or one the owner
// trained) keeps working.
func installWakeModel(dir string) (bool, error) {
	if len(bundledWakeModel) == 0 {
		return false, nil
	}
	if err := os.WriteFile(filepath.Join(dir, bundledWakeFile), bundledWakeModel, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// detectorNeeded reports whether anything listens with the on-device
// detector once voice setup has run: a model is in place for the wake word.
// When nothing does, a failed openWakeWord install costs nothing yet.
func detectorNeeded(v config.Voice) bool {
	return HasWakeModel(v)
}

// wakeByTranscript says why the wake word is matched in what is transcribed
// instead of heard by the on-device detector, or "" when the detector has a
// model for it.
func wakeByTranscript(v config.Voice) string {
	switch {
	case HasWakeModel(v):
		return ""
	case !mirrinWord(v.WakeWord):
		return "there's no wake-word model for this name"
	}
	return "this build comes without a wake-word model for now"
}

// fileHere reports whether p is a file with something in it.
func fileHere(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}
