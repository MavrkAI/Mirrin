package service

import (
	"io"
	"path/filepath"
	"slices"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// A custom tool's key, named in its tool.yaml, is kept for the background
// service like any configured key.
func TestInstallKeepsACustomToolsKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("FOO_KEY", "foo-secret")
	cfg := config.Default()
	writeFile(t, filepath.Join(cfg.ToolsDir, "parcel", "tool.yaml"), "name: parcel\ncommand: ./run.sh\nenv: [FOO_KEY, \"$BAR_TOKEN\", \"not a name\"]\n")
	writeFile(t, filepath.Join(cfg.ToolsDir, "broken", "tool.yaml"), "env: [\n")

	envs := cfg.SecretEnvs()
	for _, want := range []string{"FOO_KEY", "BAR_TOKEN"} {
		if !slices.Contains(envs, want) {
			t.Fatalf("SecretEnvs lacks %s: %v", want, envs)
		}
	}
	if slices.Contains(envs, "not a name") {
		t.Fatalf("an invalid name was kept: %v", envs)
	}

	in := installer{out: io.Discard, secretEnvs: envs}
	if err := in.keepSecrets(); err != nil {
		t.Fatal(err)
	}
	got, _ := config.ReadSecrets()
	if got["FOO_KEY"] != "foo-secret" {
		t.Fatalf("FOO_KEY not kept for the service: %v", got)
	}
}
