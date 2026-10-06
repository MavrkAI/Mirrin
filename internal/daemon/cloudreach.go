package daemon

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/reach"
)

// The paid availability layer in the daemon (internal/cloud, internal/reach
// cloud.go). It is inert until this machine was linked: startCloud reads one
// file and returns. A linked machine refreshes its entitlement about daily
// and, when reach.mode is cloud, is reachable at its handle through the
// relays. A machine standing by, because the twin moved to another machine
// by a backup handover or another machine took over the handle, sends
// nothing at all.

// cloudRun is one running twin's link.
type cloudRun struct {
	mu       sync.Mutex
	client   *cloud.Client
	ctx      context.Context    // the link's life here: standby ends it
	cancel   context.CancelFunc // ends the refresh loop and the endpoint
	keeping  bool               // the refresh loop is running
	reaching bool               // the endpoint loop is running
	endpoint *reach.Endpoint
	failure  string    // why the endpoint didn't start, for the self-check
	moved    time.Time // another machine took over the handle then
	expired  bool      // the address expired or was unlinked; free reach serves
	fallback string    // the free mode serving meanwhile
}

var cloudRuns sync.Map // *Daemon → *cloudRun

// cloudKeys are the keys entitlements are checked with; tests replace it.
var cloudKeys = func() map[string]ed25519.PublicKey { return entitle.EntitlementKeys }

// cloudStartReach starts the endpoint; tests replace it.
var cloudStartReach = reach.StartCloud

// cloudDeps fills in what tests set on a cloud endpoint; nil in the daemon.
var cloudDeps func(*reach.Deps)

// An endpoint that asks to start again within cloudRestartCalm of starting
// waits cloudRestartFirst (doubling) first; tests shorten both.
var (
	cloudRestartCalm  = 30 * time.Second
	cloudRestartFirst = 5 * time.Second
)

func (d *Daemon) cloudRun() *cloudRun {
	v, _ := cloudRuns.LoadOrStore(d, &cloudRun{})
	return v.(*cloudRun)
}

// startCloud starts the daily refresh on a linked machine. It runs before
// the backup schedule, so a machine whose handle moved stands by from the
// start, and sends nothing while standing by for either reason.
func (d *Daemon) startCloud(ctx context.Context) {
	cfg := d.Config()
	c, err := openLinked(cfg.DataDir)
	if err != nil {
		d.log.Warn("linked service", "err", err)
		return
	}
	if c == nil {
		return
	}
	run := d.cloudRun()
	run.mu.Lock()
	if run.keeping {
		run.mu.Unlock()
		return
	}
	parent := ctx
	ctx, cancel := context.WithCancel(parent)
	run.client, run.ctx, run.cancel, run.keeping = c, ctx, cancel, true
	run.mu.Unlock()
	// A standby ends ctx but is remembered until the twin stops.
	context.AfterFunc(parent, func() { cloudRuns.CompareAndDelete(d, run) })
	if st, err := backup.LoadState(cfg.DataDir); err == nil && st.Standby != nil {
		d.log.Info("linked service: standing by after a handover; sending nothing")
		cancel()
		return
	}
	if _, kind := c.State().Current(time.Now()); kind == cloud.Superseded {
		info, _, _ := c.State().Info()
		at := time.Time{}
		if info.Superseded != nil {
			at = info.Superseded.At
		}
		d.cloudStandby(at, false)
		return
	}
	go c.Keep(ctx, d.log)
}

// openLinked opens the link with the keys this build trusts (tests' fake).
func openLinked(dataDir string) (*cloud.Client, error) {
	st, err := cloud.OpenState(dataDir)
	if err != nil {
		return nil, err
	}
	info, ok, err := st.Info()
	if err != nil || !ok {
		return nil, err
	}
	return cloud.New(dataDir, info.API, cloudKeys())
}

// cloudStandingBy reports whether another machine took over the handle.
func (d *Daemon) cloudStandingBy() (time.Time, bool) {
	v, ok := cloudRuns.Load(d)
	if !ok {
		return time.Time{}, false
	}
	run := v.(*cloudRun)
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.moved, !run.moved.IsZero()
}

// standingBy reports whether this copy of the twin stands by: its handle
// or its backup moved to another machine.
func (d *Daemon) standingBy() bool {
	if _, ok := d.cloudStandingBy(); ok {
		return true
	}
	st, err := backup.LoadState(d.Config().DataDir)
	return err == nil && st.Standby != nil
}

// cloudStandby stops this copy of the twin because another machine holds
// its handle at a higher generation: paused, messaging channels stopped,
// the refresh and the relays quiet, and the menu says where it went.
func (d *Daemon) cloudStandby(at time.Time, fresh bool) {
	if at.IsZero() {
		at = time.Now()
	}
	run := d.cloudRun()
	run.mu.Lock()
	run.moved = at
	cancel := run.cancel
	run.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !d.paused.Load() {
		// The standby's pause: kept over restarts, and lifted with it
		// (backup.go, pause.go).
		if err := d.store.Set(context.Background(), standbyPauseKey, "another machine"); err != nil {
			d.log.Warn("mark the standby's pause", "err", err)
		}
	}
	d.SetPaused(true)
	for _, name := range messagingNames() {
		d.StopChannel(name)
	}
	line := reach.MovedLine(d.cfg.Name, at)
	d.log.Warn(line + "; standing by")
	if fresh {
		d.store.Audit(context.Background(), "reach.standby", "", at.UTC().Format(time.RFC3339))
		_ = desktopNotify(d.cfg.Name, line+". This copy is paused and standing by.")
	}
}

// startCloudReach serves the handle when reach.mode is cloud: the alarm and
// self-checks are added once, then the endpoint runs, and starts again
// when the entitlement names other relays. When the handle expires or the
// link goes, the free mode in reach.fallback takes over.
func (d *Daemon) startCloudReach(ctx context.Context, srv *api.Server) {
	cfg := d.Config()
	alarm, err := reach.OpenAlarm(cfg.DataDir)
	if err != nil {
		d.log.Warn("remote access", "err", err)
		return
	}
	reach.MountAlarm(srv, alarm)
	relayAlarms.Store(d, alarm)
	context.AfterFunc(ctx, func() { relayAlarms.CompareAndDelete(d, alarm) })
	if d.health != nil {
		d.health.Add(
			health.Func("reach", "Remote access", func(context.Context) (health.State, string, string) { return d.cloudHealth() }, nil),
			health.Func("certwatch", "Certificate watch", func(ctx context.Context) (health.State, string, string) {
				if e := d.relayEndpoint(); e != nil && e.Watcher != nil {
					return e.Watcher.Health(ctx)
				}
				return health.Off, "not watching: remote access isn't running", ""
			}, nil),
			health.Func("alarm", "Certificate alarm", alarm.Health, nil),
		)
	}
	d.runCloudReach(ctx, srv, alarm)
}

// runCloudReach starts the endpoint loop unless it runs already.
func (d *Daemon) runCloudReach(ctx context.Context, srv *api.Server, alarm *reach.Alarm) {
	run := d.cloudRun()
	run.mu.Lock()
	if run.reaching || !run.moved.IsZero() {
		run.mu.Unlock()
		return
	}
	run.reaching = true
	run.mu.Unlock()
	go func() {
		defer func() { run.mu.Lock(); run.reaching = false; run.mu.Unlock() }()
		delay := 5 * time.Second
		restartDelay := cloudRestartFirst
		for ctx.Err() == nil {
			run.mu.Lock()
			c, cctx := run.client, run.ctx
			run.mu.Unlock()
			if d.standingBy() {
				return // quiet: another machine is the twin now
			}
			if c == nil || cctx == nil || cctx.Err() != nil {
				d.cloudEnded(ctx, srv, reach.CloudUnlinked, time.Time{})
				return
			}
			deps := d.cloudDepsFor(srv, alarm)
			e, err := cloudStartReach(cctx, c, nil, deps)
			var stop *reach.CloudStop
			if errors.As(err, &stop) {
				d.cloudEnded(ctx, srv, stop.End, stop.At)
				return
			}
			if err != nil {
				run.mu.Lock()
				run.failure = err.Error()
				run.mu.Unlock()
				d.log.Warn("remote access: will retry", "err", err, "in", delay)
				if !pauseFor(ctx, delay) {
					return
				}
				delay = min(delay*2, 5*time.Minute)
				continue
			}
			delay = 5 * time.Second
			run.mu.Lock()
			run.endpoint, run.failure = e, ""
			run.mu.Unlock()
			relayEndpoints.Store(d, e)
			began := time.Now()
			<-e.Done()
			relayEndpoints.CompareAndDelete(d, e)
			end, at := e.Ended()
			switch end {
			case reach.CloudRestart:
				if time.Since(began) < cloudRestartCalm {
					// Restarting again soon after the last start: wait,
					// longer each time, so a relay and the control plane
					// that disagree can't make this machine spin.
					d.log.Warn("remote access: starting again", "in", restartDelay)
					if !pauseFor(ctx, restartDelay) {
						return
					}
					restartDelay = min(restartDelay*2, 5*time.Minute)
				} else {
					restartDelay = cloudRestartFirst
				}
				continue
			case reach.CloudRunning:
				continue // stopped from outside: the loop's checks decide
			}
			d.cloudEnded(ctx, srv, end, at)
			return
		}
	}()
}

func pauseFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// cloudDepsFor is what the endpoint needs from the daemon.
func (d *Daemon) cloudDepsFor(srv *api.Server, alarm *reach.Alarm) reach.Deps {
	cfg := d.Config()
	deps := reach.Deps{
		Server:  srv,
		DataDir: cfg.DataDir,
		Devices: d.deviceStore,
		Alarm:   alarm,
		Notify: func(ctx context.Context, text string) error {
			if owner := d.ownerChatKey(); owner != "" {
				return d.Notify(ctx, owner, text)
			}
			return desktopNotify(d.Config().Name, text)
		},
		Push:   d.PushSecurity,
		Banner: func(text string) { d.bus.Publish(events.Event{Kind: "notice", Text: text}) },
		Audit: func(kind, detail string) {
			d.store.Audit(context.Background(), kind, "", detail)
		},
		Log:   d.log,
		Reach: cfg.Reach,
	}
	if cloudDeps != nil {
		cloudDeps(&deps)
	}
	return deps
}

// cloudEnded follows the handle ending: standby, or free reach instead.
func (d *Daemon) cloudEnded(ctx context.Context, srv *api.Server, end reach.CloudEnd, at time.Time) {
	run := d.cloudRun()
	switch end {
	case reach.CloudStandby:
		d.cloudStandby(at, true)
		return
	case reach.CloudExpired:
		run.mu.Lock()
		c := run.client
		run.mu.Unlock()
		var exp time.Time
		if c != nil {
			cl, _ := c.State().Current(time.Now())
			exp = cl.Exp
		}
		if reach.ExpiryNotice(d.Config().DataDir, exp) {
			msg := "Your paid address has expired, so phones reach me the free way now"
			if fb := d.Config().Reach.Fallback; fb == "" || fb == "off" {
				msg = "Your paid address has expired, so phones can't reach me away from home for now. Open Reach from my menu to choose another way"
			}
			d.bus.Publish(events.Event{Kind: "notice", Text: msg + "."})
			_ = desktopNotify(d.Config().Name, msg+".")
		}
	}
	cfg := d.Config()
	fb := cfg.Reach.Fallback
	run.mu.Lock()
	run.expired, run.fallback = true, fb
	run.mu.Unlock()
	if fb != "tailscale" && fb != "files" {
		return
	}
	if !d.noteFreeReach(ctx, fb) {
		return // serving that way already (reach switched to cloud while it ran)
	}
	rc := cfg.Reach
	rc.Mode = fb
	reach.Start(ctx, srv, rc, cfg.API.Remote, nil)
}

// freeReach is the free reach mode each running twin serves (reach.Start),
// so the fallback isn't started a second time on the same port.
var freeReach sync.Map // *Daemon → mode

// noteFreeReach records that the free mode serves until ctx ends, and
// reports false when it serves already.
func (d *Daemon) noteFreeReach(ctx context.Context, mode string) bool {
	if mode != "tailscale" && mode != "files" {
		return true
	}
	if prev, loaded := freeReach.LoadOrStore(d, mode); loaded {
		return prev != mode
	}
	context.AfterFunc(ctx, func() { freeReach.CompareAndDelete(d, mode) })
	return true
}

// cloudHealth is the reach self-check in cloud mode.
func (d *Daemon) cloudHealth() (health.State, string, string) {
	if at, ok := d.cloudStandingBy(); ok {
		return health.Warn, reach.MovedLine(d.cfg.Name, at), "This copy is paused. Relink this machine to bring it back here"
	}
	run := d.cloudRun()
	run.mu.Lock()
	expired, fb, failure := run.expired, run.fallback, run.failure
	run.mu.Unlock()
	if expired {
		if fb == "tailscale" || fb == "files" {
			return health.Warn, "the paid address isn't active; other devices reach me by " + fb + " meanwhile", "Open Reach from the menu to choose"
		}
		return health.Warn, "the paid address isn't active, so other devices can't reach me away from home", "Open Reach from the menu to choose another way"
	}
	if failure != "" {
		return health.Warn, "remote access didn't start: " + failure, "Mirrin keeps retrying; run `mirrin reach verify`"
	}
	if e := d.cloudEndpoint(); e != nil {
		return reach.CloudHealth(e)
	}
	return health.Warn, "starting remote access", "Mirrin keeps retrying"
}

// cloudEndpoint is the running handle's endpoint, or nil.
func (d *Daemon) cloudEndpoint() *reach.Endpoint {
	if e := d.relayEndpoint(); e != nil && e.Cloud() {
		return e
	}
	return nil
}

// statusURLs are the relays' status endpoints for this twin (the PWA's
// offline page asks them).
func (d *Daemon) statusURLs() []string {
	return d.cloudEndpoint().StatusURLs()
}

// messaging reports whether name is a messaging channel, which a machine
// standing by keeps off.
func messaging(name string) bool { return slices.Contains(messagingNames(), name) }

// useCloudReach switches reach to the handle and starts it now, keeping the
// free mode in use as the fallback.
func (d *Daemon) useCloudReach(ctx context.Context) error {
	err := d.UpdateConfig(func(c *config.Config) {
		if c.Reach.Mode == "tailscale" || c.Reach.Mode == "files" {
			c.Reach.Fallback = c.Reach.Mode
		}
		c.Reach.Mode = "cloud"
		if c.Reach.StepUp == "" {
			c.Reach.StepUp = "dangerous"
		}
	})
	if err != nil {
		return err
	}
	d.startCloud(ctx)
	if srv := d.pageServer(); srv != nil {
		alarm, err := reach.OpenAlarm(d.Config().DataDir)
		if err != nil {
			return err
		}
		if v, ok := relayAlarms.Load(d); ok {
			alarm = v.(*reach.Alarm)
		}
		d.runCloudReach(ctx, srv, alarm)
	}
	d.BackupSoon("reach change")
	return nil
}

// quietCloud ends the refresh and the endpoint: the twin moved to another
// machine by a backup handover, so this one sends nothing to the service.
func (d *Daemon) quietCloud() {
	v, ok := cloudRuns.Load(d)
	if !ok {
		return
	}
	run := v.(*cloudRun)
	run.mu.Lock()
	cancel := run.cancel
	run.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// cloudMoved reports whether another machine took over the handle, as the
// link state records it, whether or not the daemon has started.
func (d *Daemon) cloudMoved() bool {
	if _, ok := d.cloudStandingBy(); ok {
		return true
	}
	st, err := cloud.OpenState(d.Config().DataDir)
	if err != nil {
		return false
	}
	info, ok, _ := st.Info()
	return ok && info.Superseded != nil
}
