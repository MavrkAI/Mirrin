package cloud

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"time"
)

// heldTries and heldPause bound how long a file another reader or writer
// holds is tried again on Windows.
const (
	heldTries = 20
	heldPause = 50 * time.Millisecond
)

// whileHeld runs f, and on Windows runs it again for a moment while it is
// refused because the file is in use: replacing a file someone is reading
// is refused there, and so is opening one while it is being replaced. A
// refresh writing the entitlement as the watcher reads it must not look
// like an expired link.
func whileHeld(f func() error) error {
	err := f()
	for i := 0; err != nil && runtime.GOOS == "windows" && i < heldTries && heldBriefly(err); i++ {
		time.Sleep(heldPause)
		err = f()
	}
	return err
}

// heldBriefly reports a refusal that may pass: access denied, or another
// program using the file (ERROR_SHARING_VIOLATION).
func heldBriefly(err error) bool {
	var errno syscall.Errno
	return errors.Is(err, os.ErrPermission) || errors.As(err, &errno) && errno == 32
}
