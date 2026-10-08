//go:build windows

package service

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/homelock"
)

// `mirrin service install` on Windows puts an entry in this user's Startup
// folder that starts the tray with MIRRIN_HOME and MIRRIN_SERVICE, and no
// Windows service.
func TestWindowsInstallWritesAStartupEntry(t *testing.T) {
	home := t.TempDir()
	appData := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("APPDATA", appData)
	s, err := newStartup(filepath.Join(home, "data"), nil, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var launched []string
	running := false
	s.launch = func(exe string, env []string) error { launched = env; running = true; return nil }
	s.stopTwin = func() error { running = false; return nil }
	s.running = func() bool { return running }
	s.elevated = nil // never the real token
	s.wait = time.Second

	if err := s.control("install"); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "Mirrin.cmd")
	b, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	env := startupEnv(b)
	if env["MIRRIN_HOME"] != home || env[config.ServiceEnv] != "1" {
		t.Fatalf("entry sets %v:\n%s", env, b)
	}
	exe, _ := os.Executable()
	if !strings.Contains(string(b), `"`+strings.ReplaceAll(exe, "%", "%%")+`" tray`) {
		t.Fatalf("entry doesn't start this program's tray:\n%s", b)
	}
	if !slices.Contains(launched, "MIRRIN_HOME="+home) || !slices.Contains(launched, config.ServiceEnv+"=1") {
		t.Fatalf("started now with %v", launched)
	}
}

// Nothing holds a home that was never claimed.
func TestWindowsLockHoldersOfAFreeHome(t *testing.T) {
	if got := lockHolders(homelock.Path(t.TempDir())); len(got) != 0 {
		t.Fatalf("lockHolders = %v", got)
	}
}
