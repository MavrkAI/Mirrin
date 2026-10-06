//go:build !wakemodel

package voice

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// The public build carries no wake-word model (wakemodel.go). Setup
// installs none and leaves one already in the voice folder as it is.
func TestThePublicBuildCarriesNoWakeModel(t *testing.T) {
	if len(bundledWakeModel) != 0 {
		t.Fatal("a build without -tags wakemodel carries a wake-word model")
	}
	dir := t.TempDir()
	if installed, err := installWakeModel(dir); installed || err != nil {
		t.Fatalf("installed: %v %v", installed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, heyMirrinFile)); !os.IsNotExist(err) {
		t.Fatalf("a model was written: %v", err)
	}
	if WakeModelAvailable(dir, heyMirrinFile) {
		t.Fatal("the welcome page would promise Mirrin hears his name first")
	}

	mine := filepath.Join(dir, heyMirrinFile)
	if err := os.WriteFile(mine, []byte("the owner's model"), 0o644); err != nil {
		t.Fatal(err)
	}
	if installed, err := installWakeModel(dir); installed || err != nil {
		t.Fatalf("installed over the owner's model: %v %v", installed, err)
	}
	if b, _ := os.ReadFile(mine); string(b) != "the owner's model" {
		t.Fatalf("the owner's model was replaced: %q", b)
	}
	if !WakeModelAvailable(dir, heyMirrinFile) {
		t.Fatal("the model already here doesn't count")
	}
}

// Setup and the Listening row say once, plainly, that Mirrin's name is heard
// in what is transcribed because this build has no model: an OK step, not
// an error.
func TestNoModelIsSaidPlainly(t *testing.T) {
	dir := t.TempDir()
	v := config.Voice{WakeWord: "mirrin", KokoroDir: dir}
	r := runWakeSetup(t, v, func(context.Context, string, io.Writer) error { return nil })
	want := `this build comes without a wake-word model for now, so I listen for "Hey Mirrin" in what I transcribe, a beat slower (docs/wake-word.md shows how to train your own)`
	if !r.OK || r.Notes != want {
		t.Fatalf("wake step %+v", r)
	}
	if got := listeningFor(v); got != `listening for "Hey Mirrin" in what I transcribe, a beat slower: this build comes without a wake-word model for now` {
		t.Fatalf("listening row %q", got)
	}
	for _, scary := range []string{"error", "fail", "missing"} {
		if strings.Contains(strings.ToLower(r.Notes+listeningFor(v)), scary) {
			t.Fatalf("%q in %q", scary, r.Notes)
		}
	}
}

// Voice setup said it twice: SetupWake's own line that the build comes
// without a model, then the wake step's. Now the step says it, once.
func TestNoModelIsSaidOnce(t *testing.T) {
	dir, _ := fakeVenv(t)
	r, said := runWakeSetupSaid(t, config.Voice{WakeWord: "mirrin", KokoroDir: dir}, SetupWake)
	if !r.OK || strings.Count(said+"\n"+r.Notes, "in what I transcribe") != 1 || strings.Count(said+"\n"+r.Notes, "without a wake-word model") != 1 {
		t.Fatalf("wake step %+v, setup said:\n%s", r, said)
	}
}

// Nothing in this build listens with openWakeWord until the owner adds a
// model, so an install that fails doesn't fail Mirrin's setup.
func TestAFailedWakeInstallIsNoFaultWithoutAModel(t *testing.T) {
	r, said := runWakeSetupSaid(t, config.Voice{WakeWord: "mirrin", KokoroDir: t.TempDir()}, failWake)
	want := `this build comes without a wake-word model for now, so I listen for "Hey Mirrin" in what I transcribe, a beat slower (docs/wake-word.md shows how to train your own)`
	if !r.OK || r.Notes != want || strings.Contains(said, "didn't work") {
		t.Fatalf("wake step %+v, setup said:\n%s", r, said)
	}
}
