package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// The welcome page asks the twin whether a persona's wake model is here: a
// model in the voice folder counts, one the persona only names doesn't.
func TestWelcomeAsksWhetherAWakeModelIsHere(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	dir := td.Config().Channels.Voice.KokoroDir
	if td.HasWakeModel("hey_nova.onnx") || td.HasWakeModel("") {
		t.Fatal("a model that isn't here counts")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hey_nova.onnx"), []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !td.HasWakeModel("hey_nova.onnx") {
		t.Fatal("the model in the voice folder doesn't count")
	}
}
