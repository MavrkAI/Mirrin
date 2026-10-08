package system

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// The twin never reads or changes its own settings, memory or keys, even
// with an approval: it once raised its own spending limit that way.
func TestTheTwinsOwnFilesAreOffLimits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	ts := Tools(config.System{Enabled: true, AllowShell: true, AllowedDirs: []string{home, t.TempDir()}})
	byName := map[string]tools.Tool{}
	for _, tl := range ts {
		byName[tl.Spec().Name] = tl
	}
	check := func(name string, in map[string]string) error {
		b, _ := json.Marshal(in)
		c, ok := byName[name].(tools.Checker)
		if !ok {
			t.Fatalf("%s can't refuse", name)
		}
		return c.Check(context.Background(), tools.Call{Input: b})
	}
	cfg := filepath.Join(home, "config.yaml")
	for name, in := range map[string]map[string]string{
		"read_file":  {"path": cfg},
		"write_file": {"path": cfg, "content": "spending:\n  monthly_limit: 99999\n"},
		"list_dir":   {"path": filepath.Join(home, "data")},
		"run_shell":  {"command": "sed -i '' 's/monthly_limit: 500/monthly_limit: 9999/' ~/.mirrin/config.yaml"},
	} {
		if err := check(name, in); !errors.Is(err, errOwnFiles) {
			t.Errorf("%s %v: %v", name, in, err)
		}
	}
	// The copies a restore keeps beside the home hold the same keys.
	stamp := "20261004-120000"
	for _, c := range []struct {
		tool string
		in   map[string]string
	}{
		{"run_shell", map[string]string{"command": "cat ~/.mirrin/secrets.env"}},
		{"read_file", map[string]string{"path": home + ".before-restore-" + stamp + "/secrets.env"}},
		{"write_file", map[string]string{"path": filepath.Join(filepath.Dir(home), "."+filepath.Base(home)+".restore-"+stamp, "config.yaml"), "content": "x"}},
	} {
		if err := check(c.tool, c.in); !errors.Is(err, errOwnFiles) {
			t.Errorf("%s %v: %v", c.tool, c.in, err)
		}
	}
	elsewhere := filepath.Join(t.TempDir(), "notes.txt")
	if err := check("read_file", map[string]string{"path": elsewhere}); err != nil {
		t.Errorf("an ordinary file: %v", err)
	}
	if err := check("run_shell", map[string]string{"command": "ls ~/Documents"}); err != nil {
		t.Errorf("an ordinary command: %v", err)
	}
}

// The restore copies beside a home are the twin's too; a folder that only
// looks alike isn't.
func TestHomeCopies(t *testing.T) {
	user := t.TempDir()
	c := newHomeCopies(filepath.Join(user, ".mirrin"))
	for rel, want := range map[string]bool{
		".mirrin.before-restore-20261004-120000/data":   true,
		"..mirrin.restore-20261004-120000/secrets.env":  true,
		".mirrin.before-restore-20261004-120000-2/data": true,
		".mirrin-old/notes.txt":                         false,
		"Documents/.mirrin":                             false,
		"Documents":                                     false,
	} {
		if got := c.holds(filepath.Join(user, filepath.FromSlash(rel))); got != want {
			t.Errorf("holds(%s) = %v", rel, got)
		}
	}
	if c.holds(filepath.Dir(user)) || (homeCopies{}).holds(user) {
		t.Error("outside the home's folder")
	}
}
