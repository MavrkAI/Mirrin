package identity

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// A TypeSafe key written into the config never travels in an export. On the
// new machine it is reported missing, unless TYPESAFE_API_KEY is already
// saved there, as the Accounts card saves it.
func TestExportScrubsTheJevKey(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	src := twinHome(t, func(c *config.Config) {
		c.Jev = config.Jev{Enabled: true, TypeSafeAPIKey: "SECRET-typesafe-key"}
	})
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	if _, err := Export(src, out, false); err != nil {
		t.Fatal(err)
	}
	for name, b := range readArchive(t, out) {
		if strings.Contains(string(b), "SECRET-typesafe-key") {
			t.Fatalf("%s carries the TypeSafe key", name)
		}
	}

	bare := filepath.Join(t.TempDir(), "bare")
	t.Setenv("MIRRIN_HOME", bare)
	r, err := Import(bare, out)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.Missing, "jev.typesafe_api_key") {
		t.Fatalf("missing %v, want the TypeSafe key listed", r.Missing)
	}

	saved := filepath.Join(t.TempDir(), "saved")
	t.Setenv("MIRRIN_HOME", saved)
	if err := config.SaveSecrets(map[string]string{"TYPESAFE_API_KEY": "ts-here"}); err != nil {
		t.Fatal(err)
	}
	r, err = Import(saved, out)
	if err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(t, saved)
	if slices.Contains(r.Missing, "jev.typesafe_api_key") || cfg.JevKey() != "ts-here" || !cfg.JevOn() {
		t.Fatalf("missing %v, key %q", r.Missing, cfg.JevKey())
	}
}
