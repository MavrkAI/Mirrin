package daemon

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Backups kept with the paid service use its client, so a machine standing
// by, because the twin moved away by a backup handover or because another
// machine recovered the account, doesn't even open that target; one that
// is the twin does (the positive control).
func TestCloudBackupsStayQuietOnAMachineStandingBy(t *testing.T) {
	for _, why := range []string{"the twin", "handover", "recovered elsewhere"} {
		t.Run(why, func(t *testing.T) {
			td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
			stubDesktop(t)
			if err := os.WriteFile(config.Path(), []byte("name: Mirrin\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			p, _ := backup.NewPhrase()
			if err := backup.SaveSettings(config.Path(), config.Backup{Recipient: p.Recipient(), RecoveryPub: p.RecoveryPub(), Target: backup.TargetCloud}); err != nil {
				t.Fatal(err)
			}
			var opened atomic.Int32
			prev := backup.OpenCloud
			backup.OpenCloud = func(ns string) (backup.Target, error) {
				opened.Add(1)
				return backup.Folder(t.TempDir()), nil
			}
			t.Cleanup(func() { backup.OpenCloud = prev })
			switch why {
			case "handover":
				td.standBy(&backup.Handover{HostLabel: "Akshay's Mac mini", At: time.Now()}, false)
				if err := backup.StandBy(td.cfg.DataDir, backup.Handover{Name: "handover-20260927T090000Z-0a0b0c0d.age", HostLabel: "Akshay's Mac mini", At: time.Now()}); err != nil {
					t.Fatal(err)
				}
			case "recovered elsewhere":
				td.cloudStandby(time.Now(), false)
			}
			e, err := td.backupEngine()
			if err != nil {
				t.Fatal(err)
			}
			if why == "the twin" {
				if e == nil || opened.Load() != 1 {
					t.Fatalf("the twin's engine %v, target opened %d times", e, opened.Load())
				}
				return
			}
			if e != nil || opened.Load() != 0 {
				t.Fatalf("a machine standing by built an engine (%v) and opened the paid target %d times", e, opened.Load())
			}
			if h := td.backups; h != nil {
				t.Fatal("unexpected scheduler")
			}
		})
	}
}
