package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// On macOS 12 and 13, iCloud Drive keeps a file it has moved off the Mac
// ("Optimise Mac Storage") as a placeholder named ".<name>.icloud". The
// folder target lists it under its real name and, on Get, asks for it back
// (brctl download) and waits until it is there.

// placeholderOf is the placeholder a file called name leaves behind.
func placeholderOf(name string) string { return "." + name + ".icloud" }

// fromPlaceholder returns the name of the file a placeholder stands for.
func fromPlaceholder(entry string) (string, bool) {
	if !strings.HasPrefix(entry, ".") || !strings.HasSuffix(entry, ".icloud") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(entry, "."), ".icloud")
	return name, name != ""
}

// icloudDownload asks iCloud Drive to bring a file back to this Mac (a
// variable so tests can stand in for brctl).
var icloudDownload = func(ctx context.Context, path string) error {
	out, err := exec.CommandContext(ctx, "brctl", "download", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iCloud Drive didn't download it (%v: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// icloudWait is how long Get waits for a download, and icloudPoll how
// often it looks.
var (
	icloudWait = 10 * time.Minute
	icloudPoll = time.Second
)

// fetchPlaceholder brings back the file at p when only its placeholder is
// there. It returns fs.ErrNotExist when there is neither.
func fetchPlaceholder(ctx context.Context, p string) error {
	ph := filepath.Join(filepath.Dir(p), placeholderOf(filepath.Base(p)))
	if _, err := os.Stat(ph); err != nil {
		return fs.ErrNotExist
	}
	if err := icloudDownload(ctx, p); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, icloudWait)
	defer cancel()
	t := time.NewTicker(icloudPoll)
	defer t.Stop()
	for {
		if _, err := os.Stat(p); err == nil {
			if _, err := os.Stat(ph); errors.Is(err, fs.ErrNotExist) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("iCloud Drive hasn't downloaded %s yet; check this Mac is online and try again", filepath.Base(p))
		case <-t.C:
		}
	}
}
