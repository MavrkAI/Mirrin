package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Signal's session: with backup.signal on, a snapshot also carries
// signal-cli's data folder (the linked account and its keys) as one tar
// under sessions/, and a restore with --with-sessions puts it back. Off by
// default, like the WhatsApp session: two machines on one linked Signal
// account conflict.

// nameSignal is where a snapshot keeps signal-cli's data folder.
const nameSignal = "sessions/signal-cli.tar"

// SignalDataDir is signal-cli's data folder: backup.signal_dir, else
// $XDG_DATA_HOME/signal-cli, else ~/.local/share/signal-cli (signal-cli's
// own default on every system).
func SignalDataDir(s config.Backup) string {
	if s.SignalDir != "" {
		p, _ := filepath.Abs(expandHome(s.SignalDir))
		return p
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "signal-cli")
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(user, ".local", "share", "signal-cli")
}

// signalItems is signal-cli's data folder as a snapshot takes it, when it
// is there and looks like one.
func signalItems(l Layout) []item {
	if !signalDirOK(l.SignalDir, l.Home) || !exists(filepath.Join(l.SignalDir, "data", "accounts.json")) {
		return nil
	}
	return []item{{name: nameSignal, src: l.SignalDir, tree: true}}
}

// signalDirOK refuses a signal_dir that can't be signal-cli's own folder:
// a filesystem root, the user's home, the current folder, or one holding
// the twin's home. A backup would tar all of it, and a restore move all of
// it aside.
func signalDirOK(dir, mirrinHome string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	dir = filepath.Clean(dir)
	if filepath.Dir(dir) == dir {
		return false
	}
	if user, err := os.UserHomeDir(); err == nil && within(dir, user) {
		return false
	}
	if cwd, err := os.Getwd(); err == nil && within(dir, cwd) {
		return false
	}
	return mirrinHome == "" || !within(dir, mirrinHome)
}

// signalSkip is what a backup leaves out of signal-cli's folder: SQLite's
// side files (each database is copied whole, consistently) and the
// attachments, which can be large and aren't the session.
func signalSkip(rel string, dir bool) bool {
	if dir {
		return rel == "attachments"
	}
	for _, suf := range []string{"-wal", "-shm", "-journal"} {
		if strings.HasSuffix(rel, suf) {
			return true
		}
	}
	return false
}

// sqliteMagic starts every SQLite database file.
const sqliteMagic = "SQLite format 3\x00"

func isSQLite(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, len(sqliteMagic))
	_, err = io.ReadFull(f, b)
	return err == nil && string(b) == sqliteMagic
}

// tarTree writes the folders and regular files under src to a tar at dst,
// while signal-cli may be writing to them. Each file is first copied into
// scratch (a SQLite database through VACUUM INTO, so it is consistent) and
// the tar takes the copy; a file that goes away meanwhile is left out.
// Links and anything else are left out too.
func tarTree(ctx context.Context, src, dst, scratch string) error {
	if st, err := os.Stat(src); err != nil {
		return err
	} else if !st.IsDir() {
		return fmt.Errorf("%s isn't a folder", src)
	}
	tmp, err := os.MkdirTemp(scratch, "tree-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(out)
	n := 0
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != src && errors.Is(err, fs.ErrNotExist) {
				return nil // gone since the folder was read
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if signalSkip(name, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o700, ModTime: info.ModTime(), Format: tar.FormatPAX})
		case !d.Type().IsRegular():
			return nil
		}
		n++
		cp := filepath.Join(tmp, strconv.Itoa(n))
		if isSQLite(p) {
			err = vacuumInto(ctx, p, cp)
		} else {
			err = copyFile(p, cp)
		}
		if errors.Is(err, fs.ErrNotExist) && !exists(p) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		defer os.Remove(cp)
		f, err := os.Open(cp)
		if err != nil {
			return err
		}
		defer f.Close()
		cinfo, err := f.Stat()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: int64(info.Mode().Perm()), Size: cinfo.Size(), ModTime: info.ModTime(), Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return err
	})
	if cerr := tw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// untarTree unpacks a tar tarTree wrote into dst, which must not be there
// yet. Only folders and regular files with safe names are taken.
func untarTree(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(in)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if !safeName(name) {
			return fmt.Errorf("it holds an unsafe path (%q)", h.Name)
		}
		p := filepath.Join(dst, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
			continue
		case tar.TypeReg:
		default:
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode).Perm()&0o700|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
}

// restoreSignal puts signal-cli's data folder from a restored home back in
// dir. A folder already there is moved aside, never deleted. It returns
// where the old one went ("" when there was none).
func restoreSignal(home, dir string, now time.Time) (string, error) {
	tarPath := filepath.Join(home, filepath.FromSlash(nameSignal))
	if !exists(tarPath) {
		return "", nil
	}
	if dir == "" {
		return "", errors.New("signal-cli's data folder isn't known here")
	}
	if !signalDirOK(dir, home) {
		return "", fmt.Errorf("%s can't be signal-cli's data folder (check backup.signal_dir)", dir)
	}
	var aside string
	if _, err := os.Lstat(dir); err == nil {
		aside = dir + ".before-restore-" + now.UTC().Format("20060102-150405")
		if err := os.Rename(dir, aside); err != nil {
			return "", err
		}
	}
	if err := untarTree(tarPath, dir); err != nil {
		_ = os.RemoveAll(dir)
		if aside != "" {
			_ = os.Rename(aside, dir)
		}
		return "", err
	}
	_ = os.Remove(tarPath)
	_ = os.Remove(filepath.Dir(tarPath)) // sessions/, when that was all it held
	return aside, nil
}
