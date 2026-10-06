package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// pauseKey keeps a pause across restarts: when it began and, for a pause
// for a while, when it ends (memory.Pause), or nothing. A restore keeps
// this machine's own (memory.KeyPaused).
const pauseKey = memory.KeyPaused

// pauseTimers holds each daemon's timer for the end of a pause for a while
// ("for an hour", "until tomorrow"). It lives here rather than on Daemon so
// the busy daemon.go stays as it is.
var (
	pauseMu     sync.Mutex
	pauseTimers = map[*Daemon]*time.Timer{}
)

// pauseState serialises pausing and resuming (SetPaused, PauseUntil, a
// pause for a while running out), so a pause that lands as the timer fires
// is never resumed straight away, nor left half made.
var pauseState sync.Mutex

// setPaused is SetPaused, with pauseState held.
func (d *Daemon) setPaused(p bool) {
	d.paused.Store(p)
	d.beat.SetPaused(p)
	d.savePause(p) // kept over a restart
	d.store.Audit(context.Background(), "paused", "", fmt.Sprint(p))
}

// savePause records a pause, or its end. Pause is a trust control: a
// launchd restart, a crash or the menu's Restart used to lift it without a
// word.
func (d *Daemon) savePause(p bool) {
	ctx := context.Background()
	if !p {
		d.disarmPause()
		if err := d.store.Unset(ctx, pauseKey); err != nil {
			d.log.Warn("pause: forget", "err", err)
		}
		_ = d.store.Unset(ctx, standbyPauseKey) // whoever lifted it, a standby's pause is over (backup.go)
		d.resumeStandby()
		return
	}
	v, _ := d.store.Get(ctx, pauseKey)
	cur, ok := memory.ParsePause(v)
	if ok && cur.Until.IsZero() {
		return // paused already: it keeps the time it began
	}
	since := time.Now()
	if ok {
		// A plain pause over a pause for a while: now it lasts until the
		// owner resumes, from when the first began.
		d.disarmPause()
		if !cur.Since.IsZero() {
			since = cur.Since
		}
	}
	if err := d.store.Set(ctx, pauseKey, memory.Pause{Since: since}.String()); err != nil {
		d.log.Warn("pause: save", "err", err)
	}
}

// PauseUntil pauses the twin until the owner resumes it or until passes,
// whichever comes first; then it resumes itself, as Resume in the menu
// would. The end is kept over restarts.
func (d *Daemon) PauseUntil(until time.Time) {
	if !until.After(time.Now()) {
		return
	}
	pauseState.Lock()
	defer pauseState.Unlock()
	d.setPaused(true)
	ctx := context.Background()
	since := time.Now()
	if v, _ := d.store.Get(ctx, pauseKey); v != "" {
		if cur, ok := memory.ParsePause(v); ok && !cur.Since.IsZero() {
			since = cur.Since
		}
	}
	if err := d.store.Set(ctx, pauseKey, memory.Pause{Since: since, Until: until}.String()); err != nil {
		d.log.Warn("pause: save", "err", err)
	}
	d.armPause(until)
}

// PausedUntil is when a pause for a while ends by itself: zero when the
// twin isn't paused, or is paused until the owner resumes.
func (d *Daemon) PausedUntil() time.Time {
	if !d.paused.Load() {
		return time.Time{}
	}
	v, _ := d.store.Get(context.Background(), pauseKey)
	p, _ := memory.ParsePause(v)
	return p.Until
}

// screenNight are the hours the screen's clock shows the character dozing
// when the owner has set no quiet hours. They are for the look alone: what
// can wait is held through quietHours (held.go).
const screenNight = "23:00-06:00"

// screenHours fills in what the screen shows of the twin's hours: paused
// or not, until when, the machine a standby moved to (the menu didn't
// pause it then), and the quiet hours the clock dozes through.
func (d *Daemon) screenHours(ctx context.Context, sd *ScreenData) {
	sd.Paused = d.paused.Load()
	if until := d.PausedUntil(); !until.IsZero() {
		sd.PausedUntil = until.In(d.location()).Format(time.RFC3339)
	}
	if sd.Paused {
		sd.StandingBy, _ = d.store.Get(ctx, standbyPauseKey)
	}
	sd.QuietHours = screenNight
	c := d.Config()
	for _, q := range []string{c.User.QuietHours, c.Push.QuietHours} {
		if q = strings.ReplaceAll(strings.TrimSpace(q), " ", ""); q != "" {
			sd.QuietHours = q
			break
		}
	}
}

// armPause resumes the twin at until (pauseRanOut).
func (d *Daemon) armPause(until time.Time) {
	pauseMu.Lock()
	defer pauseMu.Unlock()
	if t := pauseTimers[d]; t != nil {
		t.Stop()
	}
	pauseTimers[d] = time.AfterFunc(time.Until(until), d.pauseRanOut)
}

// disarmPause drops the timer of a pause for a while.
func (d *Daemon) disarmPause() {
	pauseMu.Lock()
	defer pauseMu.Unlock()
	if t := pauseTimers[d]; t != nil {
		t.Stop()
		delete(pauseTimers, d)
	}
}

// pauseRanOut resumes the twin when its pause for a while is over. The kept
// record decides: a pause made plain, or longer, since the timer was set
// stays; so does a machine standing by, which only `mirrin backup resume`
// or Resume in the menu brings back.
func (d *Daemon) pauseRanOut() {
	pauseState.Lock()
	defer pauseState.Unlock()
	v, err := d.store.Get(context.Background(), pauseKey)
	if err != nil {
		return // the twin has stopped
	}
	p, ok := memory.ParsePause(v)
	if !ok || !p.Expired(time.Now()) || !d.paused.Load() || d.standingBy() {
		return
	}
	d.log.Info("pause over; resuming", "until", p.Until.Local().Format(time.RFC3339))
	d.setPaused(false)
}

// restorePause puts back a pause that was on when the twin last stopped, so
// it stays paused (messages get the paused reply, nothing is scheduled) until
// the owner resumes it, and then hears what waited meanwhile. A pause for a
// while that ran out meanwhile is over.
func (d *Daemon) restorePause() {
	ctx := context.Background()
	v, _ := d.store.Get(ctx, pauseKey)
	p, ok := memory.ParsePause(v)
	if !ok {
		return
	}
	if p.Expired(time.Now()) {
		if err := d.store.Unset(ctx, pauseKey); err != nil {
			d.log.Warn("pause: forget", "err", err)
		}
		d.log.Info("a pause for a while ran out while the twin was stopped; not paused", "until", p.Until.Local().Format(time.RFC3339))
		return
	}
	since := p.Since
	if since.IsZero() {
		since = time.Now()
	}
	d.paused.Store(true)
	d.beat.PauseSince(since)
	if !p.Until.IsZero() {
		d.armPause(p.Until)
	}
	d.log.Info("still paused, as when the twin last stopped", "since", since.Local().Format(time.RFC3339))
}

// resumeStandby lifts a backup standby along with the pause: Resume in the
// menu on a machine standing by is `mirrin backup resume` (the owner chose
// this machine), and its messaging channels start again.
func (d *Daemon) resumeStandby() {
	dir := d.Config().DataDir
	if st, err := backup.LoadState(dir); err != nil || st.Standby == nil {
		return
	}
	was, err := backup.Resume(dir)
	if err != nil || was == nil {
		if err != nil {
			d.log.Warn("backup: resume", "err", err)
		}
		return
	}
	d.log.Info("backup: this machine is the twin again", "was", was.HostLabel)
	d.store.Audit(context.Background(), "backup.resume", "", was.HostLabel)
	// Resume may come from the menu or from a paired device's /pause, with
	// no terminal to warn in (as `mirrin backup resume` does): say it here.
	_ = desktopNotify(d.Config().Name, standbyResumedText(d.Config().Name, was.HostLabel))
	if d.runCtx == nil {
		return // not running: the channels start with it
	}
	for _, name := range messagingNames() {
		go func(name string) {
			if err := d.StartChannel(name); err != nil { // "not enabled" for most
				d.log.Debug("backup: resume: start channel", "channel", name, "err", err)
			}
		}(name)
	}
}

// standbyResumedText warns that the machine handed over to may answer too.
func standbyResumedText(name, host string) string {
	if name = strings.TrimSpace(name); name == "" {
		name = "Your twin"
	}
	if host = strings.TrimSpace(host); host == "" {
		host = "the other machine"
	}
	return fmt.Sprintf("%s is the twin here again. Quit Mirrin on %s first, or both will answer.", name, host)
}
