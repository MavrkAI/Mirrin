package logs

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

const (
	// MaxSize is where a log file rolls over.
	MaxSize = 5 << 20
	// Keep is how many rolled files are kept, so the logs stay under
	// (Keep+1) × MaxSize: 20 MB.
	Keep = 3
	// crashMax caps the crash log.
	crashMax = 1 << 20
)

// Dir is where Mirrin keeps its logs.
func Dir(home string) string { return filepath.Join(home, "logs") }

// Path is the main log file.
func Path(home string) string { return filepath.Join(Dir(home), "mirrin.log") }

// CrashPath is where a crash (a panic or fatal error) is written.
func CrashPath(home string) string { return filepath.Join(Dir(home), "crash.log") }

// Options says where a logger writes.
type Options struct {
	// Home is the Mirrin home; the log file is <home>/logs/mirrin.log.
	Home string
	// Level is the least a record needs to reach the file.
	Level slog.Level
	// Console, if set, also gets records at ConsoleLevel or above, and any
	// at Info or above whose message ConsoleAlways accepts (such as the
	// "online" line the service's own log is read for).
	Console       io.Writer
	ConsoleLevel  slog.Level
	ConsoleAlways func(msg string) bool
	// Redactor takes secrets out; nil makes one for Home.
	Redactor *Redactor
}

// New returns a logger that writes to the rotating log file (and the console,
// if asked), with secrets taken out of everything. If the file can't be
// opened, the error is returned with a console-only logger (or a silent one).
func New(o Options) (*slog.Logger, io.Closer, error) {
	r := o.Redactor
	if r == nil {
		r = NewRedactor(o.Home)
	}
	var handlers []slog.Handler
	var closer io.Closer = nopCloser{}
	w, err := NewWriter(Path(o.Home), MaxSize, Keep)
	if err == nil {
		closer = w
		handlers = append(handlers, slog.NewTextHandler(w, &slog.HandlerOptions{Level: o.Level}))
	}
	if o.Console != nil {
		var h slog.Handler = slog.NewTextHandler(o.Console, &slog.HandlerOptions{Level: slog.LevelDebug})
		h = &consoleFilter{next: h, level: o.ConsoleLevel, always: o.ConsoleAlways}
		handlers = append(handlers, h)
	}
	switch len(handlers) {
	case 0:
		return slog.New(slog.DiscardHandler), closer, err
	case 1:
		return slog.New(Redacting(handlers[0], r)), closer, err
	}
	return slog.New(Redacting(slog.NewMultiHandler(handlers...), r)), closer, err
}

// CaptureCrashes makes a crash (a panic nobody recovered, a fatal runtime
// error) also land in logs/crash.log, not only on a standard error that a
// double-clicked app sends nowhere. The file is started afresh past 1 MB.
func CaptureCrashes(home string) error {
	path := CrashPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > crashMax {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return debug.SetCrashOutput(f, debug.CrashOptions{})
}

// Tail returns up to n of the newest lines of the log (reaching back into
// the last rolled file when the current one is short), oldest first.
func Tail(home string, n int) ([]string, error) {
	return TailFiles(n, rolled(Path(home), 1), Path(home))
}

// TailFiles returns up to n of the last lines across files, read in order.
// Missing files are skipped; only the end of a large file is read.
func TailFiles(n int, paths ...string) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	ring := make([]string, 0, n)
	found := false
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		found = true
		const window = 1 << 20
		if st, err := f.Stat(); err == nil && st.Size() > window {
			if _, err := f.Seek(st.Size()-window, io.SeekStart); err == nil {
				r := bufio.NewReader(f)
				_, _ = r.ReadString('\n') // skip the partial first line
				ring = scanInto(ring, n, r)
				f.Close()
				continue
			}
		}
		ring = scanInto(ring, n, f)
		f.Close()
	}
	if !found {
		return nil, os.ErrNotExist
	}
	return ring, nil
}

func scanInto(ring []string, n int, r io.Reader) []string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if len(ring) == n {
			copy(ring, ring[1:])
			ring = ring[:n-1]
		}
		ring = append(ring, line)
	}
	return ring
}

type fileOnlyKey struct{}

// FileOnly marks a context so that what is logged with it goes to the log
// file and not the console: for something the console has already shown in
// its own words, such as the error a command stops on.
func FileOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, fileOnlyKey{}, true)
}

func fileOnly(ctx context.Context) bool {
	v, _ := ctx.Value(fileOnlyKey{}).(bool)
	return v
}

// consoleFilter passes records at a level, plus the few messages asked for.
type consoleFilter struct {
	next   slog.Handler
	level  slog.Level
	always func(string) bool
}

// alwaysFrom is the least level a message ConsoleAlways accepts can have.
const alwaysFrom = slog.LevelInfo

func (c *consoleFilter) Enabled(ctx context.Context, l slog.Level) bool {
	if fileOnly(ctx) {
		return false
	}
	return l >= c.level || (c.always != nil && l >= alwaysFrom)
}

func (c *consoleFilter) Handle(ctx context.Context, r slog.Record) error {
	if fileOnly(ctx) {
		return nil
	}
	if r.Level < c.level && (c.always == nil || r.Level < alwaysFrom || !c.always(r.Message)) {
		return nil
	}
	return c.next.Handle(ctx, r)
}

func (c *consoleFilter) WithAttrs(as []slog.Attr) slog.Handler {
	return &consoleFilter{next: c.next.WithAttrs(as), level: c.level, always: c.always}
}

func (c *consoleFilter) WithGroup(name string) slog.Handler {
	return &consoleFilter{next: c.next.WithGroup(name), level: c.level, always: c.always}
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
