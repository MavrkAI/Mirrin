package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUserUnitStartsSessionSupervisorAndRespectsTraySetting(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	if !strings.Contains(userUnit, "WantedBy=default.target") || !strings.Contains(userUnit, "StandardError=append:") ||
		!strings.Contains(userUnit, "{{range Arguments}}") || strings.Contains(userUnit, "DISPLAY") {
		t.Fatal(userUnit)
	}
	for _, want := range []bool{true, false} {
		env := installEnvironment("linux", want)
		if (env[desktopServiceEnv] == "1") != want {
			t.Fatalf("%v: %v", want, env)
		}
	}
	if got := serviceArguments("linux", "tray"); !reflect.DeepEqual(got, []string{"run"}) {
		t.Fatal(got)
	}
	if got := serviceArguments("darwin", "tray"); !reflect.DeepEqual(got, []string{"tray"}) {
		t.Fatal(got)
	}
}

func TestDesktopSessionNeedsActiveTargetAndFreshDisplay(t *testing.T) {
	for _, tc := range []struct {
		active bool
		env    string
		want   []string
	}{
		{false, "DISPLAY=:stale", nil},
		{true, "API_KEY=secret\n", nil},
		{true, "DISPLAY=:1\nAPI_KEY=secret\nXAUTHORITY=/tmp/auth", []string{"DISPLAY=:1", "XAUTHORITY=/tmp/auth"}},
		{true, "WAYLAND_DISPLAY=wayland-2\nXDG_RUNTIME_DIR=/run/user/1000", []string{"WAYLAND_DISPLAY=wayland-2", "XDG_RUNTIME_DIR=/run/user/1000"}},
	} {
		queried := false
		got := queryDesktopSession(context.Background(), func(_ context.Context, args ...string) ([]byte, error) {
			if args[0] == "is-active" {
				if !tc.active {
					return nil, errors.New("inactive")
				}
				return nil, nil
			}
			queried = true
			return []byte(tc.env), nil
		})
		if !reflect.DeepEqual(got, tc.want) || (!tc.active && queried) {
			t.Fatalf("%+v: %v, queried=%v", tc, got, queried)
		}
	}
}

func TestSupervisorHandsOverAtLoginAndLogout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	events := make(chan string, 16)
	var active atomic.Bool
	run := func(mode string) Run {
		return func(ctx context.Context) error {
			events <- "start " + mode
			<-ctx.Done()
			events <- "stop " + mode
			return ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- superviseDesktop(ctx, ticks, func(context.Context) []string {
			if active.Load() {
				return []string{"DISPLAY=:1"}
			}
			return nil
		}, run("run"), func(ctx context.Context, _ []string) error { return run("tray")(ctx) })
	}()
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("%q want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("waiting for", want)
		}
	}
	expect("start run") // user manager started before the desktop imported its environment
	active.Store(true)
	ticks <- time.Now()
	expect("stop run")
	expect("start tray")
	active.Store(false)
	ticks <- time.Now()
	expect("stop tray")
	expect("start run") // linger: no desktop, even with stale DISPLAY
	cancel()
	expect("stop run")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTrayFailureFallsBackWithoutRestartLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	started := make(chan struct{}, 2)
	var tries atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- superviseDesktop(ctx, ticks, func(context.Context) []string { return []string{"DISPLAY=:1"} },
			func(ctx context.Context) error { started <- struct{}{}; <-ctx.Done(); return ctx.Err() },
			func(context.Context, []string) error { tries.Add(1); return errors.New("no app indicator") })
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("no headless fallback")
	}
	for range 3 {
		ticks <- time.Now()
	}
	cancel()
	<-done
	if tries.Load() != 1 {
		t.Fatal("tray crash loop", tries.Load())
	}
}

func TestDesktopChildEnvironmentKeepsHomeWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("DISPLAY", ":stale")
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	env := desktopEnvironment([]string{"WAYLAND_DISPLAY=wayland-1"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "secret") || strings.Contains(joined, ":stale") || !strings.Contains(joined, "MIRRIN_HOME="+home) || !strings.Contains(joined, supervisedLogsEnv+"=1") {
		t.Fatal(env)
	}
}

func TestOldLinuxUnitDoesNotTruncateNonAppendWriter(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, "mirrin.service")
	if err := os.WriteFile(unit, []byte("[Service]\nStandardOutput=file:/tmp/out\nStandardError=file:/tmp/err\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mirrin.out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = f.WriteString(strings.Repeat("a", 100))
	if err != nil {
		t.Fatal(err)
	}
	// This is the same safety gate MaintainLogs uses before its first rotation.
	err = rotationAllowed("linux", unit)
	if err == nil {
		_ = rotateServiceLogs(dir, 64)
		t.Fatal("old non-append unit was allowed to rotate")
	}
	if !strings.Contains(err.Error(), "mirrin service install") {
		t.Fatal(err)
	}
	if _, err = f.WriteString("new line\n"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if len(b) != 109 || strings.ContainsRune(string(b), 0) {
		t.Fatalf("damaged log %q", b)
	}
	if err := os.WriteFile(unit, []byte("[Service]\nStandardOutput=append:/tmp/out\nStandardError=append:/tmp/err\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rotationAllowed("linux", unit); err != nil {
		t.Fatal(err)
	}
}
