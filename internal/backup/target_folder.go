package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// folder keeps objects as files in a directory: a local disk, a NAS mount,
// or a synced folder such as iCloud Drive.
type folder struct {
	// root is the folder that must already be there: the one the owner
	// chose, or iCloud Drive itself. Writing never makes it, so a disk or
	// share that isn't connected fails the backup instead of quietly
	// filling a fresh folder on the disk the backups are meant to protect.
	root string
	// dir is where the objects are: root, or a folder inside it that is
	// made as needed (Mirrin Backups/<ns>).
	dir   string
	label string
	// missing says what to do when root isn't there.
	missing string
	// also is another folder with backups of the same namespace, read
	// beside dir but never written: in iCloud Drive, the drive's other
	// backup folder (Mirrin's, or the one from before the rename) when it
	// holds some too.
	also string
}

// Folder is a target that keeps snapshots in dir, which must be there.
func Folder(dir string) Target { return &folder{root: dir, dir: dir} }

// nsFolder is the namespace folder ns inside base, the folder the owner chose.
func nsFolder(base, ns, label string) Target {
	dir := base
	if ns != "" {
		dir = filepath.Join(base, ns)
	}
	if label == "" {
		label = base // the folder the owner chose, not the namespace inside it
	}
	return &folder{root: base, dir: dir, label: label}
}

// notThereError is a backup folder that isn't there: a disk or network
// drive that isn't connected, or iCloud Drive turned off.
type notThereError struct{ msg string }

func (e *notThereError) Error() string { return e.msg }

// checkRoot fails when the folder the owner chose isn't there.
func (f *folder) checkRoot() error {
	if st, err := os.Stat(f.root); err == nil && st.IsDir() {
		return nil
	}
	if f.missing != "" {
		return &notThereError{f.missing}
	}
	return &notThereError{fmt.Sprintf("the backup folder %s isn't there (is its disk or network drive connected?)", f.root)}
}

// makeDir makes dir inside root, one folder at a time, never root itself.
func (f *folder) makeDir() error {
	if err := f.checkRoot(); err != nil {
		return err
	}
	rel, err := filepath.Rel(f.root, f.dir)
	if err != nil || !filepath.IsLocal(rel) && rel != "." {
		return fmt.Errorf("the backup folder %s isn't inside %s", f.dir, f.root)
	}
	p := f.root
	for _, el := range strings.Split(rel, string(filepath.Separator)) {
		if el == "." || el == "" {
			continue
		}
		p = filepath.Join(p, el)
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("can't make the backup folder %s (%v)", p, err)
		}
	}
	return nil
}

func (f *folder) String() string {
	if f.label != "" {
		return f.label
	}
	return f.dir
}

// Dir is where the objects are.
func (f *folder) Dir() string { return f.dir }

func (f *folder) path(name string) (string, error) {
	if _, _, ok := parseName(name); !ok {
		return "", fmt.Errorf("%q isn't a backup name", name)
	}
	return filepath.Join(f.dir, name), nil
}

func (f *folder) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	dest, err := f.path(name)
	if err != nil {
		return err
	}
	if err := f.makeDir(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.dir, ".partial-*")
	if err != nil {
		return fmt.Errorf("can't write to the backup folder %s (%v)", f.dir, err)
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	n, err := io.Copy(tmp, ctxReader{ctx, r})
	if err != nil {
		return err
	}
	if size >= 0 && n != size {
		return fmt.Errorf("wrote %d bytes of %d", n, size)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s is already there", name)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return err
	}
	ok = true
	return nil
}

func (f *folder) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if _, err := f.path(name); err != nil {
		return nil, err
	}
	rc, err := getIn(ctx, f.dir, name)
	if errors.Is(err, ErrNotFound) && f.also != "" {
		return getIn(ctx, f.also, name)
	}
	return rc, err
}

// getIn opens the object name in dir, bringing it back from iCloud Drive
// first when only its placeholder is there.
func getIn(ctx context.Context, dir, name string) (io.ReadCloser, error) {
	p := filepath.Join(dir, name)
	file, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		// Only iCloud Drive's placeholder may be here (icloud.go).
		if ferr := fetchPlaceholder(ctx, p); ferr != nil {
			if errors.Is(ferr, fs.ErrNotExist) {
				return nil, ErrNotFound
			}
			return nil, ferr
		}
		file, err = os.Open(p)
	}
	return file, err
}

func (f *folder) List(ctx context.Context) ([]Object, error) {
	out, err := listIn(f.dir)
	if errors.Is(err, fs.ErrNotExist) {
		// No backups yet, unless the folder the owner chose is missing
		// too: then it isn't connected, and "no backups" would mislead.
		if err := f.checkRoot(); err != nil {
			return nil, err
		}
		out, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	if f.also == "" {
		return out, nil
	}
	more, err := listIn(f.also)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	have := map[string]bool{}
	for _, o := range out {
		have[o.Name] = true
	}
	for _, o := range more {
		if !have[o.Name] { // dir's copy wins
			out = append(out, o)
		}
	}
	sortObjects(out)
	return out, nil
}

// listIn lists the objects in dir, oldest first. A dir that isn't there
// fails with fs.ErrNotExist.
func listIn(dir string) ([]Object, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("can't read the backup folder %s (%v)", dir, err)
	}
	var out []Object
	at := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		real, evicted := fromPlaceholder(name)
		if evicted {
			name = real // iCloud Drive moved it off this Mac (icloud.go)
		}
		if _, _, ok := parseName(name); !ok || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// An evicted snapshot's size isn't known here (the placeholder's
		// own is a few hundred bytes): 0. The file itself wins over its
		// placeholder when both are there.
		o := Object{Name: name, Size: info.Size(), Modified: info.ModTime()}
		if evicted {
			o.Size = 0
		}
		if i, ok := at[name]; ok {
			if !evicted {
				out[i] = o
			}
			continue
		}
		at[name] = len(out)
		out = append(out, o)
	}
	sortObjects(out)
	return out, nil
}

func (f *folder) Delete(ctx context.Context, name string) error {
	if _, err := f.path(name); err != nil {
		return err
	}
	for _, dir := range []string{f.dir, f.also} {
		if dir == "" {
			continue
		}
		for _, n := range []string{name, placeholderOf(name)} {
			if err := os.Remove(filepath.Join(dir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// ctxReader stops a copy when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// The folder Mirrin keeps in iCloud Drive. Backups set up before the rename
// went to "AntBot Backups", and a set of words whose backups are there
// keeps going there: the Recovery Kits printed then name it, a machine
// standing by watches it for handover markers, and another Mac that hasn't
// seen it yet may have started "Mirrin Backups" meanwhile. It is never
// moved.
const (
	icloudFolder       = "Mirrin Backups"
	legacyICloudFolder = "AntBot Backups" // rename:keep
)

// ICloudDrivePath is Mirrin's folder in this Mac's iCloud Drive.
func ICloudDrivePath() (string, error) { return icloudPath("") }

// icloudPath is the folder in this Mac's iCloud Drive that keeps the
// backups of namespace ns (icloudFolderIn).
func icloudPath(ns string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("iCloud Drive is only on a Mac here; back up to a folder instead: mirrin backup target folder <path>")
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return icloudPathIn(user, ns)
}

// icloudPathIn finds iCloud Drive under a user's home folder, and in it the
// folder that keeps the backups of namespace ns.
func icloudPathIn(user, ns string) (string, error) {
	drive := icloudDriveIn(user)
	if st, err := os.Stat(drive); err != nil || !st.IsDir() {
		return "", errors.New("iCloud Drive isn't turned on for this Mac. Turn it on in System Settings › your name › iCloud › iCloud Drive, or back up to a folder: mirrin backup target folder <path>")
	}
	return filepath.Join(drive, icloudFolderIn(drive, ns)), nil
}

// icloudDriveIn is where iCloud Drive is under a user's home folder.
func icloudDriveIn(user string) string {
	return filepath.Join(user, "Library", "Mobile Documents", "com~apple~CloudDocs")
}

// icloudFolderIn is the name of the folder in the iCloud Drive at drive that
// keeps the backups of namespace ns: the one from before the rename while
// it holds them, else Mirrin's. Without a namespace, the one from before
// the rename while Mirrin's isn't there.
func icloudFolderIn(drive, ns string) string {
	if ns != "" {
		if holdsBackups(filepath.Join(drive, legacyICloudFolder, ns)) {
			return legacyICloudFolder
		}
		return icloudFolder
	}
	if !exists(filepath.Join(drive, icloudFolder)) && exists(filepath.Join(drive, legacyICloudFolder)) {
		return legacyICloudFolder
	}
	return icloudFolder
}

// otherICloudFolder is the other of iCloud Drive's two folders.
func otherICloudFolder(name string) string {
	if name == legacyICloudFolder {
		return icloudFolder
	}
	return legacyICloudFolder
}

// holdsBackups reports whether dir holds a backup or a handover marker.
func holdsBackups(dir string) bool {
	objs, err := listIn(dir)
	return err == nil && len(objs) > 0
}

// icloudWhere is the folder in this Mac's iCloud Drive that keeps the
// backups of namespace ns ("" for any), in the owner's words: "iCloud
// Drive › Mirrin Backups".
func icloudWhere(ns string) string {
	name := icloudFolder
	if p, err := icloudPath(ns); err == nil {
		name = filepath.Base(p)
	}
	return "iCloud Drive › " + name
}

// expandHome resolves a leading ~ in a folder the owner typed.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if user, err := os.UserHomeDir(); err == nil {
			return filepath.Join(user, p[1:])
		}
	}
	return p
}
