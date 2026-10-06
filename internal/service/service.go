// Package service installs Mirrin as a background service on macOS (launchd)
// and Linux (systemd), and on Windows as an entry in the user's Startup
// folder that starts the tray at sign-in (startup.go).
package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kardianos/service"

	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/homelock"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Run is the function the service executes; it must block until ctx ends.
type Run func(ctx context.Context) error

// Mode is the subcommand the installed service launches: "tray" or "run".
var Mode = "run"

// Name is what launchd, systemd and Windows know the service as.
const Name = brand.Name

// legacyNames are the services the project installed under its old names
// (AntBot's, then openHuman's).
var legacyNames = brand.LegacyNames

type program struct {
	run    Run
	cancel context.CancelFunc
	done   chan error
}

func (p *program) Start(_ service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan error, 1)
	go func() {
		if p.run == nil {
			<-ctx.Done()
			p.done <- nil
			return
		}
		p.done <- p.run(ctx)
	}()
	return nil
}

func (p *program) Stop(_ service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		return <-p.done
	}
	return nil
}

// userUnit is kardianos' systemd unit adapted to a user service: it is wanted
// by default.target (a user session has no multi-user.target, so the stock
// unit never starts at login) and restarts after 10 s rather than 2 minutes.
const userUnit = `[Unit]
Description={{Description}}
ConditionFileIsExecutable={{Path | cmdEscape}}

[Service]
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
{{if OutputFileSupport}}StandardOutput=append:{{LogDirectory}}/{{Name}}.out
StandardError=append:{{LogDirectory}}/{{Name}}.err
{{end}}Restart=always
RestartSec=10
{{range EnvVars}}{{.}}
{{end}}
[Install]
WantedBy=default.target
`

func newService(name string, run Run, env map[string]string) (service.Service, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cfg := &service.Config{
		Name:        name,
		DisplayName: "Mirrin",
		Description: "Your personal AI that runs your life.",
		Executable:  serviceProgram(runtime.GOOS, exe, fileExists),
		Arguments:   serviceArguments(runtime.GOOS, Mode),
		EnvVars:     env,
		Option: service.KeyValue{
			"UserService":   true, // macOS: install as a LaunchAgent for the current user
			"KeepAlive":     true,
			"RunAtLoad":     true,
			"LogOutput":     true,
			"LogDirectory":  logDir(),
			"SystemdScript": userUnit,
		},
	}
	_ = os.MkdirAll(logDir(), 0o700)
	return service.New(&program{run: run}, cfg)
}

// macApp is the installed app bundle's program.
const macApp = "/Applications/Mirrin.app/Contents/MacOS/mirrin"

// serviceProgram is the program the service runs: on macOS the installed
// app bundle, which permissions and the icon attach to, else this program.
// Never AntBot.app: no update ever reaches it, so it only holds older code.
func serviceProgram(goos, exe string, exists func(string) bool) string {
	if goos == "darwin" && exists(macApp) {
		return macApp
	}
	return exe
}

// Env is the environment the service starts with. It never carries secrets:
// those stay in the 0600 secrets file, which every mirrin process loads.
// MIRRIN_HOME is explicit so the service finds this home even where it runs
// with a different profile (a Windows service runs as LocalSystem).
func Env() map[string]string {
	env := map[string]string{"MIRRIN_HOME": config.Home(), config.ServiceEnv: "1"}
	for _, k := range []string{"HOME", "PATH", "USERPROFILE"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func logDir() string { return filepath.Join(config.Home(), "logs") }

// unit is the part of a kardianos service the install flow drives.
type unit interface {
	Install() error
	Uninstall() error
	Start() error
	Stop() error
	Status() (service.Status, error)
}

func installed(u unit) bool {
	_, err := u.Status()
	return !errors.Is(err, service.ErrNotInstalled)
}

// Control performs install, uninstall, start, stop, restart or status. up
// reports whether the twin answers; install, start and restart wait for it so
// they can say whether it really came up. up may be nil when the local API is off.
func Control(cfg *config.Config, action string, up func() bool, out io.Writer) error {
	if runtime.GOOS == "windows" {
		return controlStartup(cfg, action, up, out)
	}
	s, err := newService(Name, nil, installEnvironment(runtime.GOOS, cfg.Tray))
	if err != nil {
		return err
	}
	if up == nil {
		up = func() bool { st, _ := s.Status(); return st == service.StatusRunning }
	}
	// Nothing is set up or started in a home from before the rename while a
	// twin still runs from it (homeInUse). A running service is most likely
	// that twin, and reinstalling or restarting it is what moves the home.
	oldTwinHolds := func() error {
		if _, running := stateOf(s.Status()); running {
			return nil
		}
		return homeInUse(config.Home(), userHome(), action, homelock.InUse)
	}
	switch action {
	case "install":
		if err := oldTwinHolds(); err != nil {
			return err
		}
		in := installer{
			svc: s, start: func() error { return startUnit(s) }, up: up, wait: 20 * time.Second,
			out: out, secretEnvs: cfg.SecretEnvs(), unitFiles: unitFiles(append([]string{Name}, legacyNames...)...),
			legacy: ownLegacyUnits(),
		}
		return in.run()
	case "uninstall":
		if !installed(s) {
			fmt.Fprintln(out, "The background service isn't installed, so there's nothing to remove.")
			return nil
		}
		_ = s.Stop()
		if err := s.Uninstall(); err != nil {
			return fmt.Errorf("couldn't remove the background service: %w", err)
		}
		fmt.Fprintln(out, "Removed. Mirrin won't start on its own any more; `mirrin service install` brings it back.")
		return nil
	case "start", "restart":
		if !installed(s) {
			return errors.New("the background service isn't installed yet; run `mirrin service install`")
		}
		if err := oldTwinHolds(); err != nil {
			return err
		}
		var err error
		if action == "restart" && runtime.GOOS == "darwin" {
			// Kill and relaunch in place; fall back to a fresh start if it wasn't loaded.
			if err = exec.Command("launchctl", "kickstart", "-k", launchdTarget()).Run(); err != nil {
				err = startUnit(s)
			}
		} else if action == "restart" {
			_ = s.Stop()
			err = startUnit(s)
		} else {
			err = startUnit(s)
		}
		if err != nil {
			return fmt.Errorf("couldn't %s the background service: %w", action, err)
		}
		return waitUp(up, 20*time.Second, out)
	case "stop":
		if !installed(s) {
			fmt.Fprintln(out, "The background service isn't installed.")
			return nil
		}
		if err := s.Stop(); err != nil {
			return fmt.Errorf("couldn't stop the background service: %w", err)
		}
		fmt.Fprintln(out, "Stopped. It starts again at your next login, or with `mirrin service start`.")
		return nil
	case "status":
		inst, running := State()
		switch {
		case running && up():
			fmt.Fprintln(out, "service status: running")
		case !inst:
			fmt.Fprintln(out, "service status: not installed (run `mirrin service install` to keep Mirrin running in the background)")
		case running:
			fmt.Fprintln(out, "service status: running, but not answering yet")
		default:
			fmt.Fprintln(out, "service status: installed, but not running")
			if e := LastError(); e != "" {
				fmt.Fprintln(out, "last error:", e)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown service action %q (install, uninstall, start, stop, restart or status)", action)
}

// installer is the install flow, separated from kardianos so it can be tested.
type installer struct {
	svc        unit
	legacy     map[string]unit // this home's services under old names, by name
	start      func() error
	up         func() bool
	wait       time.Duration
	out        io.Writer
	secretEnvs []string // variables whose current values the service needs
	unitFiles  []string // existing unit files whose environment may hold secrets
	home       string   // the home whose secrets file keeps them ("": config.Home())
}

func (in installer) run() error {
	if err := in.keepSecrets(); err != nil {
		return err
	}
	for _, name := range sortedNames(in.legacy) {
		if old := in.legacy[name]; installed(old) {
			_ = old.Stop()
			if err := old.Uninstall(); err == nil {
				fmt.Fprintf(in.out, "Stopped the old %s background service; Mirrin takes over from here.\n", brand.LegacyDisplayName(name))
			}
		}
	}
	// Reinstalling replaces the old definition, so a new binary or mode takes effect.
	if installed(in.svc) {
		_ = in.svc.Stop()
		_ = in.svc.Uninstall()
	}
	if err := in.svc.Install(); err != nil {
		return fmt.Errorf("couldn't install the background service: %w", err)
	}
	if err := in.start(); err != nil {
		return fmt.Errorf("installed, but couldn't start it: %w (try `mirrin service start`)", err)
	}
	if err := waitUp(in.up, in.wait, in.out); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		fmt.Fprintln(in.out, "To keep it running after you log out: loginctl enable-linger $USER")
	}
	return nil
}

// keepSecrets moves the secrets the service needs into the secrets file: from
// this shell's environment, and from any unit file an older version wrote them
// into. The environment wins, then what the file already holds, under any of
// a key's names (an AntBot unit's ANTBOT_EMAIL_PASSWORD is saved as
// MIRRIN_EMAIL_PASSWORD, unless the file has it under either name). A key
// the settings name is found in the environment under any of its names too,
// and saved under the one the settings use, which config.Secret reads
// first.
func (in installer) keepSecrets() error {
	home := in.home
	if home == "" {
		home = config.Home()
	}
	have, err := config.ReadSecretsIn(home)
	if err != nil {
		return err
	}
	vals := map[string]string{}
	for _, f := range in.unitFiles {
		for k, v := range unitEnv(f) {
			k = brand.CurrentEnv(k)
			if !looksSecret(k) || saved(have, k) {
				continue
			}
			vals[k] = v
		}
	}
	for _, k := range in.secretEnvs {
		if _, v := config.Exported(k); v != "" {
			for _, n := range brand.EnvAliases(k) {
				delete(vals, n) // a unit's older copy
			}
			vals[k] = v
		}
	}
	var changed []string
	for k, v := range vals {
		if have[k] != v {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if err := config.SaveSecretsIn(home, vals); err != nil {
		return fmt.Errorf("couldn't save keys for the background service: %w", err)
	}
	sort.Strings(changed)
	fmt.Fprintf(in.out, "Saved %s for the background twin in %s (only you can read it).\n", strings.Join(changed, ", "), config.SecretsPathIn(home))
	return nil
}

// saved reports whether the secrets file holds a value for name under any
// of its names.
func saved(have map[string]string, name string) bool {
	for _, n := range brand.EnvAliases(name) {
		if have[n] != "" {
			return true
		}
	}
	return false
}

// looksSecret reports whether a variable in a unit file holds a key, token
// or password, which belong in the secrets file rather than the unit.
func looksSecret(name string) bool {
	n := strings.ToUpper(name)
	for _, w := range []string{"KEY", "TOKEN", "PASSWORD", "SECRET"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// waitUp polls until the twin answers or the wait runs out.
func waitUp(up func() bool, wait time.Duration, out io.Writer) error {
	deadline := time.Now().Add(wait)
	for {
		if up() {
			msg := "Mirrin is running in the background and will start again when you log in."
			if Mode == "tray" {
				msg += " Look for its icon in the menu bar."
			}
			fmt.Fprintln(out, msg)
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Fprintln(out, "The background service didn't come up.")
	if e := LastError(); e != "" {
		fmt.Fprintln(out, "Last error:", e)
	}
	return fmt.Errorf("the background service isn't answering; its log is in %s", logDir())
}

// startUnit starts an installed service. On macOS it bootstraps the agent into
// the login session and kicks it, which works on current macOS where the old
// `launchctl load` can leave an agent loaded but idle.
func startUnit(s unit) error {
	if runtime.GOOS != "darwin" {
		return s.Start()
	}
	plist := unitPath("darwin", userHome(), Name)
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "enable", launchdTarget()).Run() // undo an earlier `launchctl disable`
	if err := exec.Command("launchctl", "bootstrap", domain, plist).Run(); err != nil {
		_ = s.Start() // already bootstrapped, or an older macOS: the classic load
	}
	return exec.Command("launchctl", "kickstart", launchdTarget()).Run()
}

func launchdTarget() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), Name) }

// State reports whether this home's background service is installed and
// running. One installed for another home (a second profile) doesn't count.
func State() (installed, running bool) {
	if runtime.GOOS == "windows" {
		return stateStartup()
	}
	if path := unitPath(runtime.GOOS, userHome(), Name); path != "" && fileExists(path) && !ownUnit(unitEnv(path)) {
		return false, false
	}
	s, err := newService(Name, nil, nil)
	if err != nil {
		return false, false
	}
	return stateOf(s.Status())
}

// stateOf reads a service manager's answer. Only "not installed" means not
// installed: a unit systemd reports as failed, or one Windows won't describe
// to a non-admin, is there but not known to be running.
func stateOf(st service.Status, err error) (installed, running bool) {
	if errors.Is(err, service.ErrNotInstalled) {
		return false, false
	}
	return true, err == nil && st == service.StatusRunning
}

// LastError is the last error the background service logged since it last
// started properly, in plain words, or "". A service still running under an
// old name logs under that name (antbot.err.log), which counts until this
// one has logged anything.
func LastError() string { return lastError(logDir()) }

func lastError(dir string) string {
	for _, svc := range append([]string{Name}, legacyNames...) {
		files := []string{filepath.Join(dir, svc+".err.log"), filepath.Join(dir, svc+".err")}
		if !fileExists(files[0]) && !fileExists(files[1]) {
			continue
		}
		for _, f := range files {
			if line := lastErrorLine(f); line != "" {
				return line
			}
		}
		return ""
	}
	return ""
}

// startedMarker is what the twin logs once it is up; errors before the
// latest one are old news.
const startedMarker = `msg="Mirrin online"`

// lastErrorLine reads the end of a service log (which grows without bound)
// for the last error since the twin last came up.
func lastErrorLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 64 << 10
	if st, err := f.Stat(); err == nil && st.Size() > tail {
		if _, err := f.Seek(st.Size()-tail, io.SeekStart); err != nil {
			return ""
		}
	}
	var last, lastErr string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.Contains(line, startedMarker):
			last, lastErr = "", ""
		case strings.HasPrefix(line, "error:") || strings.Contains(line, "level=ERROR"):
			last, lastErr = line, line
		case !strings.Contains(line, "level="): // plain output, such as a crash
			last = line
		}
	}
	if lastErr == "" {
		lastErr = last
	}
	return readable(lastErr)
}

// readable turns a raw log line into one plain sentence: the explanation of
// a model error when there is one, else the message and its error.
func readable(line string) string {
	if line == "" {
		return ""
	}
	msg, errText := line, ""
	if rest, ok := strings.CutPrefix(line, "error:"); ok {
		msg, errText = "", strings.TrimSpace(rest)
	} else if strings.Contains(line, "level=") {
		msg, errText = logField(line, "msg"), logField(line, "err")
	}
	if errText != "" {
		if plain, ok := llm.Describe(errors.New(errText)); ok {
			return plain
		}
	}
	if msg == "" || errText == "" {
		return msg + errText
	}
	return msg + ": " + errText
}

// logField pulls one key's value out of a slog text line.
func logField(line, key string) string {
	i := strings.Index(line, " "+key+"=")
	if i < 0 {
		return ""
	}
	v := line[i+len(key)+2:]
	if strings.HasPrefix(v, `"`) {
		if u, err := strconv.QuotedPrefix(v); err == nil {
			s, _ := strconv.Unquote(u)
			return s
		}
	}
	if j := strings.IndexByte(v, ' '); j >= 0 {
		v = v[:j]
	}
	return v
}

// RunUnderManager runs the daemon under the platform service manager, or in
// the foreground until Ctrl-C when started from a terminal, where an error
// (another copy already running, say) ends the command instead of leaving it
// waiting for a signal.
func RunUnderManager(run Run) error {
	run = withDesktopSession(run)
	stopLogs := MaintainLogs(context.Background())
	defer stopLogs()
	if service.Interactive() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return run(ctx)
	}
	s, err := newService(Name, run, nil)
	if err != nil {
		return err
	}
	return s.Run()
}

// TidyUnit brings an installed service definition up to date without a
// reinstall: keys an older version wrote into it move to the 0600 secrets
// file (a systemd unit is readable by anyone who can reach it), and the
// service's own processes get their marker. It is quiet when there is
// nothing to do, and only reads a file unless there is.
func TidyUnit(out io.Writer) {
	if runtime.GOOS == "windows" {
		tidyStartup(out)
		return
	}
	if path := unitPath(runtime.GOOS, userHome(), Name); path != "" && fileExists(path) {
		tidyUnit(path, out)
	}
}

// reloadUnits tells systemd a unit file changed (launchd reads the plist at
// the next load).
var reloadUnits = func() {
	if runtime.GOOS == "linux" {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
}

func tidyUnit(path string, out io.Writer) {
	env := unitEnv(path)
	if !ownUnit(env) {
		return
	}
	// One set up while the home's move waited still names the old home.
	if moved, err := rehomeUnit(path, userHome(), config.Home()); err != nil {
		fmt.Fprintf(out, "warning: couldn't update %s: %v\n", path, err)
	} else if moved {
		reloadUnits()
		env = unitEnv(path)
		fmt.Fprintf(out, "Pointed the background service at %s, where your twin moved.\n", config.Home())
	}
	var secrets []string
	for k := range env {
		if looksSecret(k) {
			secrets = append(secrets, k)
		}
	}
	if len(secrets) == 0 && hasServiceMarker(env) {
		return
	}
	sort.Strings(secrets)
	if len(secrets) > 0 {
		// Keep them first: a key is never dropped from the unit unless it is safe.
		if err := (installer{out: io.Discard, unitFiles: []string{path}}).keepSecrets(); err != nil {
			fmt.Fprintln(out, "warning:", err)
			return
		}
	}
	var add map[string]string
	if !hasServiceMarker(env) {
		add = map[string]string{config.ServiceEnv: "1"}
	}
	changed, err := rewriteUnitEnv(path, secrets, add)
	if err != nil {
		fmt.Fprintf(out, "warning: couldn't update %s: %v\n", path, err)
		return
	}
	if changed {
		reloadUnits()
	}
	if changed && len(secrets) > 0 {
		fmt.Fprintf(out, "Moved %s out of %s into %s, which only you can read.\n", strings.Join(secrets, ", "), path, config.SecretsPath())
	}
}

// ownUnit reports whether a service definition runs this home's twin, so a
// command run with another MIRRIN_HOME (a second profile, a test) never
// touches the owner's service. A definition naming no home, or a default one
// (AntBot's units spell out ANTBOT_HOME=~/.antbot), runs the default home.
func ownUnit(env map[string]string) bool {
	return sameHome(config.HomeEnv(func(k string) string { return env[k] }), config.HomeEnv(os.Getenv))
}

// sameHome reports whether two homes are the same, any default home (as
// config.HomeEnv gives it, "") being the default.
func sameHome(a, b string) bool {
	norm := func(h string) string {
		if h == "" || config.IsDefaultHome(h) {
			return ""
		}
		return filepath.Clean(h)
	}
	a, b = norm(a), norm(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// hasServiceMarker reports whether a service definition's environment sets
// the service marker, under its current name or AntBot's.
func hasServiceMarker(env map[string]string) bool {
	for _, k := range brand.EnvAliases(config.ServiceEnv) {
		if env[k] != "" {
			return true
		}
	}
	return false
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// unitPath is where a per-user service definition lives, or "" where there
// is no such file (Windows keeps services in the registry).
func unitPath(goos, home, name string) string {
	if home == "" {
		return ""
	}
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", name+".plist")
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", name+".service")
	}
	return ""
}

// unitFiles are this home's service definitions under names: another
// home's keys aren't this home's to keep.
func unitFiles(names ...string) []string {
	var out []string
	for _, n := range names {
		if p := unitPath(runtime.GOOS, userHome(), n); p != "" && fileExists(p) && ownUnit(unitEnv(p)) {
			out = append(out, p)
		}
	}
	return out
}
