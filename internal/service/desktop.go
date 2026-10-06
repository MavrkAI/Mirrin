package service

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/procenv"
)

// The variables a service passes on, written under the current names and
// read under AntBot's too (brand.Getenv): its systemd units, and a
// supervisor that started before an update, still use those.
const (
	desktopServiceEnv = "MIRRIN_DESKTOP_SERVICE"
	supervisedLogsEnv = "MIRRIN_LOGS_SUPERVISED"
)

// Linux always starts the supervisor through run. Desktop preference is
// explicit in the installed unit, rather than inferred from a login shell.
func serviceArguments(goos, mode string) []string {
	if goos == "linux" {
		return []string{"run"}
	}
	return []string{mode}
}
func installEnvironment(goos string, wantTray bool) map[string]string {
	env := Env()
	if goos == "linux" {
		env[desktopServiceEnv] = "0"
		if wantTray {
			env[desktopServiceEnv] = "1"
		}
	}
	return env
}

// wantsDesktop reports whether the installed unit asks for the tray when a
// desktop session is there.
func wantsDesktop() bool { return brand.Getenv(desktopServiceEnv) == "1" }

func withDesktopSession(headless Run) Run {
	if runtime.GOOS != "linux" || !wantsDesktop() {
		return headless
	}
	return func(ctx context.Context) error {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		return superviseDesktop(ctx, tick.C, desktopSession, headless, runDesktop)
	}
}

// superviseDesktop waits for the old process to release the twin before
// starting another. A failed tray falls back to headless until the next login;
// stale DISPLAY in a lingering manager never counts as an active session.
func superviseDesktop(ctx context.Context, ticks <-chan time.Time, session func(context.Context) []string, headless Run, desktop func(context.Context, []string) error) error {
	var cancel context.CancelFunc
	var done chan error
	var showing, failed bool
	stop := func() {
		if cancel != nil {
			cancel()
			<-done
			cancel = nil
		}
	}
	defer stop()
	start := func(env []string) {
		runCtx, end := context.WithCancel(ctx)
		cancel, done = end, make(chan error, 1)
		showing = len(env) > 0
		go func(result chan error) {
			if len(env) > 0 {
				result <- desktop(runCtx, env)
			} else {
				result <- headless(runCtx)
			}
		}(done)
	}
	choose := func() []string {
		env := session(ctx)
		if len(env) == 0 {
			failed = false
		}
		if failed {
			return nil
		}
		return env
	}
	start(choose())
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			cancel()
			cancel = nil
			if ctx.Err() != nil {
				return nil
			}
			if !showing {
				return err
			}
			failed = true
			start(nil)
		case <-ticks:
			env := choose()
			if (len(env) > 0) != showing {
				stop()
				start(env)
			}
		}
	}
}

func desktopSession(ctx context.Context) []string {
	return queryDesktopSession(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
		cmd.Env = procenv.Base()
		return cmd.Output()
	})
}
func queryDesktopSession(ctx context.Context, command func(context.Context, ...string) ([]byte, error)) []string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := command(ctx, "is-active", "--quiet", "graphical-session.target"); err != nil {
		return nil
	}
	out, err := command(ctx, "show-environment")
	if err != nil {
		return nil
	}
	var env []string
	display := false
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || v == "" {
			continue
		}
		switch k {
		case "DISPLAY", "WAYLAND_DISPLAY":
			display = true
			env = append(env, line)
		case "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR":
			env = append(env, line)
		}
	}
	if !display {
		return nil
	} // import may finish just after the target starts
	return env
}

func desktopEnvironment(session []string) []string {
	// Remove inherited desktop addresses before adding the current session's.
	var env []string
	for _, kv := range procenv.Base() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR":
		default:
			env = append(env, kv)
		}
	}
	return append(append(env, session...), "MIRRIN_HOME="+config.Home(), config.ServiceEnv+"=1", supervisedLogsEnv+"=1")
}
func runDesktop(ctx context.Context, session []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, exe, "tray")
	cmd.Env = desktopEnvironment(session)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
