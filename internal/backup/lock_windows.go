//go:build windows

package backup

import (
	"errors"
	"os"
	"syscall"
)

const errSharingViolation syscall.Errno = 32

// lockFile opens path with no sharing, an exclusive lock that Windows drops
// when the process ends.
func lockFile(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
