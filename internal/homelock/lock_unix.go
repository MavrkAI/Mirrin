//go:build !windows

package homelock

import (
	"errors"
	"os"
	"syscall"
)

// Lock opens path and takes an exclusive lock on it, which the system drops
// when the process ends, however it ends. ErrLocked means another process
// holds it.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}
