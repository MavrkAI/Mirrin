package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// `mirrin protocols update` printed a failed pack and still exited 0, so
// scripts and CI could not tell.
func TestUpdatePacksFailsWhenAPackCant(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	ctx := context.Background()
	if err := updatePacks(ctx, dir, nil, func() {}); err != nil {
		t.Fatalf("no packs is not a failure: %v", err)
	}

	pack := filepath.Join(protocols.PacksDir(dir), "gone")
	if err := os.MkdirAll(pack, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pack, "pack.yaml"), []byte("name: gone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "one"},
		{"remote", "add", "origin", fileURL(filepath.Join(t.TempDir(), "deleted"))},
	} {
		if out, err := exec.Command("git", append([]string{"-C", pack}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	reloaded := false
	err := updatePacks(ctx, dir, []string{"--yes"}, func() { reloaded = true })
	if err == nil || !strings.Contains(err.Error(), "1 pack(s) couldn't be updated") {
		t.Fatalf("a pack whose repository is gone should fail the command, got %v", err)
	}
	if reloaded {
		t.Fatal("nothing changed, so nothing should reload")
	}
}
