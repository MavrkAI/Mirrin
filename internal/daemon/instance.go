package daemon

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/homelock"
)

// ErrAlreadyRunning means another twin is already running from this home.
var ErrAlreadyRunning = errors.New("Mirrin is already running on this machine (in the menu bar or as the background service). Talk to it with `mirrin chat`, or quit it first (Quit in its menu, or `mirrin service stop`) to start this one")

// lockFile takes a home's claim; errLocked means another process holds it.
var (
	lockFile  = homelock.Lock
	errLocked = homelock.ErrLocked
)

// claimGrace is how long a copy that isn't under the service manager waits
// for another one to finish quitting (a restart) before giving up.
var claimGrace = 3 * time.Second

// Claim makes this daemon the one twin for its home, so two never share the
// WhatsApp session, the bot tokens, the heartbeat or the memory. Under the
// service manager it waits for another copy to quit; anywhere else another
// copy is ErrAlreadyRunning, after a moment's grace for one that is
// restarting. Run claims too; claiming again is a no-op.
func (d *Daemon) Claim(ctx context.Context) error {
	d.instMu.Lock()
	defer d.instMu.Unlock()
	if d.instance != nil {
		return nil
	}
	if err := os.MkdirAll(d.cfg.DataDir, 0o700); err != nil {
		return err
	}
	path := homelock.Path(d.cfg.DataDir)
	grace := time.Now().Add(claimGrace)
	waiting := false
	for {
		f, err := lockFile(path)
		if err == nil {
			d.instance = f
			if waiting {
				d.log.Info("the other copy quit; starting")
			}
			return nil
		}
		if !errors.Is(err, errLocked) {
			return err
		}
		if !underServiceManager() && time.Now().After(grace) {
			return ErrAlreadyRunning
		}
		if underServiceManager() && !waiting {
			waiting = true
			d.log.Info("another copy of Mirrin is running from this home; waiting for it to quit")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// HomeInUse reports whether a twin holds the claim on dataDir right now,
// whether or not its local API answers (api.listen "", or an address another
// program took). It never creates anything.
func HomeInUse(dataDir string) bool { return homelock.InUse(dataDir) }

// release gives up the claim, so another twin can start from this home.
func (d *Daemon) release() {
	d.instMu.Lock()
	defer d.instMu.Unlock()
	if d.instance != nil {
		d.instance.Close()
		d.instance = nil
	}
}

// underServiceManager reports whether the service manager started this
// process (config.UnderServiceManager).
func underServiceManager() bool { return config.UnderServiceManager() }
