package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/identity"
)

// fakeImportMachine stands in for the service manager and a running twin
// around `mirrin identity import`, and records what happened in order.
type fakeImportMachine struct {
	calls   []string
	holding func() // the running twin's claim on the home; nil once it quit
}

func setupImport(t *testing.T, installed bool, result identity.Result) (*fakeImportMachine, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	if err := config.Default().Save(); err != nil {
		t.Fatal(err)
	}
	_, dataDir := identity.LocalAPI(home)
	f := &fakeImportMachine{}
	release, err := daemon.LockHome(context.Background(), dataDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.holding = release
	t.Cleanup(func() {
		if f.holding != nil {
			f.holding()
		}
	})

	oldDeps, oldImport, oldAPI := memoryDeps, identityImport, apiAnswers
	t.Cleanup(func() { memoryDeps, identityImport, apiAnswers = oldDeps, oldImport, oldAPI })
	memoryDeps.lockWait = 300 * time.Millisecond
	memoryDeps.serviceInstalled = func() bool { return installed }
	memoryDeps.stopService = func(*config.Config) error {
		f.calls = append(f.calls, "stop")
		if f.holding != nil {
			f.holding() // the service's twin quits and gives the home back
			f.holding = nil
		}
		return nil
	}
	memoryDeps.startService = func(*config.Config) error {
		f.calls = append(f.calls, "start")
		if daemon.HomeInUse(dataDir) {
			t.Error("the service was started while the import still held the home")
		}
		return nil
	}
	apiAnswers = func(string, string) bool { return false } // never dial a real twin's port
	identityImport = func(h, file string) (identity.Result, error) {
		f.calls = append(f.calls, "import")
		if !daemon.HomeInUse(dataDir) {
			t.Error("the import ran without holding the home's claim")
		}
		return result, nil
	}
	return f, home, filepath.Join(t.TempDir(), "twin.tar.gz")
}

// Import used to refuse while the twin ran and tell the user to stop it. It
// now stops the background service, imports under the home's claim, and
// starts the service again.
func TestIdentityImportStopsAndRestartsTheService(t *testing.T) {
	f, home, file := setupImport(t, true, identity.Result{Manifest: identity.Manifest{Twin: "Jeeves"}})
	var out bytes.Buffer
	if err := importIdentity(context.Background(), &out, home, file); err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop", "import", "start"}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	if !strings.Contains(out.String(), "Imported Jeeves") || !strings.Contains(out.String(), "running again") {
		t.Fatalf("said:\n%s", out.String())
	}
}

// With things to check first, the service stays paused rather than running
// the imported settings straight away.
func TestIdentityImportLeavesServicePausedWhenThereIsSomethingToCheck(t *testing.T) {
	f, home, file := setupImport(t, true, identity.Result{Warnings: []string{"llm.base_url is now https://proxy.example/v1"}})
	var out bytes.Buffer
	if err := importIdentity(context.Background(), &out, home, file); err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop", "import"}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	if !strings.Contains(out.String(), "mirrin service start") {
		t.Fatalf("said:\n%s", out.String())
	}
}

// A twin outside the service (the menu bar app) that keeps the home is
// asked to quit, and nothing is imported.
func TestIdentityImportWithATwinItCantStop(t *testing.T) {
	f, home, file := setupImport(t, false, identity.Result{})
	err := importIdentity(context.Background(), &bytes.Buffer{}, home, file)
	if !errors.Is(err, errTwinRunning) {
		t.Fatalf("err %v", err)
	}
	if slices.Contains(f.calls, "import") {
		t.Fatal("imported under a running twin")
	}
}

// Without a service there is nothing to wait for: a twin holding the home
// was refused only after the whole lock wait (15s). It is refused at once.
func TestIdentityImportRefusesAtOnceWithoutAService(t *testing.T) {
	_, home, file := setupImport(t, false, identity.Result{})
	memoryDeps.lockWait = time.Minute
	start := time.Now()
	err := importIdentity(context.Background(), &bytes.Buffer{}, home, file)
	if !errors.Is(err, errTwinRunning) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("refused after %v", d)
	}
}
