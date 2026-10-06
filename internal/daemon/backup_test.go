package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

func TestBackupCheckIsOffUntilSetUp(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	res := td.backupCheck().Run(context.Background())
	if res.State != health.Off || !strings.Contains(res.Fix, "mirrin backup init") {
		t.Fatalf("got %+v", res)
	}
	td.BackupSoon("pairing") // before the schedule starts: nothing to do, no panic
}

func TestAMachineStandingByStaysPausedWithChannelsOff(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	var shown []string
	prev := desktopNotify
	desktopNotify = func(_, body string) error { shown = append(shown, body); return nil }
	t.Cleanup(func() { desktopNotify = prev })
	h := backup.Handover{Name: "handover-20260927T090000Z-0a0b0c0d.age", HostLabel: "Akshay's Mac mini", At: time.Now()}
	if err := backup.StandBy(td.cfg.DataDir, h); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	td.startBackup(ctx)
	if !td.paused.Load() {
		t.Fatal("a machine standing by isn't paused")
	}
	if _, ok := td.channel("telegram"); ok {
		t.Fatal("a machine standing by still has its messaging channels")
	}
	if len(shown) != 0 {
		t.Fatal("a restart announced the handover again")
	}
	res := td.backupCheck().Run(context.Background())
	if res.State != health.Warn || res.Detail != "Standing by: moved to Akshay's Mac mini" {
		t.Fatalf("health: %+v", res)
	}
}

// The whole move: this twin backs up, another machine restores the
// snapshot, and this twin's next look finds the marker and stands by.
func TestAMoveElsewhereMakesThisTwinStandBy(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	var shown []string
	prev := desktopNotify
	desktopNotify = func(_, body string) error { shown = append(shown, body); return nil }
	t.Cleanup(func() { desktopNotify = prev })
	p, err := backup.NewPhrase()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte("name: Mirrin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := backup.SaveSettings(config.Path(), config.Backup{Recipient: p.Recipient(), RecoveryPub: p.RecoveryPub(), Target: backup.TargetFolder, Path: dir}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	td.startBackup(ctx)
	e, err := td.backupEngine()
	if err != nil || e == nil {
		t.Fatalf("engine: %v", err)
	}
	if _, err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if res := td.backupCheck().Run(ctx); res.State != health.OK || !strings.Contains(res.Detail, dir) {
		t.Fatalf("health after a backup: %+v", res)
	}
	if _, err := backup.Restore(ctx, backup.RestoreOptions{Target: e.Target, Phrase: p, Home: filepath.Join(t.TempDir(), ".mirrin"), HostLabel: "New Mac"}); err != nil {
		t.Fatal(err)
	}
	if h := td.backups.CheckHandover(ctx); h == nil || h.HostLabel != "New Mac" {
		t.Fatalf("no handover found: %+v", h)
	}
	if !td.paused.Load() {
		t.Fatal("the old twin isn't paused")
	}
	if _, ok := td.channel("telegram"); ok {
		t.Fatal("the old twin kept its channels")
	}
	if len(shown) != 1 || !strings.Contains(shown[0], "Standing by: moved to New Mac") {
		t.Fatalf("shown: %v", shown)
	}
	if res := td.backupCheck().Run(ctx); res.State != health.Warn || res.Detail != "Standing by: moved to New Mac" {
		t.Fatalf("health: %+v", res)
	}
}

func TestAFreshHandoverIsAnnounced(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	var shown []string
	prev := desktopNotify
	desktopNotify = func(_, body string) error { shown = append(shown, body); return nil }
	t.Cleanup(func() { desktopNotify = prev })
	td.standBy(&backup.Handover{HostLabel: "New Mac"}, true)
	if len(shown) != 1 || !strings.Contains(shown[0], "Standing by: moved to New Mac") || !strings.Contains(shown[0], "mirrin backup resume") {
		t.Fatalf("shown: %v", shown)
	}
	if _, err := os.Stat(td.cfg.DataDir); err != nil {
		t.Fatal(err)
	}
}

// Regression (backup merged with time's lasting pause): standing by paused
// the twin, and the pause was kept over restarts (paused.v1), so after
// `mirrin backup resume` and the restart it asks for, the twin came back
// still paused, answering everyone with the paused reply. The standby's
// pause is lifted with the standby; a pause the owner set is kept.
func TestResumeAfterStandbyLiftsThePauseOnRestart(t *testing.T) {
	restart := func(t *testing.T, td *testDaemon) *Daemon {
		t.Helper()
		cfg := td.Config()
		d2, err := New(&cfg, Options{Headless: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d2.store.Close() })
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		d2.startBackup(ctx)
		return d2
	}
	standBy := func(t *testing.T, td *testDaemon) {
		t.Helper()
		h := backup.Handover{Name: "handover-20260927T090000Z-0a0b0c0d.age", HostLabel: "New Mac", At: time.Now()}
		if err := backup.StandBy(td.cfg.DataDir, h); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		td.startBackup(ctx)
		cancel()
		if !td.paused.Load() {
			t.Fatal("standing by isn't paused")
		}
	}
	quietDesktop(t)

	t.Run("standby then resume", func(t *testing.T) {
		td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
		standBy(t, td)
		// Still standing by: a restart keeps it paused from the start.
		if d := restart(t, td); !d.paused.Load() {
			t.Fatal("a restart while standing by lifted the pause")
		}
		if was, err := backup.Resume(td.cfg.DataDir); err != nil || was == nil {
			t.Fatalf("resume: %v %v", was, err)
		}
		if d := restart(t, td); d.paused.Load() {
			t.Fatal("after `mirrin backup resume` and a restart, the twin is still paused")
		}
	})
	t.Run("the owner's own pause stays", func(t *testing.T) {
		td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
		td.SetPaused(true) // the owner paused first
		standBy(t, td)
		if _, err := backup.Resume(td.cfg.DataDir); err != nil {
			t.Fatal(err)
		}
		if d := restart(t, td); !d.paused.Load() {
			t.Fatal("resuming the standby lifted the owner's own pause")
		}
	})
}

// Forgetting a fact scrubs memory and the daily copies at once; encrypted
// snapshots already written keep it until retention removes them, so the
// next one is asked for soon rather than at 03:30 (backup merged with
// observability's forget).
func TestForgettingAFactAsksForABackup(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	td.startBackup(ctx)
	s := &backup.Scheduler{DataDir: t.TempDir(), Engine: func() (*backup.Engine, error) { return nil, nil }}
	td.backups = s
	id, err := td.store.Remember(ctx, "user", "the spare key is under the mat", "t")
	if err != nil || s.Pending() {
		t.Fatalf("remembering asked for a backup (%v)", err)
	}
	if err := td.store.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !s.Pending() {
		t.Fatal("forgetting a fact didn't ask for a backup")
	}
}

// `mirrin backup init` saves the backup section while the running twin may
// be saving a setting: both read config.yaml, change their part and write it
// back. Neither loses the other's change (config.Edit).
func TestBackupSettingsSurviveASettingSavedMeanwhile(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	if err := td.UpdateConfig(func(c *config.Config) {}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = td.UpdateConfig(func(c *config.Config) { c.LLM.Effort = []string{"low", "high"}[i%2] })
		}
	}()
	for i := range 20 {
		s := config.Backup{Recipient: "age1example", RecoveryPub: "pub", Target: "folder", Path: filepath.Join(t.TempDir(), "b", string(rune('a'+i)))}
		if err := backup.SaveSettings(config.Path(), s); err != nil {
			t.Fatal(err)
		}
		var got config.Backup
		err := config.Edit(func() (err error) { got, err = backup.LoadSettings(config.Path()); return err }) // not mid-write
		if err != nil || got.Path != s.Path {
			close(stop)
			<-done
			t.Fatalf("round %d: the backup section was lost to a settings save (%+v, %v)", i, got, err)
		}
	}
	close(stop)
	<-done
}
