package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/homelock"
)

// The background services the project installed under its old names
// (antbot, openhuman) are retired, never taken over: they run an older
// program, and their definitions may hold the only copy of a key.

// RetireLegacy stops and removes this home's background services from
// before the rename (AntBot's, openHuman's) when they run another program:
// an older build, which would otherwise keep relaunching beside this one,
// and, once the home has moved to ~/.mirrin, start an empty twin in the old
// place. The keys a service's definition carried are saved first, into the
// home as it is now (they move with it), and a service whose keys can't be
// saved stays. Then it waits for that twin to let go of its home, so the
// home can move. Nothing is put in its place: `mirrin service install` does
// that. A service that runs this very program, or this process, is left to
// `mirrin service install` too, and another home's is never touched.
//
// It is quiet when there is nothing to do, and only reads files unless an
// old service is there. Call it before anything settles config.Home().
func RetireLegacy(out io.Writer) { newRetirer(out).run() }

// LegacyHold is config.HoldHomeMove: it names this home's background service
// from before the rename when it runs another program that is still there,
// or says "". Moved now, the home would be missing when that service starts
// at the next login, and the older program would start an empty twin in
// its place. RetireLegacy removes such a service. It only reads files, and
// never asks config.Home(), which calls it.
func LegacyHold() string {
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return ""
		}
		self, _ := os.Executable()
		return legacyEntryHold(startupFolder(appData), self)
	}
	return newRetirer(io.Discard).hold()
}

// newRetirer is a retirer over this machine's services.
func newRetirer(out io.Writer) retirer {
	self, _ := os.Executable()
	return retirer{
		out: out, self: self, label: os.Getenv("XPC_SERVICE_NAME"), wait: 20 * time.Second,
		path:  func(name string) string { return unitPath(runtime.GOOS, userHome(), name) },
		open:  legacyService,
		inUse: homelock.InUse,
	}
}

// retirer is RetireLegacy with the parts that reach the system as fields, so
// it can be tested.
type retirer struct {
	out   io.Writer
	self  string                          // this program
	label string                          // the launchd job running this process, if any
	wait  time.Duration                   // for the old twin to quit
	path  func(name string) string        // a service's definition, "" where there is none
	open  func(name string) (unit, error) // the service, to stop and remove it
	inUse func(dataDir string) bool       // a twin holds the claim on dataDir
}

// retirable gives the definition and environment of the service from before
// the rename called name when it is this home's and runs another program:
// one to retire.
func (r retirer) retirable(name string) (string, map[string]string, bool) {
	path := r.path(name)
	if path == "" || !fileExists(path) {
		return "", nil, false
	}
	env := unitEnv(path)
	if !ownUnit(env) || name == r.label || sameProgram(unitProgram(path), r.self) {
		return "", nil, false
	}
	return path, env, true
}

// hold is LegacyHold over r's services: the first one to retire whose
// program is still there to start.
func (r retirer) hold() string {
	for i, name := range legacyNames {
		path, _, ok := r.retirable(name)
		if !ok {
			continue
		}
		if prog := unitProgram(path); prog != "" && !fileExists(prog) {
			continue // it has nothing left to start
		}
		return "The old " + brand.LegacyDisplayNames[i] + " background service"
	}
	return ""
}

func (r retirer) run() {
	for i, name := range legacyNames {
		path, env, ok := r.retirable(name)
		if !ok {
			continue
		}
		old := brand.LegacyDisplayNames[i]
		in := installer{out: r.out, unitFiles: []string{path}, home: config.CurrentHome()}
		if err := in.keepSecrets(); err != nil {
			fmt.Fprintf(r.out, "warning: the old %s background service stays until its keys are saved: %v\n", old, err)
			continue
		}
		svc, err := r.open(name)
		if err == nil {
			_ = svc.Stop()
			err = svc.Uninstall()
		}
		if err != nil {
			fmt.Fprintf(r.out, "The old %s background service is still installed (%v). Remove it with: %s\n", old, err, removeCommand(runtime.GOOS, name))
			continue
		}
		r.waitFree(config.DataDirIn(legacyHome(env, i)))
		fmt.Fprintf(r.out, "Stopped the old %s background service, which ran an older program. `mirrin service install` keeps Mirrin running in the background instead.\n", old)
	}
}

// waitFree waits, up to r.wait, for the twin a retired service ran to let
// go of its data folder.
func (r retirer) waitFree(dataDir string) {
	deadline := time.Now().Add(r.wait)
	for r.inUse(dataDir) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
}

// legacyHome is the home a service from before the rename runs: the one its
// environment names, else its name's default (~/.antbot for AntBot's).
func legacyHome(env map[string]string, i int) string {
	if h := config.HomeEnv(func(k string) string { return env[k] }); h != "" {
		return h
	}
	return filepath.Join(userHome(), brand.LegacyHomeDirs[i])
}

// removeCommand is how to remove a per-user service by hand: stop it, and
// delete its definition, which would otherwise start it again at the next
// login (its keys are saved by then).
func removeCommand(goos, name string) string {
	file := "'" + strings.ReplaceAll(unitPath(goos, userHome(), name), "'", `'\''`) + "'"
	if goos == "linux" {
		return fmt.Sprintf("systemctl --user disable --now %s.service; rm %s", name, file)
	}
	return fmt.Sprintf("launchctl bootout gui/%d/%s; rm %s", os.Getuid(), name, file)
}

// sameProgram reports whether two paths are the same program, links
// followed.
func sameProgram(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	a, b = real(a), real(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// ownLegacyUnits are this home's services under old names, by name.
func ownLegacyUnits() map[string]unit {
	out := map[string]unit{}
	for _, name := range legacyNames {
		if runtime.GOOS == "windows" && name == windowsSCMLegacyName {
			continue // the Startup entry's flow retires that one (startup.go)
		}
		if path := unitPath(runtime.GOOS, userHome(), name); path != "" && fileExists(path) && !ownUnit(unitEnv(path)) {
			continue // another home's
		}
		if u, err := legacyService(name); err == nil {
			out[name] = u
		}
	}
	return out
}

// LegacyInstalled reports whether a background service from before the
// rename is installed for this home.
func LegacyInstalled() bool {
	for _, u := range ownLegacyUnits() {
		if installed(u) {
			return true
		}
	}
	return false
}

// RemoveLegacy removes this home's background services from before the
// rename, whatever program they run, keeping the keys their definitions
// held first.
func RemoveLegacy(out io.Writer) error {
	if err := (installer{out: out, unitFiles: unitFiles(legacyNames...)}).keepSecrets(); err != nil {
		return err
	}
	units := ownLegacyUnits()
	var errs []error
	for _, name := range sortedNames(units) {
		u := units[name]
		if !installed(u) {
			continue
		}
		_ = u.Stop()
		if err := u.Uninstall(); err != nil {
			errs = append(errs, fmt.Errorf("the old %s background service: %w", brand.LegacyDisplayName(name), err))
			continue
		}
		fmt.Fprintf(out, "Removed the old %s background service.\n", brand.LegacyDisplayName(name))
	}
	return errors.Join(errs...)
}

func sortedNames(m map[string]unit) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// legacyService is a service installed under an old name, opened only to
// stop and remove it. It doesn't settle config.Home(), so a home from before
// the rename can still move once that service's twin has quit.
func legacyService(name string) (unit, error) {
	return service.New(&program{}, &service.Config{Name: name, Option: service.KeyValue{"UserService": true}})
}
