package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/logs"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/service"
	"github.com/MavrkAI/Mirrin/internal/tray"
)

// onlineMarker is the line the daemon logs once it is up (daemon.go); the
// service's own log is read for it (service.LastError), so it always goes
// there.
const onlineMarker = "Mirrin online"

// startLogging sets up this process's log: every command writes to
// ~/.mirrin/logs/mirrin.log (rotated at 5 MB, three old files kept, secrets
// taken out), so a double-clicked menu bar app leaves a record too. Commands
// that own the terminal (chat, voice, setup) keep it clear; the background
// twin also writes to standard error, which under the service manager is a
// file that isn't rotated, so there only warnings, errors and the "online"
// line go.
func startLogging(cmd string) *slog.Logger {
	switch cmd {
	case "uninstall", "restore":
		// Uninstall may delete the home and restore moves it aside for the
		// snapshot's: neither may create one, nor hold a file open in it
		// (Windows can't delete or rename a folder with an open file). What
		// they do and any error is printed; the restored twin's own log
		// carries on from there.
		return slog.New(slog.DiscardHandler)
	}
	home := config.Home()
	_ = logs.CaptureCrashes(home)
	o := logs.Options{Home: home, Level: slog.LevelInfo, Console: os.Stderr, ConsoleLevel: slog.LevelInfo}
	switch cmd {
	case "chat", "voice", "init", "doctor", "report", "usage", "memory":
		o.Console = nil
	case "run", "tray":
		if config.ServiceMarked() {
			o.ConsoleLevel = slog.LevelWarn
			o.ConsoleAlways = func(msg string) bool { return msg == onlineMarker }
		}
	}
	if os.Getenv("MIRRIN_DEBUG") != "" {
		o.Level, o.Console, o.ConsoleLevel = slog.LevelDebug, os.Stderr, slog.LevelDebug
	}
	log, closer, err := logs.New(o)
	if err != nil && o.Console == nil {
		// No log file: the important things go to the terminal rather than nowhere.
		log, closer, _ = logs.New(logs.Options{Home: home, Console: os.Stderr, ConsoleLevel: slog.LevelWarn})
	}
	keepLog(closer)
	return log
}

// openLogs are the log files commands opened. They stay open until the
// program ends; tests, which run many commands in one program, close them
// (Windows can't delete an open file).
var (
	openLogsMu sync.Mutex
	openLogs   []io.Closer
)

func keepLog(c io.Closer) {
	if c == nil {
		return
	}
	openLogsMu.Lock()
	defer openLogsMu.Unlock()
	openLogs = append(openLogs, c)
}

// closeLogs closes the log files opened so far.
func closeLogs() {
	openLogsMu.Lock()
	defer openLogsMu.Unlock()
	for _, c := range openLogs {
		_ = c.Close()
	}
	openLogs = nil
}

// noteFatal records why mirrin is stopping, in the log file only: the
// terminal (or, under the service manager, the file service.LastError reads)
// has already had it as one "error:" line. The menu bar app has no terminal
// to print to, so it also says so on the desktop, once a day per distinct
// reason (the service manager restarts it, and the same notice every ten
// seconds would be worse than none).
func noteFatal(log *slog.Logger, cmd, msg string) {
	log.ErrorContext(logs.FileOnly(context.Background()), "stopped", "command", cmd, "err", msg)
	if cmd != "tray" {
		return
	}
	sum := sha256.Sum256([]byte(msg))
	id := hex.EncodeToString(sum[:])
	stamp := filepath.Join(logs.Dir(config.Home()), "last-stop")
	if b, err := os.ReadFile(stamp); err == nil {
		last, at, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
		if sec, err := strconv.ParseInt(at, 10, 64); err == nil && last == id && time.Since(time.Unix(sec, 0)) < fatalNoticeEvery {
			return
		}
	}
	_ = os.MkdirAll(filepath.Dir(stamp), 0o700)
	_ = os.WriteFile(stamp, []byte(fmt.Sprintf("%s %d", id, time.Now().Unix())), 0o600)
	notifyDesktop("Mirrin couldn't start", fatalNotice(msg))
}

// logSafe is what the log file keeps of a command's fatal error: msg, except
// for an error that quotes the owner's Recovery Kit words (a mistyped word
// and the real one it suggests), which the terminal shows and the log, and
// so `mirrin report`, never keep.
func logSafe(err error, msg string) string {
	var pe *backup.PhraseError
	if errors.As(err, &pe) {
		return "the Recovery Kit words weren't accepted"
	}
	return msg
}

// fatalNoticeEvery is how often the same reason for not starting is shown
// again, so one that comes back weeks after it was fixed isn't silent.
var fatalNoticeEvery = 24 * time.Hour

// reRestore finds the command a damaged memory's message asks for.
var reRestore = regexp.MustCompile(`mirrin memory restore(?: --fresh)?`)

// fatalNotice is the desktop notice for msg: its first line as a sentence,
// and the command that fixes it when there is one.
func fatalNotice(msg string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	first = strings.TrimSpace(first)
	word, _, _ := strings.Cut(first, " ")
	if r, n := utf8.DecodeRuneInString(first); n > 0 && !strings.ContainsAny(word, "./_`:") {
		first = string(unicode.ToUpper(r)) + first[n:] // not a file or command name
	}
	if !strings.HasSuffix(first, ".") {
		first += "."
	}
	if fix := reRestore.FindString(msg); fix != "" {
		return first + " Run `" + fix + "` in Terminal."
	}
	return first + " For details, run `mirrin report` in Terminal."
}

// notifyDesktop shows a desktop notification (replaced in tests).
var notifyDesktop = tray.Notify

// reportDeps are the slow or machine-wide parts of a report, replaced in tests.
var reportDeps = struct {
	health  func(ctx context.Context, cfg *config.Config) (health.Report, string)
	service func() string
}{health: reportHealth, service: reportService}

// reportCmd writes a problem report: `mirrin report [--print]`.
func reportCmd(ctx context.Context, out io.Writer, args []string) error {
	print := false
	for _, a := range args {
		switch a {
		case "--print", "-":
			print = true
		default:
			return fmt.Errorf("usage: mirrin report [--print]")
		}
	}
	text := buildReport(ctx, time.Now())
	if print {
		_, err := io.WriteString(out, text)
		return err
	}
	dir := filepath.Join(config.Home(), "reports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "mirrin-report-"+time.Now().Format("20060102-150405")+".txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote your problem report to:\n  %s\n\n", path)
	fmt.Fprintln(out, "It has versions, self-checks, settings and recent log lines. Keys, passwords")
	fmt.Fprintln(out, "and messages are left out, and nothing has been sent anywhere. Please read it")
	fmt.Fprintln(out, "before you share it, and delete anything you'd rather keep to yourself.")
	fmt.Fprintf(out, "\nTo ask for help, open a bug report and attach the file:\n  %s\n", logs.IssuesURL)
	if runtime.GOOS == "darwin" {
		fmt.Fprintf(out, "To read it now: open %q\n", path)
	}
	return nil
}

// buildReport gathers the report. Every part copes with the twin being
// broken, since that is when a report is wanted: a config that won't load,
// a memory file that won't open, a daemon that isn't running.
func buildReport(ctx context.Context, now time.Time) string {
	home := config.Home()
	r := &logs.Report{Made: now, Version: version}
	cfg, cfgErr := config.Load()
	if cfg == nil {
		cfg = config.Default()
	}

	sys := []string{
		fmt.Sprintf("mirrin %s on %s/%s (%s)", version, runtime.GOOS, runtime.GOARCH, runtime.Version()),
		"System: " + osVersion(),
		"Home: " + home,
		"Time zone: " + zoneName(cfg),
		"Background service: " + reportDeps.service(),
	}
	if rem := config.LoadRemote(); rem != nil {
		sys = append(sys, "Paired with a twin on another machine: "+rem.Name)
	}
	r.Add("This computer", sys...)

	rep, from := reportDeps.health(ctx, cfg)
	var checks []string
	if from != "" {
		checks = append(checks, "("+from+")")
	}
	for _, x := range rep.Results {
		checks = append(checks, fmt.Sprintf(" %s %-22s %s", mark(x.State), x.Label, x.Detail))
		if x.Fix != "" && x.State != health.OK {
			checks = append(checks, "   fix: "+x.Fix)
		}
	}
	if len(rep.Results) > 0 {
		checks = append(checks, rep.Summary())
	}
	r.Add("Self-check", checks...)

	var chans []string
	for _, k := range cfg.Connectors() {
		if k.Enabled {
			chans = append(chans, k.Label+": on")
		}
	}
	if len(chans) == 0 {
		chans = append(chans, "none turned on")
	}
	for _, x := range rep.Results {
		if x.Name == "channels" {
			chans = append(chans, "Now: "+x.Detail)
		}
	}
	r.Add("Channels", chans...)

	r.Add("Memory and model use", memoryLines(ctx, cfg)...)

	if cfgErr != nil {
		r.Add("Settings", "config.yaml couldn't be used: "+cfgErr.Error())
	} else if txt, err := logs.ConfigText(cfg); err == nil {
		r.Add("Settings (config.yaml; secrets and personal details hidden, empty settings left out)", strings.TrimRight(txt, "\n"))
	}

	if lines, err := logs.Tail(home, 200); err == nil && len(lines) > 0 {
		r.Add("Recent log (the last 200 lines of logs/mirrin.log)", lines...)
	} else {
		r.Add("Recent log", "nothing logged yet")
	}
	for _, name := range []string{"mirrin.err.log", "mirrin.err"} {
		if lines, err := logs.TailFiles(40, filepath.Join(logs.Dir(home), name)); err == nil && len(lines) > 0 {
			r.Add("Background service output (the last 40 lines of logs/"+name+")", lines...)
			break
		}
	}
	if lines, err := logs.TailFiles(80, logs.CrashPath(home)+".1", logs.CrashPath(home)); err == nil && len(lines) > 0 {
		r.Add("Crashes (logs/crash.log)", lines...)
	}
	return r.Text(logs.NewRedactor(home))
}

// reportHealth runs the self-checks: the running twin's own, or a one-off
// round like `mirrin doctor` when none is running.
func reportHealth(ctx context.Context, cfg *config.Config) (health.Report, string) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if c := api.Connect(cfg.API.Listen, cfg.DataDir); c != nil {
		if rep, err := c.RunHealth(ctx); err == nil {
			return rep, "from the running twin"
		}
	}
	d, err := daemon.New(cfg, daemon.Options{Headless: true, Version: version, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		return health.Report{Results: []health.Result{{Name: "twin", Label: "Twin", State: health.Fail, Detail: "couldn't start: " + err.Error()}}}, "the twin isn't running; checked just now"
	}
	defer d.Close()
	return d.RunHealth(ctx), "the twin isn't running; checked just now"
}

// reportService describes the background service.
func reportService() string {
	installed, running := service.State()
	switch {
	case running:
		return "installed and running"
	case installed:
		s := "installed, not running"
		if e := service.LastError(); e != "" {
			s += " (last error: " + e + ")"
		}
		return s
	}
	return "not installed"
}

// memoryLines describes memory.db and model use without revealing what's in it.
func memoryLines(ctx context.Context, cfg *config.Config) []string {
	s, err := memory.Open(cfg.DataDir)
	if err != nil {
		return []string{"memory.db couldn't be opened: " + err.Error()}
	}
	defer s.Close()
	var out []string
	if st, err := s.Stats(ctx); err == nil {
		out = append(out,
			fmt.Sprintf("memory.db: %s, schema version %d, passed its integrity check", memorySize(st.SizeBytes), st.SchemaVersion),
			fmt.Sprintf("Holds: %d facts, %d messages, %d reminders, %d approvals, %d activity-log entries", st.Facts, st.Messages, st.Reminders, st.Approvals, st.Audit))
		// The daily unencrypted copies in data/backups, not the encrypted
		// backups (`mirrin backup`), whose state is the "Backup" self-check.
		if st.Backups > 0 {
			out = append(out, fmt.Sprintf("Memory copies (data/backups): %d, the newest from %s", st.Backups, st.NewestBackup.Local().Format("Mon 2 Jan 15:04")))
		} else {
			out = append(out, "Memory copies (data/backups): none yet (the twin makes one a day while it runs)")
		}
	}
	if sp, err := s.Spend(ctx, time.Now(), zone(cfg), daemon.PriceBook(cfg.Usage), cfg.Usage.MonthlyBudget); err == nil {
		out = append(out, "Model use: "+spendLine(sp))
	}
	out = append(out, fmt.Sprintf("Model: %s/%s (effort %s)", cfg.LLM.Provider, cfg.LLM.Model, cfg.LLM.Effort))
	return out
}

func mark(s health.State) string {
	return map[health.State]string{health.OK: "✓", health.Warn: "!", health.Fail: "✗", health.Off: "-"}[s]
}

func memorySize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}

// zone is the owner's time zone from config: the one they pinned, or the
// system's ("", "Local", "auto", any case: config.FollowsSystem).
func zone(cfg *config.Config) *time.Location {
	if tz := cfg.User.Timezone; !config.FollowsSystem(tz) {
		if l, err := time.LoadLocation(strings.TrimSpace(tz)); err == nil {
			return l
		}
	}
	return time.Local
}

// zoneName names the zone for the report: a pinned one as set, else the
// system's by name ("Australia/Sydney (AEST)"), never the raw "auto".
func zoneName(cfg *config.Config) string {
	name, _ := time.Now().In(zone(cfg)).Zone()
	if tz := cfg.User.Timezone; !config.FollowsSystem(tz) {
		return strings.TrimSpace(tz) + " (" + name + ")"
	}
	sys := config.LocalTimezone()
	if sys == "" {
		// Windows has no IANA name to read, and time.Local only calls
		// itself "Local": the zone's own abbreviation says more.
		return name + " (following the system)"
	}
	return sys + " (" + name + ", following the system)"
}

// osVersion names the operating system release, as well as it can quickly.
func osVersion() string {
	switch runtime.GOOS {
	case "darwin":
		if b, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			return "macOS " + strings.TrimSpace(string(b))
		}
	case "linux":
		if b, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
					return strings.Trim(v, `"`)
				}
			}
		}
	}
	return runtime.GOOS
}

// memoryDeps are the machine-wide parts of `mirrin memory restore`, replaced
// in tests.
var memoryDeps = struct {
	serviceInstalled func() bool
	stopService      func(cfg *config.Config) error
	startService     func(cfg *config.Config) error
	lockWait         time.Duration
}{
	serviceInstalled: func() bool { installed, _ := service.State(); return installed },
	stopService:      func(cfg *config.Config) error { return service.Control(cfg, "stop", nil, io.Discard) },
	startService: func(cfg *config.Config) error {
		service.Mode = serviceMode(runtime.GOOS, cfg.Tray, tray.Available())
		var up func() bool
		if cfg.API.Listen != "" {
			up = func() bool { return api.Connect(cfg.API.Listen, cfg.DataDir) != nil }
		}
		return service.Control(cfg, "start", up, io.Discard)
	},
	lockWait: 15 * time.Second,
}

// memoryCmd looks after memory.db: `mirrin memory check` and
// `mirrin memory restore [--fresh] [--anyway] [backup file]`.
func memoryCmd(out io.Writer, args []string) error {
	cfg, err := config.Load()
	if cfg == nil {
		// A config.yaml that won't load mustn't stand between you and your memory.
		cfg = config.Default()
		if _, statErr := os.Stat(config.Path()); statErr == nil {
			fmt.Fprintf(out, "config.yaml couldn't be read (%v), so this looks in the usual folder: %s\n\n", err, cfg.DataDir)
		}
	}
	ctx := context.Background()
	sub := "check"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "check":
		s, err := memory.Open(cfg.DataDir)
		if err != nil {
			return err
		}
		defer s.Close()
		st, err := s.Stats(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Your twin's memory is sound: %d facts, %d messages, %s.\n", st.Facts, st.Messages, memorySize(st.SizeBytes))
		if st.Backups > 0 {
			fmt.Fprintf(out, "%d backups; the newest is from %s.\n", st.Backups, st.NewestBackup.Local().Format("Mon 2 Jan 15:04"))
		} else {
			fmt.Fprintln(out, "No backups yet: the twin makes one a day while it runs.")
		}
		return nil
	case "restore":
		var opts memory.RestoreOptions
		for _, a := range args[1:] {
			switch a {
			case "--fresh":
				opts.Fresh = true
			case "--anyway":
				opts.Anyway = true
			default:
				if strings.HasPrefix(a, "-") {
					return fmt.Errorf("usage: mirrin memory restore [--fresh] [--anyway] [backup file]")
				}
				opts.From = a
			}
		}
		return restoreMemory(ctx, out, cfg, opts)
	case "backups":
		files, _ := filepath.Glob(filepath.Join(cfg.DataDir, "backups", "memory-*.db"))
		sort.Sort(sort.Reverse(sort.StringSlice(files)))
		if len(files) == 0 {
			fmt.Fprintln(out, "No backups yet: the twin makes one a day while it runs.")
			return nil
		}
		for _, f := range files {
			fmt.Fprintln(out, f)
		}
		return nil
	}
	return fmt.Errorf("usage: mirrin memory check | backups | restore [--fresh] [--anyway] [backup file]")
}

// restoreMemory puts a backup (or an empty memory) in place of memory.db.
// A twin must not have the file open meanwhile: a running one is asked to
// quit first; the background service, which restarts a twin with a damaged
// memory every few seconds, is stopped for the moment and started again
// after; and the home's claim is held throughout, so no twin can start.
func restoreMemory(ctx context.Context, out io.Writer, cfg *config.Config, opts memory.RestoreOptions) error {
	running := fmt.Errorf("your twin is running, so its memory can't be swapped under it. Quit it first (Quit in its menu, or mirrin service stop), then run this again")
	if api.Connect(cfg.API.Listen, cfg.DataDir) != nil {
		return running
	}
	if !opts.Anyway {
		if detail, err := memory.Check(cfg.DataDir); err == nil && detail == "" {
			return memory.ErrNotDamaged // before anything is stopped
		}
	}
	svc := memoryDeps.serviceInstalled()
	if svc {
		fmt.Fprintln(out, "Pausing the background service while your memory is put back…")
		_ = memoryDeps.stopService(cfg) // it may not have been running; the claim below still keeps a twin out
	}
	paused := func(err error) error {
		if svc {
			fmt.Fprintln(out, "The background service stays paused until your memory is put back. To start it anyway: mirrin service start")
		}
		return err
	}
	release, err := daemon.LockHome(ctx, cfg.DataDir, memoryDeps.lockWait)
	if err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			return paused(running)
		}
		return paused(err)
	}
	res, err := memory.Restore(cfg.DataDir, opts)
	release()
	if err != nil {
		return paused(err)
	}
	if res.From != "" {
		fmt.Fprintf(out, "Your twin's memory is back to the copy from %s.\n", res.FromAt.Local().Format("Mon 2 Jan 15:04"))
	} else {
		fmt.Fprintln(out, "Your twin starts again with an empty memory. Your settings, persona and protocols are as they were.")
	}
	if res.Kept != "" {
		fmt.Fprintf(out, "The previous file is kept at %s.\n", res.Kept)
	}
	if svc {
		if err := memoryDeps.startService(cfg); err != nil {
			fmt.Fprintln(out, "The background service didn't start again. Start it with: mirrin service start")
		} else {
			fmt.Fprintln(out, "Your twin is running again in the background.")
		}
		return nil
	}
	fmt.Fprintln(out, "Start your twin again: open Mirrin.")
	return nil
}
