package voice

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A character voice's pitch is applied to the clip Kokoro wrote, in place.
func TestRepitchShiftsAClipInPlace(t *testing.T) {
	if _, err := exec.LookPath("sox"); err != nil {
		t.Skip("needs sox")
	}
	clip := filepath.Join(t.TempDir(), "c.wav")
	if err := exec.Command("sox", "-n", "-r", "24000", "-c", "1", clip, "synth", "0.3", "sine", "440").Run(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(clip)
	k := &kokoro{pitch: 520}
	if err := k.repitch(context.Background(), clip); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(clip)
	if string(before) == string(after) || len(after) < 1000 {
		t.Fatalf("clip unchanged or broken: %d -> %d bytes", len(before), len(after))
	}
	out, _ := exec.Command("sox", "--i", "-r", clip).Output()
	if strings.TrimSpace(string(out)) != "24000" {
		t.Fatalf("rate %q", out)
	}
	if err := (&kokoro{}).repitch(context.Background(), clip); err != nil {
		t.Fatal(err)
	}
}
