// Package backup keeps end-to-end encrypted snapshots of a twin, and brings
// one back on this machine or another.
//
// A snapshot is the twin's settings (secrets included), memory, personas,
// protocols, custom tools and keys: a tar.gz whose first entry is the
// manifest, encrypted with age to a key derived from 12 words that Mirrin
// shows once and never stores. The machine keeps only the public key, so it
// can write backups but not read them; neither can wherever they are kept.
// docs/backup-format.md describes the format so any age can open it.
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"filippo.io/age"
	_ "modernc.org/sqlite" // VACUUM INTO, as memory uses

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/identity"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// ErrBusy means another backup is running on this machine.
var ErrBusy = errors.New("a backup is already running; try again in a few minutes")

// ErrStandingBy means this machine handed the twin over to another one.
var ErrStandingBy = errors.New("this machine is standing by: the twin moved to another machine. Run `mirrin backup resume` to make this one the twin again")

// lockName, in the data folder, keeps two backups from running at once.
const lockName = "backup.lock"

// Engine takes snapshots of one twin.
type Engine struct {
	Layout   Layout
	Settings config.Backup
	Target   Target
	Keep     Retention
	// WithSessions also takes the WhatsApp session.
	WithSessions bool
	// WithSignal also takes signal-cli's data folder (Layout.SignalDir),
	// as one tar under sessions/.
	WithSignal bool
	Twin       string
	HostLabel  string
	Version    string
	Now        func() time.Time
	Log        *slog.Logger
}

// NewEngine builds the engine for a loaded config.
func NewEngine(cfg *config.Config, home, version string) (*Engine, error) {
	s := cfg.Backup
	if s.Recipient == "" {
		return nil, errors.New("backups aren't set up yet; run `mirrin backup init`")
	}
	ns, err := NamespaceOf(s.RecoveryPub)
	if err != nil {
		return nil, err
	}
	t, err := OpenTarget(s, ns)
	if err != nil {
		return nil, err
	}
	return &Engine{
		Layout:       LayoutOf(cfg, home),
		Settings:     s,
		Target:       t,
		Keep:         DefaultRetention,
		WithSessions: s.Sessions,
		WithSignal:   s.Signal,
		Twin:         cfg.Name,
		HostLabel:    HostLabel(),
		Version:      version,
	}, nil
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// lock takes the backup lock for this twin.
func (e *Engine) lock() (func(), error) {
	if err := os.MkdirAll(e.Layout.DataDir, 0o700); err != nil {
		return nil, err
	}
	f, err := lockFile(filepath.Join(e.Layout.DataDir, lockName))
	if err != nil {
		return nil, err
	}
	return func() { f.Close() }, nil
}

// Run takes a snapshot, stores it, reads it back to check the target holds
// exactly what was written, and prunes old snapshots. The result is recorded
// for the health check either way.
func (e *Engine) Run(ctx context.Context) (Manifest, error) {
	unlock, err := e.lock()
	if err != nil {
		return Manifest{}, err
	}
	defer unlock()
	removeStale(e.Layout.Home)
	st, err := LoadState(e.Layout.DataDir)
	if err != nil {
		return Manifest{}, err
	}
	if st.Standby != nil {
		return Manifest{}, ErrStandingBy
	}
	start := e.now()
	if err := e.checkGone(ctx, st); err != nil { // gone.go
		_ = UpdateState(e.Layout.DataDir, func(s *State) { s.LastAttempt, s.LastError = start, plainError(err) })
		return Manifest{}, err
	}
	m, name, size, sum, badNames, runErr := e.snapshot(ctx, st.Seq+1)
	if len(badNames) > 0 {
		e.log().Warn("backup left files out: their names can't be restored on every computer; rename them without any of "+AwkwardChars, "files", strings.Join(badNames, ", "))
	}
	err = UpdateState(e.Layout.DataDir, func(s *State) {
		s.LastAttempt = start
		if s.Since.IsZero() {
			s.Since = start
		}
		if runErr != nil {
			s.LastError = plainError(runErr)
			return
		}
		s.Seq, s.LastGood, s.LastError, s.LastName, s.LastSize, s.LastSum = m.Seq, start, "", name, size, sum
		s.LeftOut = badNames
	})
	if runErr != nil {
		return Manifest{}, runErr
	}
	if err != nil {
		return m, err
	}
	if _, err := e.prune(ctx, name); err != nil {
		e.log().Warn("backup prune", "err", err)
	}
	return m, nil
}

// snapshot builds, encrypts, stores and reads back one snapshot. It also
// returns the files it left out because their names couldn't be restored.
func (e *Engine) snapshot(ctx context.Context, seq int64) (Manifest, string, int64, string, []string, error) {
	rcpt, err := ParseRecipient(e.Settings.Recipient)
	if err != nil {
		return Manifest{}, "", 0, "", nil, err
	}
	standby, err := standbyIdentity(e.Layout.DataDir)
	if err != nil {
		return Manifest{}, "", 0, "", nil, err
	}
	items, excluded, badNames, err := collect(e.Layout, e.WithSessions)
	if err != nil {
		return Manifest{}, "", 0, "", nil, err
	}
	if e.WithSignal { // signal.go
		items = append(items, signalItems(e.Layout)...)
	}
	fail := func(err error) (Manifest, string, int64, string, []string, error) {
		return Manifest{}, "", 0, "", badNames, err
	}
	// Plain copies are staged in the twin's own private home (not the data
	// folder, which may be synced) and removed when the run ends.
	stage, err := os.MkdirTemp(e.Layout.Home, ".backup-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(stage)
	now := e.now().UTC().Truncate(time.Second)
	user, _ := os.UserHomeDir()
	m := Manifest{
		Format: ManifestFormat, Kind: identity.KindBackup, WithSecrets: true,
		ExportedAt: now, Seq: seq, Twin: e.Twin, HostLabel: e.HostLabel, Version: e.Version, OS: runtime.GOOS,
		Excluded: excluded, Home: e.Layout.Home, UserHome: user, HandoverTo: standby.Recipient().String(),
		Zone: config.LocalTimezone(),
	}
	modes := map[string]os.FileMode{}
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		dst := filepath.Join(stage, "files", filepath.FromSlash(it.name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fail(err)
		}
		switch {
		case it.db:
			err = vacuumInto(ctx, it.src, dst)
		case it.tree:
			// Signal's session is extra: a folder that can't be copied now
			// leaves it out of this snapshot, not the backup (signal.go).
			if err = tarTree(ctx, it.src, dst, stage); err != nil && ctx.Err() == nil {
				_ = os.Remove(dst)
				e.log().Warn("backup left Signal's session out of this snapshot", "folder", it.src, "err", err)
				continue
			}
		default:
			err = copyFile(it.src, dst)
		}
		if errors.Is(err, fs.ErrNotExist) && !it.db {
			continue // removed since the list was made (a tool deleted meanwhile)
		}
		if err != nil {
			return fail(fmt.Errorf("couldn't copy %s (%v)", it.name, err))
		}
		// The check a restore makes, and the daily memory copies make before
		// they're kept: a damaged memory never becomes the newest snapshot,
		// where retention could push the good ones out.
		if it.name == nameMemory {
			if err := quickCheck(dst); err != nil {
				return fail(fmt.Errorf("nothing was saved, because the memory doesn't pass SQLite's check (%v); `mirrin memory restore` brings back the last good copy", err))
			}
		}
		size, sum, err := hashFile(dst)
		if err != nil {
			return fail(err)
		}
		if st, err := os.Stat(it.src); err == nil {
			modes[it.name] = st.Mode()
		}
		m.Contents = append(m.Contents, identity.ManifestFile{Path: it.name, Size: size, SHA256: sum})
	}
	// The reader's rule, before anything is encrypted: a snapshot a restore
	// would call damaged is never saved.
	if _, err := checkEntries(m.Contents); err != nil {
		return fail(fmt.Errorf("nothing was saved, because a restore couldn't open this backup: %v", err))
	}
	for _, c := range m.Contents {
		if !restorable(c.Path) {
			return fail(fmt.Errorf("nothing was saved, because %q couldn't be restored on every computer", c.Path))
		}
	}
	out := filepath.Join(stage, "snapshot.age")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	h := sha256.New()
	if err := writeArchive(io.MultiWriter(f, h), rcpt, m, filepath.Join(stage, "files"), modes); err != nil {
		return fail(err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return fail(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	name := newName("snap", now)
	if err := e.Target.Put(ctx, name, f, size); err != nil {
		var gone *notThereError
		if errors.As(err, &gone) {
			return fail(err) // says what to do in its own words
		}
		if errors.Is(err, ErrWritesClosed) {
			return fail(fmt.Errorf("the backup wasn't saved: %v. %s", err, FreeTargets))
		}
		return fail(fmt.Errorf("couldn't save the backup to %s (%v)", e.Target, err))
	}
	// Read it back: this machine can't decrypt its own backups (it has no
	// words), but it can check the target holds exactly what was written.
	if err := readBack(ctx, e.Target, name, size, sum); err != nil {
		_ = e.Target.Delete(ctx, name)
		return fail(fmt.Errorf("the backup saved to %s didn't read back intact (%v)", e.Target, err))
	}
	return m, name, size, sum, badNames, nil
}

func readBack(ctx context.Context, t Target, name string, size int64, sum string) error {
	rc, err := t.Get(ctx, name)
	if err != nil {
		return err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != sum {
		return errors.New("its bytes differ")
	}
	return nil
}

// vacuumInto writes a consistent copy of the SQLite database at src to dst
// while the twin keeps using it (a variable so tests can hand over a copy
// that is damaged in ways VACUUM INTO itself wouldn't catch).
var vacuumInto = vacuumIntoFile

func vacuumIntoFile(ctx context.Context, src, dst string) error {
	// Opened read-write so SQLite can use the write-ahead log the twin keeps
	// open; VACUUM INTO only reads it.
	db, err := sql.Open("sqlite", memory.FileURI(src)+"?mode=rw&_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return err
	}
	return os.Chmod(dst, 0o600)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Listed is one snapshot as the words see it.
type Listed struct {
	Object
	Manifest Manifest
	// Err is why it couldn't be read: ErrWrongWords or damage.
	Err error
}

// List reads the manifest of every snapshot in t, newest first by the
// authenticated time. Only the start of each snapshot is decrypted.
func List(ctx context.Context, t Target, p Phrase) ([]Listed, error) {
	objs, err := t.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := p.Identities()
	var out []Listed
	for i := len(objs) - 1; i >= 0; i-- {
		o := objs[i]
		if !IsSnapshot(o.Name) {
			continue
		}
		l := Listed{Object: o}
		rc, err := t.Get(ctx, o.Name)
		if err != nil {
			l.Err = err
		} else {
			l.Manifest, l.Err = ReadManifest(rc, ids)
			rc.Close()
		}
		out = append(out, l)
	}
	sortListed(out)
	return out, nil
}

// sortListed puts readable snapshots first, newest (by manifest) first.
func sortListed(ls []Listed) {
	for i := 1; i < len(ls); i++ {
		for j := i; j > 0 && listedBefore(ls[j], ls[j-1]); j-- {
			ls[j], ls[j-1] = ls[j-1], ls[j]
		}
	}
}

func listedBefore(a, b Listed) bool {
	if (a.Err == nil) != (b.Err == nil) {
		return a.Err == nil
	}
	if !a.Manifest.ExportedAt.Equal(b.Manifest.ExportedAt) {
		return a.Manifest.ExportedAt.After(b.Manifest.ExportedAt)
	}
	return a.Manifest.Seq > b.Manifest.Seq
}

// Verify decrypts a whole snapshot and checks every file in it.
func Verify(ctx context.Context, t Target, name string, p Phrase) (Manifest, error) {
	rc, err := t.Get(ctx, name)
	if err != nil {
		return Manifest{}, err
	}
	defer rc.Close()
	return VerifyArchive(rc, p.Identities())
}

// Prune applies retention now.
func (e *Engine) Prune(ctx context.Context) ([]string, error) {
	st, _ := LoadState(e.Layout.DataDir)
	return e.prune(ctx, st.LastName)
}

func (e *Engine) prune(ctx context.Context, newestVerified string) ([]string, error) {
	objs, err := e.Target.List(ctx)
	if err != nil {
		return nil, err
	}
	var snaps []Object
	for _, o := range objs {
		if IsSnapshot(o.Name) {
			snaps = append(snaps, o)
		}
	}
	_, drop := e.Keep.Apply(snaps, newestVerified, e.now())
	var gone []string
	for _, o := range drop {
		if err := e.Target.Delete(ctx, o.Name); err != nil {
			return gone, err
		}
		gone = append(gone, o.Name)
	}
	return gone, nil
}

// HostLabel names this machine the way its owner does: the Mac's computer
// name ("Akshay's MacBook Pro"), else the host name.
func HostLabel() string {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "scutil", "--get", "ComputerName").Output(); err == nil {
			if s := strings.TrimSpace(string(bytes.ToValidUTF8(out, nil))); s != "" {
				return s
			}
		}
	}
	h, _ := os.Hostname()
	return h
}

// plainError is an error as the health check shows it: one line.
func plainError(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// standbyKeyFile, in the data folder, is this machine's own age key. Its
// public half goes in every manifest, so a restore elsewhere can leave a
// handover marker that only this machine can read. It is never backed up.
const standbyKeyFile = "backup-standby.key"

// standbyIdentity loads this machine's standby key, making it the first time.
func standbyIdentity(dataDir string) (*age.HybridIdentity, error) {
	p := filepath.Join(dataDir, standbyKeyFile)
	if b, err := os.ReadFile(p); err == nil {
		if id, err := age.ParseHybridIdentity(strings.TrimSpace(string(b))); err == nil {
			return id, nil
		}
	}
	id, err := age.GenerateHybridIdentity()
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(p, []byte(id.String()+"\n")); err != nil {
		return nil, err
	}
	return id, nil
}

// existingStandby loads this machine's standby key without making one.
func existingStandby(dataDir string) *age.HybridIdentity {
	b, err := os.ReadFile(filepath.Join(dataDir, standbyKeyFile))
	if err != nil {
		return nil
	}
	id, err := age.ParseHybridIdentity(strings.TrimSpace(string(b)))
	if err != nil {
		return nil
	}
	return id
}

// removeStale deletes the staging folders of runs that never finished (the
// machine lost power mid-backup). Their plain copies must not linger: a
// fact forgotten since would survive in them. The caller holds the lock,
// so no run is using them.
func removeStale(home string) {
	stale, _ := filepath.Glob(filepath.Join(home, ".backup-*"))
	for _, d := range stale {
		if st, err := os.Lstat(d); err == nil && st.IsDir() {
			_ = os.RemoveAll(d)
		}
	}
}
