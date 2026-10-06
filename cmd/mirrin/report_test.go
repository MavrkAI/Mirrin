package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/logs"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/service"
	"github.com/MavrkAI/Mirrin/internal/tray"
)

// reportHome is a twin with secrets, private memories and a log that quotes a
// token, the things a problem report must never carry.
func reportHome(t *testing.T) *config.Config {
	t.Helper()
	home(t)
	cfg := config.Default()
	cfg.API.Listen = "127.0.0.1:1" // nothing answers: never a real twin
	cfg.User.About = "Recovering from knee surgery."
	cfg.LLM.Provider, cfg.LLM.Model = "anthropic", "claude-opus-5"
	cfg.LLM.APIKey = "sk-ant-api03-inline-secret-key"
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Owner = "123456789"
	cfg.Channels.Telegram.Token = "7000000001:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	cfg.Skills.Browser.Enabled = false
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveSecrets(map[string]string{"OPENAI_API_KEY": "saved-openai-secret"}); err != nil {
		t.Fatal(err)
	}
	s, err := memory.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, _ = s.Remember(ctx, "health", "Noor is allergic to penicillin.", "t")
	_ = s.AppendMessage(ctx, "telegram:123456789", llm.Text(llm.RoleUser, "please book my cardiologist"))
	s.Audit(ctx, "message.in", "telegram:123456789", "please book my cardiologist")
	_ = s.RecordUsage(ctx, time.Now().Format(memory.DayFormat), "anthropic/claude-opus-5", "chat", llm.Tokens{Input: 100000, Output: 1000})
	s.Close()
	_ = os.MkdirAll(logs.Dir(config.Home()), 0o700)
	_ = os.WriteFile(logs.Path(config.Home()), []byte(
		`time=2026-09-27T10:00:00 level=ERROR msg="telegram poll" err="Get \"https://api.telegram.org/bot7000000001:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/getUpdates\": EOF"`+"\n"+
			`time=2026-09-27T10:00:01 level=WARN msg="reply not sent: its channel is down" chat=telegram:123456789 key=saved-openai-secret`+"\n"), 0o600)
	reportDeps.health = func(context.Context, *config.Config) (health.Report, string) {
		return health.Report{Results: []health.Result{
			{Name: "model", Label: "Model", State: health.OK, Detail: "anthropic/claude-opus-5"},
			{Name: "email", Label: "Email", State: health.Fail, Detail: "noor@castellano.example: login failed", Fix: "set MIRRIN_EMAIL_PASSWORD"},
			{Name: "channels", Label: "Channels", State: health.Fail, Detail: "Telegram: 401 Unauthorized"},
		}}, "checked just now"
	}
	reportDeps.service = func() string { return "not installed" }
	t.Cleanup(func() { reportDeps.health, reportDeps.service = reportHealth, reportService })
	fakeService(t, false)
	return cfg
}

// fakeService stands in for the machine's background service, so a test
// never stops or starts a real one. It returns the calls made to it.
func fakeService(t *testing.T, installed bool) *[]string {
	t.Helper()
	calls := &[]string{}
	prev := memoryDeps
	memoryDeps.serviceInstalled = func() bool { return installed }
	memoryDeps.stopService = func(cfg *config.Config) error {
		*calls = append(*calls, "stop:"+memoryState(cfg))
		return nil
	}
	memoryDeps.startService = func(cfg *config.Config) error {
		*calls = append(*calls, "start:"+memoryState(cfg))
		return nil
	}
	memoryDeps.lockWait = 300 * time.Millisecond
	t.Cleanup(func() { memoryDeps = prev })
	return calls
}

// memoryState is "sound" or "damaged".
func memoryState(cfg *config.Config) string {
	if detail, err := memory.Check(cfg.DataDir); err == nil && detail == "" {
		return "sound"
	}
	return "damaged"
}

// damageMemory backs up the memory, then ruins memory.db.
func damageMemory(t *testing.T, cfg *config.Config) {
	t.Helper()
	s, err := memory.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "memory.db"), []byte(strings.Repeat("garbage ", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"memory.db-wal", "memory.db-shm"} {
		_ = os.Remove(filepath.Join(cfg.DataDir, f))
	}
}

// The background service restarts a twin whose memory is damaged every few
// seconds; restore pauses it and starts it again once the memory is back.
func TestMemoryRestorePausesTheService(t *testing.T) {
	cfg := reportHome(t)
	calls := fakeService(t, true)
	damageMemory(t, cfg)
	var out strings.Builder
	if err := memoryCmd(&out, []string{"restore"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, ","); got != "stop:damaged,start:sound" {
		t.Fatalf("service calls %s", got)
	}
	for _, want := range []string{"Pausing the background service", "back to the copy from", "running again in the background"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// A sound memory is left alone before the service is touched.
	*calls = nil
	if err := memoryCmd(&out, []string{"restore"}); !errors.Is(err, memory.ErrNotDamaged) || len(*calls) != 0 {
		t.Fatalf("sound memory: %v, service calls %v", err, *calls)
	}
}

// A twin that holds the home (the menu bar app started by hand) keeps a
// restore out, and nothing is changed.
func TestMemoryRestoreWaitsForTheTwinToQuit(t *testing.T) {
	cfg := reportHome(t)
	calls := fakeService(t, true)
	damageMemory(t, cfg)
	release, err := daemon.LockHome(context.Background(), cfg.DataDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var out strings.Builder
	err = memoryCmd(&out, []string{"restore"})
	if err == nil || !strings.Contains(err.Error(), "your twin is running") {
		t.Fatalf("want the running twin named, got %v", err)
	}
	if memoryState(cfg) != "damaged" || strings.Join(*calls, ",") != "stop:damaged" || !strings.Contains(out.String(), "stays paused") {
		t.Fatalf("state %s, calls %v, output:\n%s", memoryState(cfg), *calls, out.String())
	}
}

// A config.yaml that loads but fails a check still says where the data is.
func TestMemoryCommandUsesTheConfiguredFolder(t *testing.T) {
	cfg := reportHome(t)
	cfg.DataDir = filepath.Join(config.Home(), "elsewhere")
	cfg.LLM.Model = "" // fails validation, but the file still loads
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	s, err := memory.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Remember(context.Background(), "user", "one", "t")
	_, _ = s.Remember(context.Background(), "user", "two", "t")
	s.Close()
	var out strings.Builder
	if err := memoryCmd(&out, []string{"check"}); err != nil || !strings.Contains(out.String(), "sound: 2 facts") {
		t.Fatalf("check: %v %q", err, out.String())
	}
	// One that won't load at all says which folder is used instead.
	if err := os.WriteFile(config.Path(), []byte("llm: [this is not\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := memoryCmd(&out, []string{"backups"}); err != nil || !strings.Contains(out.String(), "couldn't be read") || !strings.Contains(out.String(), config.Default().DataDir) {
		t.Fatalf("broken config: %v %q", err, out.String())
	}
}

func TestReportHasWhatHelpsAndNothingPrivate(t *testing.T) {
	reportHome(t)
	txt := buildReport(context.Background(), time.Now())
	for _, leak := range []string{
		"inline-secret-key", "AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "saved-openai-secret", // secrets
		"penicillin", "cardiologist", "knee surgery", // memories, messages, about
		"123456789", "noor@castellano", // phone-like ids, email
	} {
		if strings.Contains(txt, leak) {
			t.Errorf("report leaks %q", leak)
		}
	}
	for _, want := range []string{
		"mirrin " + version, "== Self-check", "Email", "n•••@castellano.example", "fix: set MIRRIN_EMAIL_PASSWORD",
		"Telegram: on", "Now: Telegram: 401 Unauthorized",
		"1 facts, 1 messages", "passed its integrity check", "Model use: US$", "this month",
		"provider: anthropic", "== Recent log", "telegram poll", "bot[hidden]/getUpdates",
		"Background service: not installed",
		// The daily memory copies aren't the encrypted backups, which have
		// their own self-check row (backup merged with observability).
		"Memory copies (data/backups):",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(txt, "\nBackups: ") {
		t.Error("the memory copies are still called Backups, which reads as the encrypted backup's health")
	}
	if t.Failed() {
		t.Log(txt)
	}
}

func TestReportCommandWritesAPrivateFile(t *testing.T) {
	reportHome(t)
	var out strings.Builder
	if err := reportCmd(context.Background(), &out, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(config.Home(), "reports", "mirrin-report-*.txt"))
	if len(files) != 1 {
		t.Fatalf("want one report, got %v", files)
	}
	st, _ := os.Stat(files[0])
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("report is %v, want 0600", st.Mode().Perm())
	}
	for _, want := range []string{files[0], "read it", logs.IssuesURL, "nothing has been sent"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := reportCmd(context.Background(), &out, []string{"--print"}); err != nil || !strings.Contains(out.String(), "Mirrin problem report") {
		t.Fatalf("--print: %v %q", err, out.String())
	}
}

// The report is wanted most when the twin won't start.
func TestReportWorksWhenConfigAndMemoryAreBroken(t *testing.T) {
	cfg := reportHome(t)
	if err := os.WriteFile(config.Path(), []byte("llm: [this is not\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "memory.db"), []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"memory.db-wal", "memory.db-shm"} {
		_ = os.Remove(filepath.Join(cfg.DataDir, f))
	}
	txt := buildReport(context.Background(), time.Now())
	for _, want := range []string{"config.yaml couldn't be used", "memory.db couldn't be opened", "memory file is damaged", "mirrin memory restore"} {
		if !strings.Contains(txt, want) {
			t.Errorf("report lacks %q:\n%s", want, txt)
		}
	}
}

func TestMemoryCommandRestoresADamagedFile(t *testing.T) {
	cfg := reportHome(t)
	ctx := context.Background()
	s, err := memory.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(ctx, 7); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var out strings.Builder
	if err := memoryCmd(&out, []string{"check"}); err != nil || !strings.Contains(out.String(), "sound: 1 facts") {
		t.Fatalf("check: %v %q", err, out.String())
	}
	if err := memoryCmd(&out, []string{"restore"}); err == nil || !strings.Contains(err.Error(), "is fine") {
		t.Fatalf("a sound memory must be left alone: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "memory.db"), []byte(strings.Repeat("garbage ", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"memory.db-wal", "memory.db-shm"} {
		_ = os.Remove(filepath.Join(cfg.DataDir, f))
	}
	if err := memoryCmd(&out, []string{"check"}); err == nil || !strings.Contains(err.Error(), "mirrin memory restore") {
		t.Fatalf("check on a damaged file: %v", err)
	}
	out.Reset()
	if err := memoryCmd(&out, []string{"restore"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "back to the copy from") || !strings.Contains(out.String(), ".damaged-") {
		t.Fatalf("restore said %q", out.String())
	}
	s, err = memory.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if facts, _ := s.AllFacts(ctx, 10); len(facts) != 1 {
		t.Fatalf("restored memory has %d facts", len(facts))
	}
}

func TestLogsGoToTheFileInEveryMode(t *testing.T) {
	home(t)
	t.Setenv("MIRRIN_DEBUG", "")
	errFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	real := os.Stderr
	os.Stderr = errFile
	t.Cleanup(func() { os.Stderr = real })

	// A double-clicked menu bar app: nobody reads its standard error.
	t.Setenv(config.ServiceEnv, "")
	startLogging("tray").Info("tray detail")
	// The background service: its standard error is an unrotated file.
	t.Setenv(config.ServiceEnv, "1")
	svc := startLogging("run")
	svc.Info("routine service detail")
	svc.Info(onlineMarker, "model", "x")
	svc.Warn("service warning")
	// A terminal chat keeps the terminal clear.
	t.Setenv(config.ServiceEnv, "")
	startLogging("chat").Warn("chat warning")

	b, _ := os.ReadFile(logs.Path(config.Home()))
	for _, want := range []string{"tray detail", "routine service detail", onlineMarker, "service warning", "chat warning"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log file lacks %q:\n%s", want, b)
		}
	}
	e, _ := os.ReadFile(errFile.Name())
	if !strings.Contains(string(e), onlineMarker) || !strings.Contains(string(e), "service warning") ||
		strings.Contains(string(e), "routine service detail") || strings.Contains(string(e), "chat warning") {
		t.Errorf("standard error got:\n%s", e)
	}
}

func TestFatalNoticeOncePerReason(t *testing.T) {
	home(t)
	var shown []string
	notifyDesktop = func(title, body string) { shown = append(shown, body) }
	t.Cleanup(func() { notifyDesktop = tray.Notify })
	log := startLogging("tray")
	damaged := "your twin's memory file is damaged, so it stopped before changing anything.\n  File: /x/memory.db\nTo go back to the backup from Sun 27 Sep 09:00, run:\n  mirrin memory restore"
	noteFatal(log, "tray", damaged)
	noteFatal(log, "tray", damaged) // the service manager restarts it
	noteFatal(log, "tray", "config.yaml can't be read")
	noteFatal(log, "chat", "a terminal already shows it")
	want := []string{
		"Your twin's memory file is damaged, so it stopped before changing anything. Run `mirrin memory restore` in Terminal.",
		"config.yaml can't be read. For details, run `mirrin report` in Terminal.",
	}
	if strings.Join(shown, "|") != strings.Join(want, "|") {
		t.Fatalf("notices:\n%q\nwant\n%q", shown, want)
	}
	b, _ := os.ReadFile(logs.Path(config.Home()))
	if !strings.Contains(string(b), "a terminal already shows it") {
		t.Fatalf("every fatal error is logged:\n%s", b)
	}

	// The same reason a day later (fixed, then back again) is shown again.
	shown = nil
	noteFatal(log, "tray", "config.yaml can't be read")
	if len(shown) != 0 {
		t.Fatalf("shown again within the day: %q", shown)
	}
	fatalNoticeEvery = 0
	t.Cleanup(func() { fatalNoticeEvery = 24 * time.Hour })
	noteFatal(log, "tray", "config.yaml can't be read")
	if len(shown) != 1 {
		t.Fatalf("a reason that comes back must be shown again: %q", shown)
	}
	if n := fatalNotice("no model API key: run mirrin init\nmore"); n != "No model API key: run mirrin init. For details, run `mirrin report` in Terminal." {
		t.Fatalf("notice %q", n)
	}
	if n := fatalNotice("x\nThere's no backup yet. Run:\n  mirrin memory restore --fresh"); !strings.Contains(n, "Run `mirrin memory restore --fresh` in Terminal.") {
		t.Fatalf("notice %q", n)
	}
}

// A fatal error is printed once. Under the service manager, standard error
// is the file service.LastError reads, and a second, raw copy of the error
// there would turn Health's one-line "last error" into the whole message.
func TestFatalErrorIsPrintedOnce(t *testing.T) {
	home(t)
	t.Setenv("MIRRIN_DEBUG", "")
	t.Setenv(config.ServiceEnv, "1")
	notifyDesktop = func(string, string) {}
	t.Cleanup(func() { notifyDesktop = tray.Notify })
	if err := os.MkdirAll(logs.Dir(config.Home()), 0o700); err != nil {
		t.Fatal(err)
	}
	errPath := filepath.Join(logs.Dir(config.Home()), "mirrin.err.log")
	errFile, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	real := os.Stderr
	os.Stderr = errFile
	t.Cleanup(func() { os.Stderr = real; errFile.Close() })

	log := startLogging("tray")
	log.Info(onlineMarker) // an earlier good start
	msg := "your twin's memory file is damaged, so it stopped before changing anything.\n  File: /x/memory.db\nTo go back to the backup from Sun 27 Sep 09:00, run:\n  mirrin memory restore"
	// What main does with an error it stops on.
	fmt.Fprintln(os.Stderr, "error:", msg)
	noteFatal(log, "tray", msg)

	b, _ := os.ReadFile(errPath)
	if n := strings.Count(string(b), "error:"); n != 1 || strings.Contains(string(b), "level=ERROR") {
		t.Fatalf("the error must reach standard error once (got %d), with no raw log copy:\n%s", n, b)
	}
	if got, want := service.LastError(), "your twin's memory file is damaged, so it stopped before changing anything."; got != want {
		t.Fatalf("LastError = %q, want %q", got, want)
	}
	if f, _ := os.ReadFile(logs.Path(config.Home())); !strings.Contains(string(f), "msg=stopped") {
		t.Fatalf("the log file must still record it:\n%s", f)
	}
}

// Regression (observability merged with backup): a mistyped Recovery Kit
// word stopped `mirrin restore` with an error that quotes the typed word and
// the real one it suggests, and noteFatal wrote it to logs/mirrin.log, from
// where `mirrin report` shared it. The terminal still shows it; the log
// keeps only that the words weren't accepted.
func TestRecoveryKitWordsNeverReachTheLog(t *testing.T) {
	home(t)
	_, err := backup.ParsePhrase("abandon ability able about above absent absorb abstrxct absurd abuse access accident")
	var pe *backup.PhraseError
	if !errors.As(err, &pe) || pe.Suggest == "" {
		t.Fatalf("want a phrase error with a suggestion, got %v", err)
	}
	got := logSafe(err, err.Error())
	for _, word := range []string{pe.Got, pe.Suggest, "abstrxct", "abstract"} {
		if strings.Contains(got, word) {
			t.Fatalf("the log would keep %q: %q", word, got)
		}
	}
	// `mirrin backup key` and friends read the words too, and log to the file.
	log := startLogging("backup")
	noteFatal(log, "backup", logSafe(fmt.Errorf("backup: %w", err), err.Error()))
	b, _ := os.ReadFile(logs.Path(config.Home()))
	if strings.Contains(string(b), "abstr") || !strings.Contains(string(b), "Recovery Kit words weren't accepted") {
		t.Fatalf("log:\n%s", b)
	}
	if other := errors.New("config.yaml can't be read"); logSafe(other, other.Error()) != other.Error() {
		t.Fatal("other errors are logged as they are")
	}
	// An age identity pasted into an error is hidden by its shape.
	if r := logs.NewRedactorWith().String("key AGE-SECRET-KEY-PQ-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ ok"); strings.Contains(r, "AGE-SECRET-KEY") {
		t.Fatalf("redacted %q", r)
	}
}

// Regression (release merged with observability): every command started
// writing logs/mirrin.log and logs/crash.log into the home, uninstall too,
// before it looked at that folder. An empty or missing home then counted as
// a twin, uninstall refused it and left a folder behind, and on Windows the
// open log files stopped the delete part-way. Uninstall opens nothing there.
func TestUninstallLeavesNoLogInTheHome(t *testing.T) {
	t.Setenv("MIRRIN_DEBUG", "")
	h := filepath.Join(t.TempDir(), "home")
	t.Setenv("MIRRIN_HOME", h)
	log := startLogging("uninstall")
	log.Error("x")
	noteFatal(log, "uninstall", "x")
	if _, err := os.Stat(h); !os.IsNotExist(err) {
		t.Fatalf("uninstall's logging made the home (%v)", err)
	}
	empty := t.TempDir()
	t.Setenv("MIRRIN_HOME", empty)
	startLogging("uninstall").Error("x")
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Fatalf("uninstall's logging wrote into an empty home: %v", ents)
	}
	// `mirrin restore` moves the home aside; on Windows a folder with an
	// open file in it can't be renamed, so it holds none there either.
	startLogging("restore").Error("x")
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Fatalf("restore's logging opened a file in the home it moves: %v", ents)
	}
}

// The report names the zone the twin follows: a pinned one as set, and for
// "auto" or "local" (any case) the system's, never the raw setting.
func TestReportNamesTheZoneItFollows(t *testing.T) {
	cfg := config.Default()
	for _, tz := range []string{"auto", " Local ", "local", ""} {
		cfg.User.Timezone = tz
		if got := zoneName(cfg); strings.HasPrefix(got, "auto") || strings.HasPrefix(strings.TrimSpace(got), "Local ") || !strings.Contains(got, "following the system") {
			t.Errorf("%q: %q", tz, got)
		}
		if zone(cfg) != time.Local {
			t.Errorf("%q: zone %v", tz, zone(cfg))
		}
	}
	cfg.User.Timezone = " Asia/Tokyo "
	if got := zoneName(cfg); !strings.HasPrefix(got, "Asia/Tokyo (") || zone(cfg).String() != "Asia/Tokyo" {
		t.Errorf("pinned: %q %v", got, zone(cfg))
	}
}
