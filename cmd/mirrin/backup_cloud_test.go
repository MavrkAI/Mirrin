package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/cloud/cloudtest"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// trustDev makes the CLI trust the dev control plane's keys.
func trustDev(t *testing.T, d *cloudtest.DevServer) {
	prev := cloudKeys
	cloudKeys = func() map[string]ed25519.PublicKey { return d.Ent }
	t.Cleanup(func() { cloudKeys = prev })
}

// The paid backup journey on the command line, against `mirrin-cloud serve
// --dev` and its disk storage: backups move to Cloud with the words,
// migrate between Cloud and folders byte for byte, and come back on a new
// machine with the words alone.
func TestBackupToCloudMigrateAndRestore(t *testing.T) {
	d := cloudtest.StartDev(t, "")
	trustDev(t, d)
	cfg := twinHome(t)
	ctx := context.Background()
	old := d.Link(t, cfg.DataDir)

	var out, msg bytes.Buffer
	term := &terminal{in: bufio.NewReader(&kitReader{out: &out, t: t}), out: &out, msg: &msg}
	dirA := t.TempDir()
	if err := runBackup(ctx, term, []string{"init", "--folder", dirA}); err != nil {
		t.Fatalf("%v\n%s%s", err, out.String(), msg.String())
	}
	p := wordsFromKit(t, out.String())
	words := strings.Join(p.Words(), " ") + "\n"

	// Other words are refused before anything is sent.
	other, _ := backup.NewPhrase()
	term, _, _ = testTerminal(strings.NewReader(strings.Join(other.Words(), " ") + "\n"))
	if err := runBackup(ctx, term, []string{"target", "cloud"}); err == nil || !strings.Contains(err.Error(), "not the ones for this twin") {
		t.Fatalf("target cloud with other words: %v", err)
	}
	term, o, m := testTerminal(strings.NewReader(words))
	if err := runBackup(ctx, term, []string{"target", "cloud"}); err != nil || !strings.Contains(o.String(), "Backups now go to Mirrin Cloud") {
		t.Fatalf("target cloud: %v\n%s%s", err, o, m)
	}
	term, o, _ = testTerminal(strings.NewReader(""))
	if err := runBackup(ctx, term, []string{"now"}); err != nil || !strings.Contains(o.String(), "to Mirrin Cloud") {
		t.Fatalf("now: %v\n%s", err, o)
	}
	term, o, _ = testTerminal(strings.NewReader(""))
	if err := runBackup(ctx, term, []string{"status"}); err != nil || !strings.Contains(o.String(), "Backups: on, to Mirrin Cloud") {
		t.Fatalf("status: %v\n%s", err, o)
	}

	// The folder's backup joins it in Cloud, then everything goes to
	// another folder, byte for byte as the storage holds it.
	term, o, m = testTerminal(strings.NewReader(""))
	if err := runBackup(ctx, term, []string{"migrate", "--from", "folder", dirA, "--to", "cloud"}); err != nil || !strings.Contains(o.String(), "Copied 1 backup") {
		t.Fatalf("migrate to cloud: %v\n%s%s", err, o, m)
	}
	dirB := t.TempDir()
	term, o, m = testTerminal(strings.NewReader(""))
	if err := runBackup(ctx, term, []string{"migrate", "--from", "cloud", "--to", "folder", dirB}); err != nil || !strings.Contains(o.String(), "Copied 2 backups") {
		t.Fatalf("migrate to a folder: %v\n%s%s", err, o, m)
	}
	if !strings.Contains(o.String(), "New backups still go to Mirrin Cloud") {
		t.Errorf("migrate didn't say where backups go: %s", o)
	}
	stored, _ := filepath.Glob(filepath.Join(d.DataDir, "storage", "ns", p.Namespace(), "*.age"))
	if len(stored) != 2 {
		t.Fatalf("storage holds %v", stored)
	}
	for _, s := range stored {
		want, _ := os.ReadFile(s)
		got, err := os.ReadFile(filepath.Join(dirB, p.Namespace(), filepath.Base(s)))
		if err != nil || !bytes.Equal(got, want) || len(got) == 0 {
			t.Fatalf("%s: not copied byte for byte (%v)", filepath.Base(s), err)
		}
	}

	// A new machine, with the words alone.
	t.Setenv("MIRRIN_HOME", t.TempDir())
	term, o, m = testTerminal(strings.NewReader(words))
	if err := runRestore(ctx, term, []string{"--from", "cloud", "--api", d.Origin, "--yes"}); err != nil {
		t.Fatalf("restore: %v\n%s%s", err, o, m)
	}
	if !strings.Contains(o.String(), "Restored Jeeves") || !strings.Contains(m.String(), "moved") {
		t.Fatalf("restore said:\n%s\n%s", o, m)
	}
	restored, err := config.Load()
	if restored == nil {
		t.Fatal(err)
	}
	if restored.Backup.Target != backup.TargetCloud {
		t.Fatalf("restored target %q", restored.Backup.Target)
	}
	if _, err := os.Stat(filepath.Join(restored.DataDir, "cloud", "device.key")); err != nil {
		t.Fatalf("the recovered link didn't move in: %v", err)
	}
	if _, err := os.Stat(cloudRestoreDir(config.Home())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the link's folder stayed: %v", err)
	}
	// The restored twin backs up to Cloud as the account's machine now…
	term, o, _ = testTerminal(strings.NewReader(""))
	if err := runBackup(ctx, term, []string{"now"}); err != nil || !strings.Contains(o.String(), "to Mirrin Cloud") {
		t.Fatalf("now after restore: %v\n%s", err, o)
	}
	// …and the old one stands by.
	var sup *cloud.SupersededError
	if _, err := old.Refresh(ctx); !errors.As(err, &sup) {
		t.Fatalf("the old machine's refresh: %v", err)
	}
}

// init --cloud needs a linked machine before any words are made, then binds
// the new words and takes the first backup there.
func TestBackupInitCloud(t *testing.T) {
	d := cloudtest.StartDev(t, "")
	trustDev(t, d)
	cfg := twinHome(t)
	ctx := context.Background()
	term, o, _ := testTerminal(strings.NewReader(""))
	if err := runBackup(ctx, term, []string{"init", "--cloud"}); err == nil || !strings.Contains(err.Error(), "mirrin cloud link") || strings.Contains(o.String(), "Recovery Kit") {
		t.Fatalf("init --cloud unlinked: %v\n%s", err, o)
	}
	d.Link(t, cfg.DataDir)
	var out, msg bytes.Buffer
	term = &terminal{in: bufio.NewReader(&kitReader{out: &out, t: t}), out: &out, msg: &msg}
	if err := runBackup(ctx, term, []string{"init", "--cloud"}); err != nil {
		t.Fatalf("%v\n%s%s", err, out.String(), msg.String())
	}
	if !strings.Contains(out.String(), "Backups are on. They go to Mirrin Cloud") || !strings.Contains(out.String(), "to Mirrin Cloud.") {
		t.Fatalf("init --cloud said:\n%s", out.String())
	}
}
