package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

const serviceLogLimit = 5 << 20

var serviceLogMu sync.Mutex

// MaintainLogs bounds the files opened by launchd/systemd. Copy/truncate
// preserves their open file descriptors; renaming alone would leave the
// service writing forever into an old file. A write racing the truncate can
// be lost; the separate structured mirrin.log remains the primary log.
func MaintainLogs(parent context.Context) func() {
	if !config.ServiceMarked() || os.Getenv(supervisedLogsEnv) == "1" {
		return func() {}
	}
	if err := rotationAllowed(runtime.GOOS, unitPath(runtime.GOOS, userHome(), Name)); err != nil {
		slog.Warn(err.Error())
		return func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	dir := logDir()
	rotateServiceLogs(dir, serviceLogLimit)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				rotateServiceLogs(dir, serviceLogLimit)
			}
		}
	}()
	return func() { cancel(); <-done }
}

// rotateServiceLogs bounds the service manager's logs in dir.
func rotateServiceLogs(dir string, limit int64) error {
	serviceLogMu.Lock()
	defer serviceLogMu.Unlock()
	var errs []error
	for _, ext := range []string{".err", ".out", ".err.log", ".out.log"} {
		if err := rotateServiceLog(filepath.Join(dir, Name+ext), limit); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func rotateServiceLog(path string, limit int64) error {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() < limit {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	// Keep at most limit bytes, even for an old, previously unbounded log.
	if _, err := f.Seek(max(0, st.Size()-limit), io.SeekStart); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".service-log-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, copyErr := io.CopyN(tmp, f, min(st.Size(), limit))
	closeErr := tmp.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	for i := 2; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", path, i)
		to := fmt.Sprintf("%s.%d", path, i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(tmp.Name(), path+".1"); err != nil {
		return err
	}
	return f.Truncate(0)
}

// Old systemd units used file:, which retains the writer's offset after a
// truncate. Never truncate those live files; reinstall opts into append:.
func rotationAllowed(goos, unit string) error {
	if goos != "linux" {
		return nil
	}
	b, err := os.ReadFile(unit)
	if err == nil {
		settings := map[string]string{}
		section := ""
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				section = line
				continue
			}
			if section != "[Service]" {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if ok {
				settings[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		if strings.HasPrefix(settings["StandardOutput"], "append:") && strings.HasPrefix(settings["StandardError"], "append:") {
			return nil
		}
	}
	return errors.New("Service log rotation is waiting for an updated Linux unit. Run mirrin service install to refresh it.")
}
