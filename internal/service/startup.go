package service

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/procenv"
)

// On Windows the background twin isn't a Windows service: a service runs as
// LocalSystem in session 0, with no tray icon, no microphone and another
// profile's folders. `mirrin service install` instead puts a script in the
// user's Startup folder that starts `mirrin tray` at sign-in, with
// MIRRIN_HOME and MIRRIN_SERVICE set, and starts it now. The setup program
// (packaging/windows/mirrin.iss) writes the same script.

// startupName is the script in the Startup folder.
const startupName = "Mirrin.cmd"

// startupFolder is the per-user Startup folder under %APPDATA%.
func startupFolder(appData string) string {
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
}

// startupScript is the Startup entry that runs exe's tray for home. cmd
// reads it as UTF-8 after chcp, so a home with accents works, and a % in a
// path is doubled so cmd doesn't expand it.
func startupScript(exe, home string) string {
	esc := func(s string) string { return strings.ReplaceAll(s, "%", "%%") }
	return "@echo off\r\n" +
		"chcp 65001 >nul\r\n" +
		"rem Starts Mirrin in the system tray when you sign in. `mirrin service uninstall` removes this.\r\n" +
		`set "MIRRIN_HOME=` + esc(home) + "\"\r\n" +
		`set "` + config.ServiceEnv + "=1\"\r\n" +
		`start "" /min "` + esc(exe) + "\" tray\r\n"
}

// startupEnv reads the variables a Startup script sets.
func startupEnv(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		rest, ok := strings.CutPrefix(line, `set "`)
		if !ok {
			continue
		}
		rest = strings.TrimSuffix(rest, `"`)
		if k, v, ok := strings.Cut(rest, "="); ok && k != "" {
			out[k] = strings.ReplaceAll(v, "%%", "%")
		}
	}
	return out
}

// startup is the Windows background twin: its Startup entry, and the tray
// it starts. The parts that reach Windows are functions, so the flow can be
// tested anywhere.
type startup struct {
	dir, exe, home string
	out            io.Writer
	up             func() bool // the twin answers
	wait           time.Duration
	secretEnvs     []string
	launch         func(exe string, env []string) error // start `exe tray` in the background
	stopTwin       func() error                         // quit this home's tray
	running        func() bool                          // a twin runs from this home
	elevated       func() bool                          // this command runs as administrator (nil: never)
}

func (s startup) isElevated() bool { return s.elevated != nil && s.elevated() }

func (s startup) path() string { return filepath.Join(s.dir, startupName) }

// installed reports whether this home's Startup entry is there.
func (s startup) installed() bool {
	b, err := os.ReadFile(s.path())
	if err != nil {
		return false
	}
	env := startupEnv(b)
	return sameHome(config.HomeEnv(func(k string) string { return env[k] }), s.home)
}

// env is what the tray started now gets: what a program needs to run, and
// the service's two variables. Keys stay in the secrets file.
func (s startup) env() []string {
	return append(procenv.Base(), "MIRRIN_HOME="+s.home, config.ServiceEnv+"=1")
}

func (s startup) write() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("couldn't create your Startup folder %s: %w", s.dir, err)
	}
	tmp := s.path() + ".new"
	if err := os.WriteFile(tmp, []byte(startupScript(s.exe, s.home)), 0o600); err != nil {
		return fmt.Errorf("couldn't write %s: %w", s.path(), err)
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("couldn't write %s: %w", s.path(), err)
	}
	return nil
}

func (s startup) start() error {
	if err := s.launch(s.exe, s.env()); err != nil {
		return fmt.Errorf("couldn't start Mirrin: %w", err)
	}
	return waitUp(s.up, s.wait, s.out)
}

// errElevated is why the tray isn't started from an administrator
// terminal: it would run the whole twin, shell and tools included, with
// administrator rights until the next sign-in.
var errElevated = errors.New("this terminal runs as administrator, and Mirrin shouldn't run with those rights. Run `mirrin service start` in a normal terminal, or sign out and back in")

func (s startup) control(action string) error {
	if s.up == nil {
		s.up = s.running
	}
	switch action {
	case "install":
		in := installer{out: s.out, secretEnvs: s.secretEnvs}
		if err := in.keepSecrets(); err != nil {
			return err
		}
		if err := s.write(); err != nil {
			return err
		}
		if s.isElevated() {
			fmt.Fprintln(s.out, "Done: Mirrin will start in the tray when you next sign in. It wasn't started now because this terminal runs as administrator; run `mirrin service start` in a normal terminal to start it now.")
			return nil
		}
		// A reinstall runs the program installed now.
		if s.running() {
			_ = s.stopTwin()
		}
		return s.start()
	case "uninstall":
		if !s.installed() {
			fmt.Fprintln(s.out, "Mirrin doesn't start when you sign in, so there's nothing to remove.")
			return nil
		}
		_ = s.stopTwin()
		if err := os.Remove(s.path()); err != nil {
			return fmt.Errorf("couldn't remove %s: %w", s.path(), err)
		}
		fmt.Fprintln(s.out, "Removed. Mirrin won't start when you sign in any more; `mirrin service install` brings it back.")
		return nil
	case "start", "restart":
		if s.isElevated() {
			return errElevated
		}
		if !s.installed() {
			return errors.New("Mirrin isn't set to start when you sign in yet; run `mirrin service install`")
		}
		if action == "restart" {
			if err := s.stopTwin(); err != nil {
				return fmt.Errorf("couldn't quit the running Mirrin: %w", err)
			}
		} else if s.running() {
			fmt.Fprintln(s.out, "Mirrin is already running in the background.")
			return nil
		}
		return s.start()
	case "stop":
		if err := s.stopTwin(); err != nil {
			return fmt.Errorf("couldn't quit Mirrin: %w", err)
		}
		fmt.Fprintln(s.out, "Stopped. It starts again when you next sign in, or with `mirrin service start`.")
		return nil
	case "status":
		inst, running := s.installed(), s.running()
		switch {
		case running && s.up():
			fmt.Fprintln(s.out, "service status: running")
		case !inst:
			fmt.Fprintln(s.out, "service status: not installed (run `mirrin service install` to keep Mirrin running in the background)")
		case running:
			fmt.Fprintln(s.out, "service status: running, but not answering yet")
		default:
			fmt.Fprintln(s.out, "service status: installed, but not running")
			if e := LastError(); e != "" {
				fmt.Fprintln(s.out, "last error:", e)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown service action %q (install, uninstall, start, stop, restart or status)", action)
}

// ownTrays picks, from "pid<TAB>command line" lines, the trays that hold
// this home (holders, the processes with its claim open), leaving out self.
// Another profile's twin, or a `mirrin run` in a terminal, isn't one.
func ownTrays(list string, self int, holders []int) []int {
	var out []int
	for _, line := range strings.Split(list, "\n") {
		id, cmdline, ok := strings.Cut(strings.TrimSpace(line), "\t")
		pid, err := strconv.Atoi(id)
		if !ok || err != nil || pid == self || !slices.Contains(holders, pid) {
			continue
		}
		if f := strings.Fields(cmdline); len(f) > 1 && f[len(f)-1] == "tray" {
			out = append(out, pid)
		}
	}
	return out
}
