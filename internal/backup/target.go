package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Target is where snapshots are kept. It only ever sees ciphertext and
// object names that carry a time and nothing else. Folder and iCloud Drive
// and S3-compatible buckets implement it here; Mirrin Cloud implements the
// same four calls in internal/cloud/backuptarget, which this package never
// imports (OpenCloud).
type Target interface {
	// Put stores size bytes from r as name, replacing nothing: a name is new.
	Put(ctx context.Context, name string, r io.Reader, size int64) error
	// Get opens a stored object; ErrNotFound when there is none.
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	// List returns the snapshots and handover markers, oldest first.
	List(ctx context.Context) ([]Object, error)
	// Delete removes an object.
	Delete(ctx context.Context, name string) error
	// String says where, in words the owner recognises.
	String() string
}

// Object is one stored snapshot or marker.
type Object struct {
	Name     string
	Size     int64
	Modified time.Time
}

// ErrNotFound is returned by Get for a name that isn't stored.
var ErrNotFound = errors.New("not found")

// Object names: snap-20260927T033000Z-1a2b3c4d.age and
// handover-20260927T090000Z-5e6f7a8b.age. No host or twin name.
const nameStamp = "20060102T150405Z"

var reName = regexp.MustCompile(`^(snap|handover)-(\d{8}T\d{6}Z)-([0-9a-f]{8})\.age$`)

func newName(kind string, t time.Time) string {
	var r [4]byte
	_, _ = rand.Read(r[:])
	return fmt.Sprintf("%s-%s-%s.age", kind, t.UTC().Format(nameStamp), hex.EncodeToString(r[:]))
}

// parseName returns an object's kind ("snap" or "handover") and the time in
// its name, which is only a hint: the authenticated time is in the manifest.
func parseName(name string) (kind string, at time.Time, ok bool) {
	m := reName.FindStringSubmatch(name)
	if m == nil {
		return "", time.Time{}, false
	}
	at, err := time.Parse(nameStamp, m[2])
	if err != nil {
		return "", time.Time{}, false
	}
	return m[1], at, true
}

// ValidName reports whether name is an object name a target keeps: a
// snapshot or a handover marker.
func ValidName(name string) bool {
	_, _, ok := parseName(name)
	return ok
}

// IsSnapshot reports whether an object name is a snapshot.
func IsSnapshot(name string) bool {
	kind, _, ok := parseName(name)
	return ok && kind == "snap"
}

// sortObjects orders objects by the time in their names, oldest first.
func sortObjects(objs []Object) {
	sort.SliceStable(objs, func(i, j int) bool {
		_, a, _ := parseName(objs[i].Name)
		_, b, _ := parseName(objs[j].Name)
		if a.Equal(b) {
			return objs[i].Name < objs[j].Name
		}
		return a.Before(b)
	})
}

// OpenTarget opens the target the settings name, in the backup's own
// namespace folder (ns), so backups made with other words never mix.
func OpenTarget(s config.Backup, ns string) (Target, error) {
	switch s.Target {
	case "", TargetICloud:
		base, err := icloudPath(ns)
		if err != nil {
			return nil, err
		}
		return icloudTarget(base, ns), nil
	case TargetFolder:
		if s.Path == "" {
			return nil, errors.New("backup.path is empty; run `mirrin backup target folder <path>`")
		}
		return nsFolder(expandHome(s.Path), ns, ""), nil
	case TargetS3:
		return openS3(s, ns)
	case TargetCloud:
		if OpenCloud == nil {
			return nil, errors.New("backup.target is cloud, which this program can't reach; run `mirrin backup target` to choose another place")
		}
		return OpenCloud(ns)
	}
	return nil, fmt.Errorf("backup.target %q isn't one Mirrin knows (icloud, folder or s3)", s.Target)
}

// Targets Mirrin knows. TargetCloud is the optional paid service.
const (
	TargetICloud = "icloud"
	TargetFolder = "folder"
	TargetS3     = "s3"
	TargetCloud  = "cloud"
)

// OpenCloud opens the paid service's target for the backups of namespace ns
// on this machine's link. This package can't see that service's client
// (only the wiring may, internal/cloud/boundary_test.go), so cmd/mirrin sets
// it; nil means the program can't reach it.
var OpenCloud func(ns string) (Target, error)

// ErrWritesClosed is wrapped by a target that keeps the backups there but
// takes no new ones, such as the paid service once payment has lapsed. The
// engine then says which free places backups can go instead.
var ErrWritesClosed = errors.New("no new backups can be saved there")

// ErrNotKept is wrapped by a target that won't take a backup because it
// keeps no such one, such as the paid service for a name its retention
// removed before. Migrate reports it and carries on.
var ErrNotKept = errors.New("not kept there")

// Keeper is a target that keeps only some backups, whatever it is sent,
// and says which in Keeps, such as "7 daily, 4 weekly and 6 monthly
// backups".
type Keeper interface {
	Keeps() string
}

// FreeTargets says where backups can go for free, for a target that takes
// no more.
const FreeTargets = "Backups can go to a folder (`mirrin backup target folder <path>`), iCloud Drive (`mirrin backup target icloud`) or your own S3 bucket (`mirrin backup target s3 s3://<bucket>/<folder>`), all free; `mirrin backup migrate` copies the ones already made"

// icloudTarget is the target for namespace ns in base, a folder in iCloud
// Drive. iCloud Drive itself must be there; the folder in it is made as
// needed, so deleting it in Finder doesn't stop backups. Backups of ns in
// the drive's other folder (from before the rename, or made by a Mac that
// hadn't seen that one yet) are listed and read too.
func icloudTarget(base, ns string) Target {
	f := nsFolder(base, ns, "iCloud Drive › "+filepath.Base(base)).(*folder)
	f.root = filepath.Dir(base)
	f.missing = "iCloud Drive isn't there any more; turn it on in System Settings › your name › iCloud › iCloud Drive"
	if ns != "" {
		if other := filepath.Join(f.root, otherICloudFolder(filepath.Base(base)), ns); holdsBackups(other) {
			f.also = other
		}
	}
	return f
}
