package daemon

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/MavrkAI/Mirrin/internal/homelock"
)

// LockHome takes this home's claim (homelock) for work that must not
// happen while a twin runs from it, such as putting a backup of memory.db in
// place. A twin that starts meanwhile finds the home taken and doesn't run.
// LockHome waits up to wait for one that is still quitting, then returns
// ErrAlreadyRunning. release gives the claim back.
func LockHome(ctx context.Context, dataDir string, wait time.Duration) (release func(), err error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := homelock.Path(dataDir)
	deadline := time.Now().Add(wait)
	for {
		f, err := lockFile(path)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, errLocked) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, ErrAlreadyRunning
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
