package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// kitLines are the numbered word lines of a printed Recovery Kit.
var kitLines = regexp.MustCompile(`(?m)^\s+\d+\. \S+\s+\d+\. \S+$`)

// wordsFromKit reads the words back off a printed kit.
func wordsFromKit(t *testing.T, out string) backup.Phrase {
	t.Helper()
	p, err := backup.ParsePhrase(strings.Join(kitLines.FindAllString(out, -1), "\n"))
	if err != nil {
		t.Fatalf("no kit in the output: %v\n%s", err, out)
	}
	return p
}

// kitReader answers the word check by reading the kit already printed to
// out, as a person copying from the screen would; wrong answers first.
type kitReader struct {
	out    *bytes.Buffer
	wrong  int
	buf    []byte
	t      *testing.T
	answer string
}

func (k *kitReader) Read(p []byte) (int, error) {
	if len(k.buf) == 0 {
		switch {
		case k.wrong > 0:
			k.wrong--
			k.buf = []byte("nope\n")
		case k.answer == "":
			k.answer = wordsFromKit(k.t, k.out.String()).Word(backup.CheckWord)
			k.buf = []byte(k.answer + "\n")
		default:
			return 0, io.EOF
		}
	}
	n := copy(p, k.buf)
	k.buf = k.buf[n:]
	return n, nil
}

func testTerminal(in io.Reader) (*terminal, *bytes.Buffer, *bytes.Buffer) {
	var out, msg bytes.Buffer
	return &terminal{in: bufio.NewReader(in), out: &out, msg: &msg}, &out, &msg
}

func twinHome(t *testing.T) *config.Config {
	t.Helper()
	home(t)
	cfg := config.Default()
	cfg.Name = "Jeeves"
	cfg.LLM.APIKey = "sk-test"
	// No local API address: twinRunning then only looks at data/mirrin.lock,
	// and never calls a real twin that may be listening on 127.0.0.1:7742.
	cfg.API.Listen = ""
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBackupInitNowVerifyKeyAndRestore(t *testing.T) {
	cfg := twinHome(t)
	dir := t.TempDir()
	ctx := context.Background()

	// init: shows the kit, asks for word 7 (a wrong try first), backs up.
	var out, msg bytes.Buffer
	kr := &kitReader{out: &out, wrong: 1, t: t}
	term := &terminal{in: bufio.NewReader(kr), out: &out, msg: &msg}
	if err := runBackup(ctx, term, []string{"init", "--folder", dir}); err != nil {
		t.Fatalf("%v\n%s%s", err, out.String(), msg.String())
	}
	p := wordsFromKit(t, out.String())
	if !strings.Contains(msg.String(), "That isn't word 7") || !strings.Contains(out.String(), "Backups are on") || !strings.Contains(out.String(), "Backed up Jeeves") {
		t.Fatalf("init said:\n%s\n%s", out.String(), msg.String())
	}
	raw, _ := os.ReadFile(config.Path())
	if strings.Contains(string(raw), p.Word(1)+" "+p.Word(2)) || !strings.Contains(string(raw), p.RecoveryPub()) {
		t.Fatalf("config after init:\n%s", raw)
	}

	// Asking again doesn't make new words.
	term, o2, _ := testTerminal(strings.NewReader(""))
	if err := runBackup(ctx, term, []string{"init"}); err != nil || !strings.Contains(o2.String(), "already on") {
		t.Fatalf("%v %s", err, o2)
	}

	term, o3, _ := testTerminal(strings.NewReader(""))
	if err := runBackup(ctx, term, []string{"now"}); err != nil || !strings.Contains(o3.String(), "Backed up") {
		t.Fatalf("now: %v %s", err, o3)
	}
	term, o4, _ := testTerminal(strings.NewReader(""))
	if err := runBackup(ctx, term, []string{"status"}); err != nil || !strings.Contains(o4.String(), "Backups: on, to "+dir) || !strings.Contains(o4.String(), "#2") ||
		!strings.Contains(o4.String(), "once Mirrin is running; it isn't now") {
		t.Fatalf("status: %v\n%s", err, o4)
	}
	words := strings.Join(p.Words(), " ") + "\n"
	term, o5, _ := testTerminal(strings.NewReader(words))
	if err := runBackup(ctx, term, []string{"verify"}); err != nil || !strings.Contains(o5.String(), "is intact: #2") {
		t.Fatalf("verify: %v\n%s", err, o5)
	}
	term, o6, _ := testTerminal(strings.NewReader(words))
	if err := runBackup(ctx, term, []string{"list"}); err != nil || strings.Count(o6.String(), "snap-") != 2 {
		t.Fatalf("list: %v\n%s", err, o6)
	}

	// key --age prints exactly one line, which age reads as an identity.
	term, o7, m7 := testTerminal(strings.NewReader(words))
	if err := runBackup(ctx, term, []string{"key", "--age"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(o7.String(), "AGE-SECRET-KEY-PQ-1") || strings.Count(o7.String(), "\n") != 1 || !strings.Contains(m7.String(), "age -d") {
		t.Fatalf("key: %q", o7.String())
	}
	ids, err := age.ParseIdentities(strings.NewReader(o7.String()))
	if err != nil {
		t.Fatal(err)
	}
	snaps, _ := filepath.Glob(filepath.Join(dir, p.Namespace(), "snap-*.age"))
	f, err := os.Open(snaps[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.ReadManifest(f, ids); err != nil {
		t.Fatalf("the printed key doesn't open a backup: %v", err)
	}
	f.Close()

	// Other words are caught before anything is read.
	other, _ := backup.NewPhrase()
	term, _, _ = testTerminal(strings.NewReader(strings.Join(other.Words(), " ") + "\n"))
	if err := runBackup(ctx, term, []string{"verify"}); err == nil || !strings.Contains(err.Error(), "not the ones for this twin") {
		t.Fatalf("other words: %v", err)
	}

	// A new machine: another home, restore from the folder.
	t.Setenv("MIRRIN_HOME", t.TempDir())
	term, o8, _ := testTerminal(strings.NewReader(words))
	if err := runRestore(ctx, term, []string{"--from", dir}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, want := range []string{"Restored Jeeves", "#2", "new token", "Start your twin"} {
		if !strings.Contains(o8.String(), want) {
			t.Errorf("restore output lacks %q:\n%s", want, o8)
		}
	}
	restored, err := config.Load()
	if err != nil && restored == nil {
		t.Fatal(err)
	}
	if restored.Name != "Jeeves" || restored.Backup.Recipient != p.Recipient() || !strings.HasPrefix(restored.DataDir, config.Home()) {
		t.Fatalf("restored config: %s %q %q", restored.Name, restored.DataDir, config.Home())
	}
	// The token the restore made is one the API takes as it is.
	made, _ := os.ReadFile(filepath.Join(restored.DataDir, "api.token"))
	tok, err := api.LoadOrCreateToken(restored.DataDir)
	if err != nil || tok != strings.TrimSpace(string(made)) {
		t.Fatalf("api token: %q vs %q (%v)", tok, made, err)
	}
	_ = cfg

	// Restoring again over the twin now here needs a yes.
	term, _, _ = testTerminal(strings.NewReader(words))
	if err := runRestore(ctx, term, []string{"--from", dir}); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("restore over a twin without --yes: %v", err)
	}
}

func TestBackupInitWithoutTheWordTurnsNothingOn(t *testing.T) {
	twinHome(t)
	var out, msg bytes.Buffer
	kr := &kitReader{out: &out, wrong: 3, t: t}
	term := &terminal{in: bufio.NewReader(kr), out: &out, msg: &msg}
	err := runBackup(context.Background(), term, []string{"init", "--folder", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "weren't turned on") {
		t.Fatalf("got %v", err)
	}
	if s, _ := backup.LoadSettings(config.Path()); s.Recipient != "" {
		t.Fatal("backups were turned on without the words being checked")
	}
}

func TestBackupStatusWhenOff(t *testing.T) {
	twinHome(t)
	term, out, _ := testTerminal(strings.NewReader(""))
	if err := runBackup(context.Background(), term, nil); err != nil || !strings.Contains(out.String(), "Backups are off") || !strings.Contains(out.String(), "mirrin backup init") {
		t.Fatalf("%v %s", err, out)
	}
}

func TestRestoreSaysWhatIsWrongWithTheWords(t *testing.T) {
	home(t)
	term, _, _ := testTerminal(strings.NewReader("legal winner thank year wave sausage worth usefull legal winner thank yellow\n"))
	err := runRestore(context.Background(), term, []string{"--from", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), `word 8 ("usefull")`) || !strings.Contains(err.Error(), `"useful"`) {
		t.Fatalf("got %v", err)
	}
	term, _, _ = testTerminal(strings.NewReader("legal winner thank year wave sausage worth useful legal winner thank yellow\n"))
	err = runRestore(context.Background(), term, []string{"--from", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no backups found") {
		t.Fatalf("empty folder: %v", err)
	}
}

func TestBackupIsACommand(t *testing.T) {
	var out strings.Builder
	unknownCommand(&out, "bakup")
	if !strings.Contains(out.String(), "mirrin backup") {
		t.Fatalf("got %q", out.String())
	}
}

// At a terminal a typo is pointed out and the words asked for again.
func TestATypoIsAskedAgainAtATerminal(t *testing.T) {
	good := "legal winner thank year wave sausage worth useful legal winner thank yellow"
	in := "legal winner thank year wave sausage worth usefull legal winner thank yellow\n" + good + "\n"
	term, _, msg := testTerminal(strings.NewReader(in))
	term.tty = true
	p, err := term.readPhrase()
	if err != nil || strings.Join(p.Words(), " ") != good {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(msg.String(), `Word 8 ("usefull") isn't one of the Recovery Kit words. Did you mean "useful"?`+"\n") {
		t.Fatalf("prompt: %q", msg.String())
	}
}

// initWith runs `mirrin backup init` with args, answering the word check
// from the kit it prints, and returns the words and what it said.
func initWith(t *testing.T, args ...string) (backup.Phrase, string) {
	t.Helper()
	var out, msg bytes.Buffer
	term := &terminal{in: bufio.NewReader(&kitReader{out: &out, t: t}), out: &out, msg: &msg}
	if err := runBackup(context.Background(), term, append([]string{"init"}, args...)); err != nil {
		t.Fatalf("%v\n%s%s", err, out.String(), msg.String())
	}
	return wordsFromKit(t, out.String()), out.String()
}

// New words start a new folder; the old one is named, since nothing
// prunes it any more.
func TestNewWordsNameTheOldFolder(t *testing.T) {
	twinHome(t)
	dir := t.TempDir()
	old, _ := initWith(t, "--folder", dir)
	_, said := initWith(t, "--new")
	if !strings.Contains(said, filepath.Join(dir, old.Namespace())) || !strings.Contains(said, "doesn't prune them") {
		t.Fatalf("init --new said:\n%s", said)
	}
	if !strings.Contains(said, "every night at 03:30 while Mirrin is running") {
		t.Fatalf("the schedule promise: %s", said)
	}
}

// A folder inside the twin is refused before any words are shown.
func TestInitRefusesAFolderInsideTheTwinBeforeTheWords(t *testing.T) {
	twinHome(t)
	term, out, _ := testTerminal(strings.NewReader(""))
	err := runBackup(context.Background(), term, []string{"init", "--folder", filepath.Join(config.Home(), "backups")})
	if err == nil || !strings.Contains(err.Error(), "outside Mirrin's own folder") || strings.Contains(out.String(), "Recovery Kit") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// A snapshot dated after today isn't restored unless it is named.
func TestRestorePassesOverAFutureDatedSnapshot(t *testing.T) {
	twinHome(t)
	dir := t.TempDir()
	p, _ := initWith(t, "--folder", dir)
	cfg, _ := config.Load()
	e, err := backup.NewEngine(cfg, config.Home(), "test")
	if err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.Now().Add(90 * 24 * time.Hour) }
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIRRIN_HOME", t.TempDir())
	term, out, msg := testTerminal(strings.NewReader(strings.Join(p.Words(), " ") + "\n"))
	if err := runRestore(context.Background(), term, []string{"--from", dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.String(), "Passed over snap-") || !strings.Contains(out.String(), "#1") {
		t.Fatalf("restore:\n%s\n%s", msg, out)
	}
}

func TestRestoreReportSaysWhenTheOldMachineWontSeeANote(t *testing.T) {
	var b strings.Builder
	printRestore(&b, backup.Report{Manifest: backup.Manifest{HostLabel: "Old Mac"}, HandoverNote: "Mirrin couldn't leave it a note in /Volumes/NAS, where it looks for one: the backup folder /Volumes/NAS isn't there (is its disk or network drive connected?)"})
	if !strings.Contains(b.String(), "Quit Mirrin on Old Mac yourself, so nothing answers twice. Mirrin couldn't leave it a note in /Volumes/NAS") {
		t.Fatalf("got:\n%s", b.String())
	}
	b.Reset()
	printRestore(&b, backup.Report{Manifest: backup.Manifest{HostLabel: "This Mac"}, SameMachine: true, HandoverNote: "it came from this machine"})
	if strings.Contains(b.String(), "Quit Mirrin") {
		t.Fatalf("restoring on the same machine asked to quit it:\n%s", b.String())
	}
	if got := leftOutText([]string{"protocols/Meeting 10:30.yaml"}); !strings.Contains(got, "Left out protocols/Meeting 10:30.yaml") || !strings.Contains(got, `without any of : \ ? * < > | "`) {
		t.Fatalf("left out: %s", got)
	}
}

// The device review after a restore says what it means under per-device
// keys: every device listed still reaches the twin with its own key, so an
// unknown one is cut off now, and that works with the twin stopped.
func TestRestoreReportReviewsDevicesHonestly(t *testing.T) {
	var b strings.Builder
	printRestore(&b, backup.Report{Manifest: backup.Manifest{HostLabel: "Old Mac"}, SameMachine: true, Devices: []string{"Akshay's iPhone (0123abcd)"}})
	got := b.String()
	for _, want := range []string{"Akshay's iPhone (0123abcd)", "keeps its own key", "`mirrin devices revoke <id>`", "with the twin stopped"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "once your twin is running") || strings.Contains(got, "pair again") {
		t.Errorf("says devices lose their keys, or that revoking waits for the twin:\n%s", got)
	}
}
