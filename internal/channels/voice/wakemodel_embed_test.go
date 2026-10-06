//go:build wakemodel

package voice

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// A build made with -tags wakemodel carries the "Hey Maverick" model
// trained when the default persona had that name, and setup puts it in the
// voice folder under its own name. It is never taken for "Hey Mirrin".
func TestTheWakeModelBuildCarriesHeyMaverick(t *testing.T) {
	asset, err := os.ReadFile(filepath.Join("assets", bundledWakeFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(bundledWakeModel) == 0 || !bytes.Equal(bundledWakeModel, asset) {
		t.Fatalf("carries %d bytes, want the %d of %s", len(bundledWakeModel), len(asset), bundledWakeFile)
	}
	dir := t.TempDir()
	if !WakeModelAvailable(dir, bundledWakeFile) {
		t.Fatal("the model this build carries doesn't count as available")
	}
	if WakeModelAvailable(dir, heyMirrinFile) {
		t.Fatal("the welcome page would promise Mirrin hears his name first")
	}
	if installed, err := installWakeModel(dir); !installed || err != nil {
		t.Fatalf("installed: %v %v", installed, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, bundledWakeFile)); !bytes.Equal(b, asset) {
		t.Fatal("setup didn't put the model in the voice folder")
	}
	v := config.Voice{WakeWord: "mirrin", KokoroDir: dir}
	if HasWakeModel(v) || !strings.Contains(listeningFor(v), "in what I transcribe") {
		t.Fatalf("Mirrin borrowed the Hey Maverick model: %v %q", HasWakeModel(v), listeningFor(v))
	}
	if !HasWakeModel(config.Voice{WakeWord: "maverick", WakeModel: filepath.Join(dir, bundledWakeFile), KokoroDir: dir}) {
		t.Fatal("a wake_model naming it isn't used")
	}
}
