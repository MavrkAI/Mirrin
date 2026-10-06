// Package logs keeps Mirrin's own record of what happened: a size-capped,
// rotating log file under the Mirrin home with secrets taken out, the tail
// of it for a problem report, and crash output for when the process dies.
package logs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Writer appends to a log file and rolls it over at a size cap:
// mirrin.log → mirrin.log.1 → … → mirrin.log.<keep>, the oldest dropped, so
// the logs never take more than about (keep+1) × max bytes. Several processes
// may write the same file (the menu bar app and a terminal chat); a process
// that finds the file rolled by another reopens it rather than rolling again.
type Writer struct {
	mu     sync.Mutex
	path   string
	max    int64
	keep   int
	f      *os.File
	size   int64
	writes int
	// When the file couldn't be rolled (Windows won't rename a file another
	// process has open), the size and time then: the next try waits for
	// another eighth of the cap or a minute, rather than every write
	// renaming, reopening and failing again.
	stuckAt   int64
	stuckTime time.Time
}

// renameFile is os.Rename (replaced in tests).
var renameFile = os.Rename

// NewWriter opens (or creates, 0600) the log file at path.
func NewWriter(path string, max int64, keep int) (*Writer, error) {
	if max <= 0 {
		max = MaxSize
	}
	if keep < 1 {
		keep = 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w := &Writer{path: path, max: max, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.f = f
	w.size = 0
	if st, err := f.Stat(); err == nil {
		w.size = st.Size()
	}
	return nil
}

// Write appends p, rolling the file over first if p would take it past the cap.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	// Other processes append too; now and then, catch up with the real size.
	if w.writes++; w.writes%64 == 0 {
		if st, err := w.f.Stat(); err == nil {
			w.size = st.Size()
		}
	}
	if w.size+int64(len(p)) > w.max && w.size > 0 && w.mayRoll() {
		w.roll()
		if w.f == nil {
			return 0, errors.New("the log file can't be opened")
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// mayRoll reports whether it's time to try rolling the file (again).
func (w *Writer) mayRoll() bool {
	return w.stuckTime.IsZero() || w.size >= w.stuckAt+w.max/8 || time.Since(w.stuckTime) >= time.Minute
}

// roll rotates the files, unless another process already did.
func (w *Writer) roll() {
	cur, curErr := w.f.Stat()
	onDisk, diskErr := os.Stat(w.path)
	w.f.Close()
	w.f = nil
	if curErr == nil && diskErr == nil && !os.SameFile(cur, onDisk) {
		// Someone else rolled it: write to the new file, if it has room.
		if w.open() == nil && w.size < w.max {
			w.stuckTime = time.Time{}
			return
		}
		if w.f != nil {
			w.f.Close()
			w.f = nil
		}
	}
	for i := w.keep - 1; i >= 1; i-- {
		_ = renameFile(rolled(w.path, i), rolled(w.path, i+1))
	}
	// On Windows this fails while another process has the file open; it
	// then runs a little long until one of them can roll it.
	err := renameFile(w.path, rolled(w.path, 1))
	_ = w.open()
	if err != nil {
		w.stuckAt, w.stuckTime = w.size, time.Now()
	} else {
		w.stuckTime = time.Time{}
	}
}

func rolled(path string, i int) string { return fmt.Sprintf("%s.%d", path, i) }

// Close closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
