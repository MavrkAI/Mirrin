//go:build windows

package config

import (
	"errors"
	"os"
	"syscall"
	"time"
)

const errSharingViolation syscall.Errno = 32

// lockWait takes lockFile, trying again every few milliseconds for up to wait.
func lockWait(path string, wait time.Duration) (*os.File, error) {
	deadline := time.Now().Add(wait)
	for {
		f, err := lockFile(path)
		if err == nil || !errors.Is(err, errLocked) || time.Now().After(deadline) {
			return f, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lockFile opens path with no sharing, which works as an exclusive lock that
// Windows drops when the file is closed or the process ends.
func lockFile(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, errLocked
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
