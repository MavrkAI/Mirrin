package backup

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"time"
)

// renameRetries is how long a refused rename is tried again on Windows.
var renameRetries = 20 // × renamePause
const renamePause = 100 * time.Millisecond

// renameFolder is os.Rename, tried again for a moment on Windows: a virus
// scanner or the search indexer looking at files just written holds them
// briefly, and Windows then refuses to move their folder ("Access is
// denied") though nothing of ours has it open.
func renameFolder(from, to string) error {
	err := os.Rename(from, to)
	for i := 0; err != nil && runtime.GOOS == "windows" && i < renameRetries && heldBriefly(err); i++ {
		time.Sleep(renamePause)
		err = os.Rename(from, to)
	}
	return err
}

// heldBriefly reports a refusal that may pass: access denied, or another
// program using the file (ERROR_SHARING_VIOLATION).
func heldBriefly(err error) bool {
	var errno syscall.Errno
	return errors.Is(err, os.ErrPermission) || errors.As(err, &errno) && errno == 32
}
