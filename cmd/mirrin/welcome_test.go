package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
)

func TestWelcomeRestoreWP07Fixture(t *testing.T) {
	twinHome(t)
	dir := t.TempDir()
	ctx := context.Background()
	var out, msg bytes.Buffer
	term, _, _ := testTerminal(&kitReader{out: &out, t: t})
	term.out = &out
	term.msg = &msg
	if err := runBackup(ctx, term, []string{"init", "--folder", dir}); err != nil {
		t.Fatal(err)
	}
	phrase := wordsFromKit(t, out.String())
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	cfg.API.Listen = ""
	cfg.Name = "Before restore"
	cfg.Backup.Target = "folder"
	cfg.Backup.Path = dir
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	request := api.WelcomeRestoreRequest{Kind: "folder", Path: dir, Phrase: strings.Join(phrase.Words(), " "), Confirm: true}
	target, p, err := daemon.WelcomeRestoreTarget(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := backup.List(ctx, target, p)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots: %d %v", len(snapshots), err)
	}
	// The same verified target is accepted before the tray hands off.
	d, err := daemon.New(cfg, daemon.Options{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	handed := false
	d.SetWelcomeRestore(func(got api.WelcomeRestoreRequest) error {
		handed = got.Path == dir && got.Phrase == request.Phrase
		return nil
	})
	bad := request
	bad.Phrase = "not recovery words"
	if _, err := d.RestoreWelcome(ctx, bad); err == nil || handed {
		d.Close()
		t.Fatal("invalid words handed off")
	}
	if _, err := d.RestoreWelcome(ctx, request); err != nil {
		d.Close()
		t.Fatal(err)
	}
	if !handed {
		t.Fatal("restore wasn't handed to the worker")
	}
	sources := d.RestoreSources(ctx)
	if len(sources) == 0 || sources[0].Kind != "folder" || sources[0].Path != dir {
		t.Fatal(sources)
	}
	d.Close()
	report, err := restoreWelcomeHome(ctx, cfg, request)
	if err != nil {
		t.Fatal(err)
	}
	if report.Manifest.Twin != "Jeeves" || report.Aside == "" {
		t.Fatalf("report: %+v", report)
	}
	restored, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if restored.Name != "Jeeves" || !strings.HasPrefix(restored.DataDir, config.Home()) {
		t.Fatal("restored config not relocated")
	}
	if token, err := os.ReadFile(filepath.Join(restored.DataDir, "api.token")); err != nil || len(strings.TrimSpace(string(token))) < 32 {
		t.Fatal("missing fresh API token", err)
	}
	if _, err := os.Stat(filepath.Join(report.Aside, "config.yaml")); err != nil {
		t.Fatal("old twin wasn't kept", err)
	}
}
func TestWelcomeRestoreWaitsForHome(t *testing.T) {
	cfg := twinHome(t)
	release, err := daemon.LockHome(context.Background(), cfg.DataDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	p, err := backup.NewPhrase()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = restoreWelcomeHome(ctx, cfg, api.WelcomeRestoreRequest{Kind: "folder", Path: t.TempDir(), Phrase: strings.Join(p.Words(), " "), Confirm: true})
	if err == nil {
		t.Fatal("restored while home was in use")
	}
	got, err := config.Load()
	if err != nil || got.Name != cfg.Name {
		t.Fatal("changed occupied home", err)
	}
}

func TestWelcomeRestoreRejectsFutureBackupBeforeHandoff(t *testing.T) {
	cfg := twinHome(t)
	p, err := backup.NewPhrase()
	if err != nil {
		t.Fatal(err)
	}
	target := backup.Folder(t.TempDir())
	engine := &backup.Engine{
		Layout: backup.LayoutOf(cfg, config.Home()), Target: target,
		Settings: config.Backup{Recipient: p.Recipient()},
		Now:      func() time.Time { return time.Now().Add(72 * time.Hour) },
	}
	if _, err := engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, err := daemon.New(cfg, daemon.Options{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	handed := false
	d.SetWelcomeRestore(func(api.WelcomeRestoreRequest) error { handed = true; return nil })
	_, err = d.RestoreWelcome(context.Background(), api.WelcomeRestoreRequest{
		Kind: "folder", Path: target.String(), Phrase: strings.Join(p.Words(), " "), Confirm: true,
	})
	if err == nil || handed {
		t.Fatal("future backup quit the tray before restore could reject it")
	}
}
func TestWelcomeWorkerRejectsUnconfirmedInput(t *testing.T) {
	home(t)
	if got := welcomeRestoreWorker(strings.NewReader(`{"Confirm":false}`)); got != 2 {
		t.Fatal(got)
	}
}

func TestWelcomeRestoreRefusesManagedTray(t *testing.T) {
	home(t)
	for _, env := range []string{config.ServiceEnv, "XPC_SERVICE_NAME"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(config.ServiceEnv, "")
			t.Setenv("XPC_SERVICE_NAME", "")
			t.Setenv(env, "mirrin")
			err := startWelcomeRestore(api.WelcomeRestoreRequest{Confirm: true})
			var human *api.HumanError
			if !errors.As(err, &human) || !strings.Contains(human.Fix, "mirrin service stop") {
				t.Fatalf("managed tray restore: %v", err)
			}
		})
	}
}
