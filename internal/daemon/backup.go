package daemon

import (
	"context"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// Encrypted backups (internal/backup): nightly at 03:30 or as soon as the
// machine is awake after that, 10 minutes after a security change
// (BackupSoon), and an hourly look for a handover marker, which means the
// twin was restored on another machine and this one must stand by.
//
// The settings are read from config.yaml before every run, so turning
// backups on with `mirrin backup init` needs no restart.

// startBackup runs the backup schedule until ctx ends. A machine that is
// standing by stays paused, with its messaging channels stopped, across
// restarts.
func (d *Daemon) startBackup(ctx context.Context) {
	s := &backup.Scheduler{
		Engine:    d.backupEngine,
		DataDir:   d.cfg.DataDir,
		Loc:       d.loc,
		Zone:      d.location, // timekeeping.go: 03:30 where the machine is now
		Log:       d.log,
		OnStandby: func(h backup.Handover) { d.standBy(&h, true) },
		OnRun:     d.backupRan,
	}
	d.backups = s
	// A forgotten fact stays in snapshots already written until retention
	// removes them (docs/backup-format.md); the next one needn't wait for 03:30.
	d.store.OnForgot(func() { d.BackupSoon("forget") })
	d.store.OnForgot(func() { d.setPortraitAside(context.Background()) }) // portrait.go
	st, err := backup.LoadState(d.cfg.DataDir)
	switch {
	case err == nil && st.Standby != nil:
		d.standBy(st.Standby, false)
	case err == nil && d.standbyPaused() && !d.cloudMoved():
		// `mirrin backup resume` lifted the standby while the twin was
		// stopped: the pause that came with it goes too. A pause the owner
		// set themselves was never marked, so it stays (pause.go).
		d.log.Info("backup: this machine is the twin again; resuming")
		d.SetPaused(false)
	}
	s.Start(ctx)
}

// standbyPauseKey marks a pause that a standby put on (the owner's own pause
// isn't marked), so the pause, kept over restarts like any (pause.go), is
// lifted with the standby and only then.
const standbyPauseKey = memory.KeyStandbyPause

func (d *Daemon) standbyPaused() bool {
	v, _ := d.store.Get(context.Background(), standbyPauseKey)
	return v != ""
}

// BackupSoon asks for a snapshot 10 minutes from now: after a pairing, a
// revoke, a passkey enrolment or a reach change, so a restore never brings
// back a device the owner removed. A burst of changes makes one snapshot.
func (d *Daemon) BackupSoon(reason string) {
	if d.backups != nil {
		d.backups.Trigger(reason)
	}
}

// backupEngine builds the engine from the settings on disk now; nil when
// backups aren't set up.
func (d *Daemon) backupEngine() (*backup.Engine, error) {
	s, err := backup.LoadSettings(config.Path())
	if err != nil || s.Recipient == "" {
		return nil, err
	}
	if s.Target == backup.TargetCloud && d.standingBy() {
		// Another machine is the twin now: backing up to the paid service
		// would contact it, and this machine sends it nothing.
		return nil, nil
	}
	cfg := d.Config()
	cfg.Backup = s
	e, err := backup.NewEngine(&cfg, config.Home(), d.version)
	if err != nil {
		return nil, err
	}
	e.Log = d.log
	return e, nil
}

// standBy stops this copy of the twin because it moved to another machine:
// paused, messaging channels stopped, and the self-check says where it went.
func (d *Daemon) standBy(h *backup.Handover, fresh bool) {
	d.quietCloud() // cloudreach.go: the linked service hears nothing more from here
	if !d.paused.Load() {
		// This pause is the standby's: kept over restarts, from the very
		// start (New, before tasks and the schedule), and lifted with it.
		mark := h.HostLabel
		if mark == "" {
			mark = "another machine"
		}
		if err := d.store.Set(context.Background(), standbyPauseKey, mark); err != nil {
			d.log.Warn("backup: mark the standby's pause", "err", err)
		}
	}
	d.SetPaused(true)
	for _, name := range messagingNames() {
		d.StopChannel(name)
	}
	msg := backup.StandbyMessage(h)
	d.log.Warn("backup: " + msg)
	if fresh {
		d.store.Audit(context.Background(), "backup.standby", "", h.HostLabel)
		_ = desktopNotify(d.cfg.Name, msg+". This copy is paused; run `mirrin backup resume` to bring it back.")
	}
}

// backupRan follows a run: once a year, the owner is asked whether they
// still have their 12 words.
func (d *Daemon) backupRan(_ backup.Manifest, err error) {
	if err != nil {
		return
	}
	st, lerr := backup.LoadState(d.cfg.DataDir)
	if lerr != nil || !backup.NudgeDue(st, time.Now()) {
		return
	}
	owner := d.ownerChatKey()
	if owner == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if d.Notify(ctx, owner, backup.NudgeText) == nil {
		_ = backup.UpdateState(d.cfg.DataDir, func(s *backup.State) { s.Nudged = time.Now() })
	}
}

// backupCheck is the self-check for backups.
func (d *Daemon) backupCheck() health.Check {
	return health.Func("backup", "Backup", func(context.Context) (health.State, string, string) {
		s, _ := backup.LoadSettings(config.Path())
		st, _ := backup.LoadState(d.cfg.DataDir)
		where := ""
		if s.Recipient != "" {
			where = " (" + backup.Where(s) + ")"
		}
		return backup.Health(st, backup.Settings{Recipient: s.Recipient, Where: where}, time.Now())
	}, nil)
}
