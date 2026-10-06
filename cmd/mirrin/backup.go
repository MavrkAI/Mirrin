package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
)

func init() { commands = append(commands, "backup", "restore") }

const backupUsage = `usage: mirrin backup <command>
  init [--folder <path> | --icloud | --s3 s3://<bucket>/<folder> | --cloud] [--new] [--with-sessions]
                                                Turn on encrypted backups and get your 12 words
  now [--with-sessions]                         Back up now (--with-sessions: the WhatsApp link too)
  status                                        When the last backup was, and where they go
  list                                          The backups that are there
  verify [snapshot]                             Check a backup opens with your words and is intact
  key --age                                     Print the age key your words make (for age -d)
  target folder <path> | icloud | s3 s3://<bucket>/<folder> | cloud
                                                Change where backups go
  migrate [--from <place>] --to <place>         Copy every backup to another place, byte for byte
  prune                                         Remove old backups (keeps 7 daily, 4 weekly, 6 monthly)
  resume                                        Make this machine the twin again after a move
` + s3OptionsUsage

const restoreUsage = `usage: mirrin restore [--from <folder> | --from icloud | --from s3://<bucket>/<folder> | --from cloud] [--snapshot <name>] [--with-sessions] [--force] [--yes]
` + s3OptionsUsage

// terminal is where the backup commands read and write.
type terminal struct {
	in  *bufio.Reader
	out io.Writer // results
	msg io.Writer // prompts and notes, so `key --age > key.txt` stays clean
	tty bool
}

func stdTerminal() *terminal {
	return &terminal{in: bufio.NewReader(os.Stdin), out: os.Stdout, msg: os.Stderr, tty: term.IsTerminal(int(os.Stdin.Fd()))}
}

func (t *terminal) ask(prompt string) (string, error) {
	fmt.Fprint(t.msg, prompt)
	line, err := t.in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", errors.New("no answer was typed")
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// readPhrase asks for the 12 words. They can be typed on one line or
// pasted from the kit over several; a typo is pointed out, with a guess.
func (t *terminal) readPhrase() (backup.Phrase, error) {
	for try := 0; ; try++ {
		fmt.Fprintln(t.msg, "Type your 12 words from your Recovery Kit, then press Enter:")
		var words []string
		for {
			line, err := t.in.ReadString('\n')
			words = append(words, strings.Fields(line)...)
			if err != nil || countWords(words) >= backup.PhraseWords || strings.TrimSpace(line) == "" {
				break
			}
		}
		p, err := backup.ParsePhrase(strings.Join(words, " "))
		if err == nil {
			return p, nil
		}
		if !t.tty || try == 2 {
			return p, err
		}
		fmt.Fprintln(t.msg, sentence(err.Error()))
		fmt.Fprintln(t.msg)
	}
}

// sentence capitalises s and ends it with a full stop unless it has one.
func sentence(s string) string {
	s = capitalize(s)
	if s != "" && !strings.ContainsAny(s[len(s)-1:], ".?!") {
		s += "."
	}
	return s
}

// countWords counts typed words, leaving out the kit's numbers.
func countWords(tokens []string) int {
	n := 0
	for _, tok := range tokens {
		if strings.Trim(tok, "0123456789.):") != "" {
			n++
		}
	}
	return n
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func backupCmd(ctx context.Context, args []string) error {
	return runBackup(ctx, stdTerminal(), args)
}

func restoreCmd(ctx context.Context, args []string) error {
	return runRestore(ctx, stdTerminal(), args)
}

func runBackup(ctx context.Context, t *terminal, args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return backupInit(ctx, t, rest)
	case "now":
		return backupNow(ctx, t, rest)
	case "status":
		return backupStatus(t)
	case "list":
		return backupList(ctx, t)
	case "verify":
		return backupVerify(ctx, t, rest)
	case "key":
		if len(rest) != 1 || rest[0] != "--age" {
			return errors.New("usage: mirrin backup key --age")
		}
		return backupKey(t)
	case "target":
		if len(rest) == 1 && rest[0] == backup.TargetCloud {
			return backupTargetCloud(ctx, t) // backup_cloud.go
		}
		return backupTarget(t, rest)
	case "migrate":
		return backupMigrate(ctx, t, rest) // backup_cloud.go
	case "prune":
		return backupPrune(ctx, t)
	case "resume":
		return backupResume(t)
	case "help", "-h", "--help":
		fmt.Fprintln(t.out, backupUsage)
		return nil
	}
	return fmt.Errorf("mirrin backup %s isn't a command\n%s", sub, backupUsage)
}

// loadForBackup loads the config, which must exist.
func loadForBackup() (*config.Config, error) {
	cfg, err := config.Load()
	if cfg == nil {
		if _, serr := os.Stat(config.Path()); errors.Is(serr, os.ErrNotExist) {
			return nil, errors.New("there's no twin here to back up yet; run `mirrin init` first")
		}
		return nil, err
	}
	return cfg, nil // a config that loads but doesn't validate still backs up
}

// targetFlag reads --folder <path> or --icloud.
func targetFlag(args []string, i int) (config.Backup, int, error) {
	switch args[i] {
	case "--icloud":
		return config.Backup{Target: backup.TargetICloud}, i, nil
	case "--folder":
		if i+1 >= len(args) {
			return config.Backup{}, i, errors.New("--folder needs a folder: mirrin backup init --folder /Volumes/Backup")
		}
		p, err := filepath.Abs(expandTilde(args[i+1]))
		return config.Backup{Target: backup.TargetFolder, Path: p}, i + 1, err
	}
	return config.Backup{}, i, fmt.Errorf("unknown option %s\n%s", args[i], backupUsage)
}

func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

// defaultTarget is iCloud Drive on a Mac that has it.
func defaultTarget() (config.Backup, error) {
	if runtime.GOOS == "darwin" {
		if _, err := backup.ICloudDrivePath(); err != nil {
			return config.Backup{}, fmt.Errorf("%v\n(or choose a folder: mirrin backup init --folder <path>)", err)
		}
		return config.Backup{Target: backup.TargetICloud}, nil
	}
	return config.Backup{}, errors.New("tell me where to keep backups: mirrin backup init --folder <path> (a USB disk, a NAS or a synced folder)")
}

func backupInit(ctx context.Context, t *terminal, args []string) error {
	var where config.Backup
	var s3o s3Options
	fresh, sessions, s3url := false, false, ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--new":
			fresh = true
			continue
		case "--with-sessions":
			sessions = true
			continue
		case "--cloud":
			where = config.Backup{Target: backup.TargetCloud}
			continue
		case "--s3":
			if i+1 >= len(args) {
				return errors.New("--s3 needs a bucket: mirrin backup init --s3 s3://my-bucket/mirrin")
			}
			i++
			s3url, where = args[i], config.Backup{Target: backup.TargetS3}
			continue
		}
		if j, ok, err := s3o.take(args, i); ok {
			if err != nil {
				return err
			}
			i = j
			continue
		}
		w, j, err := targetFlag(args, i)
		if err != nil {
			return err
		}
		where, i = w, j
	}
	switch {
	case where.Target == backup.TargetS3:
		w, err := s3o.where(s3url)
		if err != nil {
			return err
		}
		where = w
	case s3o.set || s3url != "":
		return errors.New("choose one place for backups; --endpoint, --region and the other S3 options go with --s3")
	}
	cfg, err := loadForBackup()
	if err != nil {
		return err
	}
	if cfg.Backup.Recipient != "" && !fresh {
		fmt.Fprintf(t.out, "Backups are already on, to %s (Kit ID %s).\n", backup.Where(cfg.Backup), kitIDOf(cfg.Backup))
		fmt.Fprintln(t.out, "To get new words, run `mirrin backup init --new`.")
		return nil
	}
	if where.Target == "" {
		if cfg.Backup.Target != "" {
			where = config.Backup{Target: cfg.Backup.Target, Path: cfg.Backup.Path, S3: cfg.Backup.S3}
		} else if where, err = defaultTarget(); err != nil {
			return err
		}
	}
	where.Sessions = cfg.Backup.Sessions || sessions
	if err := backup.CheckWhere(config.Path(), cfg.DataDir, where); err != nil {
		return err // before the words: nobody writes down words for nothing
	}
	if err := prepareS3(ctx, t, where); err != nil {
		return err // likewise
	}
	var bind func(backup.Phrase) error
	if where.Target == backup.TargetCloud {
		if bind, err = bindForInit(ctx, cfg.DataDir); err != nil {
			return err // likewise: this machine must be linked first
		}
	}
	p, err := backup.NewPhrase()
	if err != nil {
		return err
	}
	fmt.Fprintln(t.out, backup.RecoveryKit(p, backup.KitInfo{Twin: cfg.Name, Where: backup.Where(where), Made: time.Now()}))
	fmt.Fprintln(t.out, "Write the 12 words down now. They won't be shown again.")
	ok := false
	for try := 0; try < 3 && !ok; try++ {
		typed, err := t.ask(fmt.Sprintf("To check, type word %d: ", backup.CheckWord))
		if err != nil {
			break
		}
		if ok = backup.CheckTyped(p, backup.CheckWord, typed); !ok {
			fmt.Fprintf(t.msg, "That isn't word %d. Look at the list above and try again.\n", backup.CheckWord)
		}
	}
	if !ok {
		return errors.New("backups weren't turned on, because the words weren't checked. Run `mirrin backup init` again when you have paper ready")
	}
	if bind != nil {
		if err := bind(p); err != nil {
			return fmt.Errorf("backups weren't turned on: %v", err)
		}
	}
	s, err := backup.Setup(config.Path(), cfg.DataDir, p, where, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(t.out, "Backups are on. They go to %s every night at 03:30 while Mirrin is running, encrypted with your words.\n", backup.Where(s))
	if old := cfg.Backup; fresh && old.Recipient != "" {
		if ns, err := backup.NamespaceOf(old.RecoveryPub); err == nil {
			fmt.Fprintf(t.out, "Your older backups stay in %s. They open only with your old words, and Mirrin doesn't prune them any more: delete that folder when you no longer need them, and keep the old kit until then.\n", backup.WhereNS(old, ns))
		}
	}
	fmt.Fprintln(t.out, "Taking the first backup now…")
	cfg.Backup = s
	return runEngine(ctx, t, cfg)
}

func runEngine(ctx context.Context, t *terminal, cfg *config.Config) error {
	e, err := backup.NewEngine(cfg, config.Home(), version)
	if err != nil {
		return err
	}
	e.Log = slog.New(slog.DiscardHandler) // what matters is said below, in plain words
	m, err := e.Run(ctx)
	if err != nil {
		if errors.Is(err, backup.ErrStandingBy) || errors.Is(err, backup.ErrBusy) {
			return err
		}
		return fmt.Errorf("the backup didn't finish: %v\nIt tries again at 03:30 while Mirrin is running, or run `mirrin backup now` once that's fixed", err)
	}
	st, _ := backup.LoadState(e.Layout.DataDir)
	fmt.Fprintf(t.out, "Backed up %s: %d files, %s, to %s.\n", nameOr(m.Twin), len(m.Contents), sizeText(st.LastSize), e.Target)
	if len(st.LeftOut) > 0 {
		fmt.Fprintln(t.out, leftOutText(st.LeftOut))
	}
	return nil
}

// leftOutText names the files a backup left out because their names
// can't be restored on every computer, and says what to do.
func leftOutText(left []string) string {
	names := make([]string, len(left))
	for i, n := range left {
		names[i] = clean(n)
	}
	if len(names) == 1 {
		return "Left out " + names[0] + ": its name can't be restored on every computer. Rename it (without any of " + backup.AwkwardChars + ") and the next backup takes it."
	}
	return fmt.Sprintf("Left out %d files whose names can't be restored on every computer: %s. Rename them (without any of "+backup.AwkwardChars+") and the next backup takes them.", len(names), strings.Join(names, ", "))
}

func nameOr(twin string) string {
	if twin == "" {
		return "your twin"
	}
	return clean(twin)
}

func backupNow(ctx context.Context, t *terminal, args []string) error {
	sessions := false
	for _, a := range args {
		if a != "--with-sessions" {
			return fmt.Errorf("unknown option %s\n%s", a, backupUsage)
		}
		sessions = true
	}
	cfg, err := loadForBackup()
	if err != nil {
		return err
	}
	cfg.Backup.Sessions = cfg.Backup.Sessions || sessions
	fmt.Fprintln(t.msg, "Backing up…")
	return runEngine(ctx, t, cfg)
}

func kitIDOf(s config.Backup) string {
	ns, err := backup.NamespaceOf(s.RecoveryPub)
	if err != nil {
		return "unknown"
	}
	return backup.KitIDFor(ns)
}

func backupStatus(t *terminal) error {
	s, err := backup.LoadSettings(config.Path())
	if err != nil {
		return err
	}
	cfg, _ := loadForBackup()
	dataDir := filepath.Join(config.Home(), "data")
	if cfg != nil {
		dataDir = cfg.DataDir
	}
	st, _ := backup.LoadState(dataDir)
	if st.Standby != nil {
		fmt.Fprintf(t.out, "%s (since %s).\n", backup.StandbyMessage(st.Standby), st.Standby.At.Local().Format("2 Jan 15:04"))
		fmt.Fprintln(t.out, "To make this machine the twin again: mirrin backup resume")
		return nil
	}
	if s.Recipient == "" {
		fmt.Fprintln(t.out, "Backups are off.")
		fmt.Fprintln(t.out, "Turn them on with `mirrin backup init`: encrypted with 12 words only you have, to iCloud Drive, a folder you choose or an S3 bucket.")
		return nil
	}
	fmt.Fprintf(t.out, "Backups: on, to %s\n", backup.Where(s))
	fmt.Fprintf(t.out, "Kit ID:  %s\n", kitIDOf(s))
	if !st.LastGood.IsZero() {
		fmt.Fprintf(t.out, "Last:    %s (#%d, %s)\n", st.LastGood.Local().Format("Mon 2 Jan 15:04"), st.Seq, sizeText(st.LastSize))
	} else {
		fmt.Fprintln(t.out, "Last:    none yet")
	}
	if st.LastError != "" && st.LastAttempt.After(st.LastGood) {
		fmt.Fprintf(t.out, "Problem: %s\n", st.LastError)
	}
	if len(st.LeftOut) > 0 {
		fmt.Fprintf(t.out, "Note:    %s\n", leftOutText(st.LeftOut))
	}
	if twinRunning() {
		fmt.Fprintln(t.out, "Next:    tonight at 03:30 (or `mirrin backup now`)")
	} else {
		fmt.Fprintln(t.out, "Next:    at 03:30 once Mirrin is running; it isn't now. Open Mirrin, or run `mirrin service install` to keep it running (`mirrin backup now` backs up at once)")
	}
	return nil
}

// openConfigured opens the configured target.
func openConfigured() (config.Backup, backup.Target, error) {
	s, err := backup.LoadSettings(config.Path())
	if err != nil {
		return s, nil, err
	}
	if s.Recipient == "" {
		return s, nil, errors.New("backups aren't set up here yet; run `mirrin backup init` (or `mirrin restore` to bring a twin back)")
	}
	ns, err := backup.NamespaceOf(s.RecoveryPub)
	if err != nil {
		return s, nil, err
	}
	tg, err := backup.OpenTarget(s, ns)
	return s, tg, err
}

func backupList(ctx context.Context, t *terminal) error {
	_, tg, err := openConfigured()
	if err != nil {
		return err
	}
	objs, err := tg.List(ctx)
	if err != nil {
		return err
	}
	n := 0
	for i := len(objs) - 1; i >= 0; i-- {
		if !backup.IsSnapshot(objs[i].Name) {
			continue
		}
		n++
		fmt.Fprintf(t.out, "  %s  %8s  %s\n", nameTime(objs[i].Name), listedSize(objs[i].Size), objs[i].Name)
	}
	if n == 0 {
		fmt.Fprintf(t.out, "No backups in %s yet. Run `mirrin backup now`.\n", tg)
		return nil
	}
	fmt.Fprintf(t.out, "%d in %s. Times come from the file names; `mirrin backup verify` checks one with your words.\n", n, tg)
	return nil
}

// nameTime is the time in a snapshot's name, in local time.
func nameTime(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return name
	}
	at, err := time.Parse("20060102T150405Z", parts[1])
	if err != nil {
		return name
	}
	return at.Local().Format("Mon 2 Jan 2006 15:04")
}

// listedSize is a listed snapshot's size; 0 means it isn't known here (an
// evicted iCloud Drive file).
func listedSize(n int64) string {
	if n == 0 {
		return "in iCloud"
	}
	return sizeText(n)
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// wordsFor reads the words and checks they are this twin's.
func wordsFor(t *terminal, s config.Backup) (backup.Phrase, error) {
	p, err := t.readPhrase()
	if err != nil {
		return p, err
	}
	if s.Recipient != "" && !p.Opens(s.Recipient) {
		return p, fmt.Errorf("those words are real, but they're not the ones for this twin's backups (their Kit ID is %s; this twin's is %s)", backup.KitID(p), kitIDOf(s))
	}
	return p, nil
}

func backupVerify(ctx context.Context, t *terminal, args []string) error {
	s, tg, err := openConfigured()
	if err != nil {
		return err
	}
	p, err := wordsFor(t, s)
	if err != nil {
		return err
	}
	name := ""
	if len(args) > 0 {
		name = args[0]
	} else {
		ls, err := backup.List(ctx, tg, p)
		if err != nil {
			return err
		}
		if len(ls) == 0 {
			return fmt.Errorf("no backups in %s yet; run `mirrin backup now`", tg)
		}
		name = ls[0].Name
	}
	fmt.Fprintf(t.msg, "Checking %s…\n", name)
	m, err := backup.Verify(ctx, tg, name, p)
	if err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	fmt.Fprintf(t.out, "%s is intact: #%d, taken %s on %s, %d files, every checksum matches.\n",
		name, m.Seq, m.ExportedAt.Local().Format("Mon 2 Jan 2006 15:04"), clean(m.HostLabel), len(m.Contents))
	return nil
}

func backupKey(t *terminal) error {
	s, _ := backup.LoadSettings(config.Path())
	p, err := wordsFor(t, s)
	if err != nil {
		return err
	}
	id, err := p.AgeIdentity()
	if err != nil {
		return err
	}
	if s.Recipient != "" && !strings.HasPrefix(s.Recipient, "age1pq1") {
		x, err := p.X25519Identity()
		if err != nil {
			return err
		}
		fmt.Fprintln(t.out, x.String())
	} else {
		fmt.Fprintln(t.out, id.String())
	}
	fmt.Fprintln(t.msg, "That key opens every backup: keep it as safe as the words. To open one with age 1.3 or newer:")
	fmt.Fprintln(t.msg, "  mirrin backup key --age > key.txt && age -d -i key.txt snap-….age | tar -xz")
	return nil
}

func backupTarget(t *terminal, args []string) error {
	var where config.Backup
	switch {
	case len(args) == 1 && args[0] == "icloud":
		where = config.Backup{Target: backup.TargetICloud}
	case len(args) == 2 && args[0] == "folder":
		p, err := filepath.Abs(expandTilde(args[1]))
		if err != nil {
			return err
		}
		where = config.Backup{Target: backup.TargetFolder, Path: p}
	case len(args) >= 1 && args[0] == "s3":
		w, err := s3TargetArgs(args[1:])
		if err != nil {
			return err
		}
		where = w
	default:
		return errors.New("usage: mirrin backup target folder <path> | icloud | s3 s3://<bucket>/<folder> [S3 options] | cloud")
	}
	if _, err := os.Stat(config.Path()); err != nil {
		return errors.New("there's no twin here yet; run `mirrin init` first")
	}
	if err := s3Keys(t, where, true); err != nil {
		return err
	}
	s, err := backup.SetTarget(config.Path(), where)
	if err != nil {
		return err
	}
	fmt.Fprintf(t.out, "Backups now go to %s.", backup.Where(s))
	if s.Recipient == "" {
		fmt.Fprint(t.out, " Turn them on with `mirrin backup init`.")
	} else {
		fmt.Fprint(t.out, " The ones already made stay where they are.")
	}
	fmt.Fprintln(t.out)
	return nil
}

func backupPrune(ctx context.Context, t *terminal) error {
	cfg, err := loadForBackup()
	if err != nil {
		return err
	}
	e, err := backup.NewEngine(cfg, config.Home(), version)
	if err != nil {
		return err
	}
	gone, err := e.Prune(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(t.out, "Removed %s from %s (keeping 7 daily, 4 weekly and 6 monthly).\n", plural(len(gone), "old backup"), e.Target)
	return nil
}

func backupResume(t *terminal) error {
	cfg, err := loadForBackup()
	if err != nil {
		return err
	}
	was, err := backup.Resume(cfg.DataDir)
	if err != nil {
		return err
	}
	if was == nil {
		fmt.Fprintln(t.out, "This machine isn't standing by; nothing to do.")
		return nil
	}
	host := clean(was.HostLabel)
	if host == "" {
		host = "the other machine"
	}
	fmt.Fprintf(t.out, "This machine is the twin again. Quit Mirrin on %s first, or both will answer.\n", host)
	fmt.Fprintln(t.out, "Then restart the twin here: choose Restart in the menu bar, or run `mirrin service restart`.")
	return nil
}

// restoreFlags are `mirrin restore`'s options.
type restoreFlags struct {
	from, snapshot           string
	api                      string // the paid service's control plane, for --from cloud
	withSessions, force, yes bool
	s3                       s3Options
}

func parseRestore(args []string) (restoreFlags, error) {
	var f restoreFlags
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--with-sessions":
			f.withSessions = true
		case "--force":
			f.force = true
		case "--yes", "-y":
			f.yes = true
		case "--from", "--snapshot", "--api":
			if i+1 >= len(args) {
				return f, fmt.Errorf("%s needs a value\n%s", a, restoreUsage)
			}
			i++
			switch a {
			case "--from":
				f.from = args[i]
			case "--api":
				f.api = args[i]
			default:
				f.snapshot = args[i]
			}
		default:
			j, ok, err := f.s3.take(args, i)
			if err != nil {
				return f, err
			}
			if !ok {
				return f, fmt.Errorf("unknown option %s\n%s", a, restoreUsage)
			}
			i = j
		}
	}
	if f.s3.set && !strings.HasPrefix(f.from, "s3://") {
		return f, errors.New("--endpoint, --region and the other S3 options go with --from s3://<bucket>/<folder>")
	}
	if f.api != "" && f.from != backup.TargetCloud {
		return f, errors.New("--api goes with --from cloud")
	}
	return f, nil
}

// restoreBase is the folder backups are looked for in.
func restoreBase(f restoreFlags) (config.Backup, error) {
	switch {
	case f.from == "icloud":
		return config.Backup{Target: backup.TargetICloud}, nil
	case f.from == backup.TargetCloud:
		return config.Backup{Target: backup.TargetCloud}, nil
	case strings.HasPrefix(f.from, "s3://"):
		return restoreS3(f.from, f.s3)
	case f.from != "":
		p, err := filepath.Abs(expandTilde(f.from))
		return config.Backup{Target: backup.TargetFolder, Path: p}, err
	}
	if s, err := backup.LoadSettings(config.Path()); err == nil && s.Target != "" {
		return config.Backup{Target: s.Target, Path: s.Path, S3: s.S3}, nil
	}
	if runtime.GOOS == "darwin" {
		return config.Backup{Target: backup.TargetICloud}, nil
	}
	return config.Backup{}, errors.New("where are the backups? mirrin restore --from <folder>")
}

// restoreTarget finds the backups these words made: in their own folder
// under base, or in base itself when that folder was given directly.
func restoreTarget(ctx context.Context, where config.Backup, p backup.Phrase) (backup.Target, error) {
	tg, err := backup.OpenTarget(where, p.Namespace())
	if err != nil {
		return nil, err
	}
	if objs, _ := tg.List(ctx); len(objs) > 0 || where.Target != backup.TargetFolder {
		return tg, nil
	}
	direct := backup.Folder(where.Path)
	if objs, _ := direct.List(ctx); len(objs) > 0 {
		return direct, nil
	}
	return tg, nil
}

func runRestore(ctx context.Context, t *terminal, args []string) error {
	f, err := parseRestore(args)
	if err != nil {
		return err
	}
	if !f.force && twinRunning() {
		return backup.ErrRunning
	}
	where, err := restoreBase(f)
	if err != nil {
		return err
	}
	if err := s3Keys(t, where, false); err != nil {
		return err
	}
	p, err := t.readPhrase()
	if err != nil {
		return err
	}
	var tg backup.Target
	var settle func(string) error
	if where.Target == backup.TargetCloud {
		// The paid service: the account moves here first (backup_cloud.go).
		cr, err := recoverForRestore(ctx, t, f, p)
		if err != nil {
			return err
		}
		tg, settle = cr.target, cr.settle
	} else if tg, err = restoreTarget(ctx, where, p); err != nil {
		return err
	}
	ls, err := backup.List(ctx, tg, p)
	if err != nil {
		return err
	}
	var pick *backup.Listed
	var passed []*backup.Listed
	shown := 0
	for i := range ls {
		l := &ls[i]
		if l.Err != nil {
			continue
		}
		switch {
		case f.snapshot != "":
			if f.snapshot == l.Name {
				pick = l
			}
		case pick == nil && backup.Future(l.Manifest, time.Now()):
			passed = append(passed, l) // dated after today: only when named
		case pick == nil:
			pick = l
		}
		if shown < 5 {
			if shown == 0 {
				fmt.Fprintf(t.msg, "Backups in %s:\n", tg)
			}
			fmt.Fprintf(t.msg, "  %s  #%-5d %-24s %8s  %s\n", l.Manifest.ExportedAt.Local().Format("Mon 2 Jan 2006 15:04"), l.Manifest.Seq, clean(l.Manifest.HostLabel), sizeText(l.Size), l.Name)
			shown++
		}
	}
	for _, l := range passed {
		fmt.Fprintf(t.msg, "Passed over %s: it says it was made on %s, which is after today. Restore it with --snapshot %s only if you know where it came from.\n",
			l.Name, l.Manifest.ExportedAt.Local().Format("2 Jan 2006"), l.Name)
	}
	if pick == nil {
		if f.snapshot != "" {
			return fmt.Errorf("%s isn't a backup these words open in %s", f.snapshot, tg)
		}
		_, err := backup.Newest(ls, tg)
		return err // says why: wrong words, damaged, or none there
	}
	home := config.Home()
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); err == nil && !f.yes && settle == nil {
		if !t.tty {
			return errors.New("there's a twin here already; add --yes to replace it (it's kept aside, not deleted)")
		}
		a, err := t.ask(fmt.Sprintf("Restore #%d from %s? The twin here now is kept aside, not deleted. [y/N] ", pick.Manifest.Seq, pick.Manifest.ExportedAt.Local().Format("Mon 2 Jan 15:04")))
		if err != nil || !strings.HasPrefix(strings.ToLower(a), "y") {
			return errors.New("nothing was changed")
		}
	}
	r, err := backup.Restore(ctx, backup.RestoreOptions{
		Target: tg, Name: pick.Name, Phrase: p, Home: home, Force: f.force, Running: twinRunning,
		WithSessions: f.withSessions, HostLabel: backup.HostLabel(), Settle: settle,
	})
	if err != nil {
		return err
	}
	printRestore(t.out, r)
	return nil
}

func printRestore(w io.Writer, r backup.Report) {
	m := r.Manifest
	fmt.Fprintf(w, "Restored %s from %s (#%d, %s).\n", nameOr(m.Twin), m.ExportedAt.Local().Format("Mon 2 Jan 2006 15:04"), m.Seq, clean(m.HostLabel))
	if r.Stale != "" {
		fmt.Fprintln(w, r.Stale)
	}
	if r.Aside != "" {
		fmt.Fprintf(w, "What was here before is kept in %s.\n", r.Aside)
	}
	if len(r.Kept) > 0 {
		fmt.Fprintf(w, "Brought over from it, since they belong to this machine: %s.\n", strings.Join(r.Kept, ", "))
	}
	for _, mv := range r.Moved {
		fmt.Fprintln(w, "Moved: "+clean(mv))
	}
	if len(r.Renamed) > 0 {
		fmt.Fprintln(w, "Restored under new names, because this computer doesn't allow "+backup.AwkwardChars+" in a name:")
		for _, n := range r.Renamed {
			fmt.Fprintln(w, "  - "+clean(n))
		}
	}
	if r.Signal != "" {
		fmt.Fprintf(w, "Signal's session is back in %s.\n", r.Signal)
	}
	switch {
	case r.Handover != "":
		fmt.Fprintf(w, "The twin on %s will stand by within the hour. To be sure nothing answers twice, quit Mirrin there now.\n", nameOrHost(m.HostLabel))
	case !r.SameMachine && r.HandoverNote != "":
		fmt.Fprintf(w, "Quit Mirrin on %s yourself, so nothing answers twice. %s\n", nameOrHost(m.HostLabel), sentence(clean(r.HandoverNote)))
	}
	if len(r.Devices) > 0 {
		fmt.Fprintln(w, "Devices that could reach your twin when this backup was made:")
		for _, d := range r.Devices {
			fmt.Fprintln(w, "  - "+clean(d))
		}
		fmt.Fprintln(w, "Each keeps its own key and can still reach your twin. If you don't recognise one, cut it off now: `mirrin devices revoke <id>` (it works with the twin stopped).")
	}
	fmt.Fprintln(w, "This computer's menu and terminal have a new token (paired devices keep their own keys).")
	if len(r.Checklist) > 0 {
		fmt.Fprintln(w, "Still to do here:")
		for _, c := range r.Checklist {
			fmt.Fprintln(w, "  - "+clean(c))
		}
	}
	fmt.Fprintln(w, "Start your twin when you're ready: open Mirrin, or run `mirrin service start`.")
}

func nameOrHost(h string) string {
	if h = clean(h); h == "" {
		return "the old machine"
	}
	return h
}

// backupMain dispatches `mirrin backup` and `mirrin restore`.
func backupMain(ctx context.Context, cmd string, args []string) error {
	if cmd == "restore" {
		return restoreCmd(ctx, args)
	}
	return backupCmd(ctx, args)
}
