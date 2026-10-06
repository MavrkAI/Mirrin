//go:build !windows

package config

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockWait opens path and waits up to wait for an exclusive lock on it,
// which the system drops when the file is closed or the process ends. It
// waits in the kernel, so an edit waiting its turn gets it as soon as the
// one before lets go, however quickly others follow.
func lockWait(path string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	got := make(chan error, 1)
	go func() {
		for {
			err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
			if !errors.Is(err, syscall.EINTR) {
				got <- err
				return
			}
		}
	}()
	select {
	case err := <-got:
		if err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	case <-time.After(wait):
		go func() { <-got; f.Close() }() // let go of it whenever it comes
		return nil, errLocked
	}
}
