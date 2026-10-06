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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/procenv"
)

// On Windows the background twin isn't a Windows service: a service runs as
// LocalSystem in session 0, with no tray icon, no microphone and another
// profile's folders. `mirrin service install` instead puts a script in the
// user's Startup folder that starts `mirrin tray` at sign-in, with
// MIRRIN_HOME and MIRRIN_SERVICE set, and starts it now. The setup program
// (packaging/windows/mirrin.iss) writes the same script.

const (
	// startupName is the script in the Startup folder.
	startupName = "Mirrin.cmd"
	// legacyStartupName is the script AntBot put there, which starts the
	// older antbot.exe. Mirrin's replaces it: two would start two trays.
	legacyStartupName = "AntBot.cmd" // rename:keep
	// startupLink is the shortcut earlier setup programs put there, which
	// started the tray without the service's environment.
	startupLink = "AntBot.lnk" // rename:keep
	// windowsSCMLegacyName is the Windows service (LocalSystem) earlier
	// versions installed, under the name they had then. It stays this name
	// on the machines that have it.
	windowsSCMLegacyName = "antbot" // rename:keep
)

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
	legacy         func() bool                          // this home's Windows service from before is there (nil: never)
	retire         func() error                         // remove that service, keeping its keys first
	elevated       func() bool                          // this command runs as administrator (nil: never)
}

// legacyAdvice says how to remove the Windows service an earlier Mirrin
// installed, which needs administrator rights. `mirrin service install`
// isn't the advice: run as administrator, it would start the whole twin with
// administrator rights.
var legacyAdvice = "The Windows service an earlier version installed is still there, and only an administrator can remove it. Open a terminal as administrator, run `sc.exe delete " + windowsSCMLegacyName + "` there and close it, then run `mirrin service install` in a normal terminal."

func (s startup) hasLegacy() bool    { return s.legacy != nil && s.legacy() }
func (s startup) isElevated() bool   { return s.elevated != nil && s.elevated() }
func (s startup) anyInstalled() bool { return s.installed() || s.hasLegacy() }

// retireLegacy removes this home's Windows service from before, if there is
// one. It fails when that service is still there afterwards.
func (s startup) retireLegacy() error {
	if !s.hasLegacy() {
		return nil
	}
	if s.retire != nil {
		if err := s.retire(); err == nil {
			fmt.Fprintln(s.out, "Removed the Windows service an earlier Mirrin installed; Mirrin now starts from your Startup folder when you sign in.")
			return nil
		}
	}
	return errors.New(legacyAdvice)
}

func (s startup) path() string { return filepath.Join(s.dir, startupName) }

// installed reports whether this home's Startup entry is there: Mirrin's,
// or AntBot's until it is replaced (tidyLegacyEntry).
func (s startup) installed() bool {
	return s.owns(startupName) || s.owns(legacyStartupName)
}

// owns reports whether the Startup folder's script name starts this home's
// twin.
func (s startup) owns(name string) bool {
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return false
	}
	env := startupEnv(b)
	return sameHome(config.HomeEnv(func(k string) string { return env[k] }), s.home)
}

// tidyLegacyEntry replaces this home's Startup entry from AntBot (which
// starts the older antbot.exe) with Mirrin's, which starts this program.
// The tray AntBot's entry started keeps running until it is quit or the
// user signs out; from the next sign-in Mirrin starts instead. It is quiet
// when there is nothing to do.
func (s startup) tidyLegacyEntry() {
	if !s.owns(legacyStartupName) {
		return
	}
	if s.owns(startupName) {
		_ = os.Remove(filepath.Join(s.dir, legacyStartupName))
		return
	}
	if err := s.write(); err != nil {
		fmt.Fprintln(s.out, "warning:", err)
		return
	}
	fmt.Fprintf(s.out, "Mirrin now starts when you sign in, in place of %s. An %s that's running keeps going until you quit it or sign out.\n", brand.LegacyDisplayNames[0], brand.LegacyDisplayNames[0])
}

// legacyEntryHold is LegacyHold on Windows: it names AntBot's Startup entry
// in the Startup folder dir when it starts this home's twin with another
// program that is still there, or says "". tidyLegacyEntry replaces it.
func legacyEntryHold(dir, self string) string {
	b, err := os.ReadFile(filepath.Join(dir, legacyStartupName))
	if err != nil || !ownUnit(startupEnv(b)) {
		return ""
	}
	prog := startupProgram(b)
	if sameProgram(prog, self) || prog != "" && !fileExists(prog) {
		return ""
	}
	return "The old " + brand.LegacyDisplayNames[0] + " entry in your Startup folder"
}

// startupProgram is the program a Startup script starts, or "".
func startupProgram(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), `start "" /min "`)
		if !ok {
			continue
		}
		if end := strings.Index(rest, `"`); end >= 0 {
			return strings.ReplaceAll(rest[:end], "%%", "%")
		}
	}
	return ""
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
	// Two entries would start two trays.
	_ = os.Remove(filepath.Join(s.dir, startupLink))
	if s.owns(legacyStartupName) {
		_ = os.Remove(filepath.Join(s.dir, legacyStartupName))
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
		// With the old service still there, both would start at sign-in.
		if err := s.retireLegacy(); err != nil {
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
		had, leg := s.installed(), s.hasLegacy()
		if !had && !leg {
			fmt.Fprintln(s.out, "Mirrin doesn't start when you sign in, so there's nothing to remove.")
			return nil
		}
		if had {
			_ = s.stopTwin()
			for _, name := range []string{startupName, legacyStartupName} {
				if p := filepath.Join(s.dir, name); s.owns(name) {
					if err := os.Remove(p); err != nil {
						return fmt.Errorf("couldn't remove %s: %w", p, err)
					}
				}
			}
			_ = os.Remove(filepath.Join(s.dir, startupLink))
		}
		if err := s.retireLegacy(); err != nil {
			return err
		}
		fmt.Fprintln(s.out, "Removed. Mirrin won't start when you sign in any more; `mirrin service install` brings it back.")
		return nil
	case "start", "restart":
		if s.isElevated() {
			return errElevated
		}
		// The old service would keep running the old version.
		if err := s.retireLegacy(); err != nil {
			return err
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
		if err := s.retireLegacy(); err != nil {
			return err
		}
		if err := s.stopTwin(); err != nil {
			return fmt.Errorf("couldn't quit Mirrin: %w", err)
		}
		fmt.Fprintln(s.out, "Stopped. It starts again when you next sign in, or with `mirrin service start`.")
		return nil
	case "status":
		inst, running := s.anyInstalled(), s.running()
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
		if s.hasLegacy() {
			fmt.Fprintln(s.out, legacyAdvice)
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

// legacyNoticeFile records that the advice about an older Windows service's
// settings was given, so every command doesn't repeat it.
func legacyNoticeFile() string { return filepath.Join(config.Home(), "windows-service-notice") }

// regEnv is a Windows service's Environment value in the registry
// (REG_MULTI_SZ, one KEY=value a line).
type regEnv interface {
	Read() ([]string, error)
	Write([]string) error
}

// tidyRegistryEnv moves the keys an older Mirrin wrote into its Windows
// service's environment to the 0600 secrets file, then rewrites that
// environment without them (and with MIRRIN_SERVICE=1). Only this home's
// service is touched, and a key is never dropped unless it was kept first.
func tidyRegistryEnv(r regEnv, out io.Writer) {
	lines, err := r.Read()
	if err != nil || len(lines) == 0 {
		return
	}
	env := map[string]string{}
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, "="); ok && k != "" {
			env[k] = v
		}
	}
	if !ownUnit(env) {
		return
	}
	have, err := config.ReadSecrets()
	if err != nil {
		fmt.Fprintln(out, "warning:", err)
		return
	}
	save := map[string]string{}
	var secrets, keep []string
	for _, l := range lines {
		k, v, _ := strings.Cut(l, "=")
		if looksSecret(k) {
			secrets = append(secrets, k)
			if name := brand.CurrentEnv(k); !saved(have, name) {
				save[name] = v
			}
			continue
		}
		keep = append(keep, l)
	}
	sort.Strings(secrets)
	if len(secrets) == 0 && hasServiceMarker(env) {
		return
	}
	if len(save) > 0 {
		if err := config.SaveSecrets(save); err != nil {
			fmt.Fprintln(out, "warning: couldn't save keys for the background service:", err)
			return
		}
	}
	if !hasServiceMarker(env) {
		keep = append(keep, config.ServiceEnv+"=1")
	}
	if err := r.Write(keep); err != nil {
		if len(secrets) > 0 && !fileExists(legacyNoticeFile()) {
			fmt.Fprintf(out, "An older version left %s in its Windows service's settings. They're saved in %s now; to remove them there, open a terminal as administrator and run `sc.exe delete %s`.\n", strings.Join(secrets, ", "), config.SecretsPath(), windowsSCMLegacyName)
			_ = os.WriteFile(legacyNoticeFile(), []byte("shown\n"), 0o600)
		}
		return
	}
	if len(secrets) > 0 {
		fmt.Fprintf(out, "Moved %s out of the Windows service's settings into %s, which only you can read.\n", strings.Join(secrets, ", "), config.SecretsPath())
	}
}
