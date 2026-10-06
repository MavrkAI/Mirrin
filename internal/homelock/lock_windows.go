//go:build windows

package homelock

import (
	"errors"
	"os"
	"syscall"
)

const errSharingViolation syscall.Errno = 32

// Lock opens path with no sharing, which works as an exclusive lock that
// Windows drops when the process ends. ErrLocked means another process
// holds it.
func Lock(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
