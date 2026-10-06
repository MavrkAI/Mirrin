package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"filippo.io/age"

	"github.com/MavrkAI/Mirrin/internal/identity"
)

// Manifest is the first entry of every snapshot: the identity export's
// manifest, format 2, kind "backup".
type Manifest = identity.Manifest

// ManifestFormat is the manifest format backups write and read.
const ManifestFormat = 2

// maxManifest bounds the manifest a reader accepts.
const maxManifest = 8 << 20

var (
	// ErrWrongWords means the words given don't open a snapshot.
	ErrWrongWords = errors.New("these words don't open this backup; check them against your Recovery Kit")
	// ErrDamaged means a snapshot failed its checks. Nothing is restored from it.
	ErrDamaged = errors.New("snapshot damaged")
)

func damaged(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrDamaged, fmt.Sprintf(format, a...))
}

// writeArchive writes m and the files staged under stage (at their archive
// paths) to w: a tar with manifest.json first, gzipped, encrypted with age
// to rcpt. Anyone with the words can open it with the stock age tool.
func writeArchive(w io.Writer, rcpt age.Recipient, m Manifest, stage string, modes map[string]os.FileMode) error {
	aw, err := age.Encrypt(w, rcpt)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(aw)
	tw := tar.NewWriter(gz)
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: nameManifest, Mode: 0o600, Size: int64(len(mb)), ModTime: m.ExportedAt, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	if _, err := tw.Write(mb); err != nil {
		return err
	}
	for _, f := range m.Contents {
		mode := int64(0o600)
		if modes[f.Path]&0o111 != 0 {
			mode = 0o700
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.Path, Mode: mode, Size: f.Size, ModTime: m.ExportedAt, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		src, err := os.Open(filepath.Join(stage, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		n, err := io.Copy(tw, src)
		src.Close()
		if err != nil {
			return err
		}
		if n != f.Size {
			return fmt.Errorf("%s changed while it was being backed up", f.Path)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return aw.Close()
}

// archive reads a snapshot: the manifest first, then each file, checked.
type archive struct {
	plain io.Reader // decrypted
	gz    *gzip.Reader
	tr    *tar.Reader
	m     Manifest
	want  map[string]identity.ManifestFile
}

// openArchive decrypts r with ids and reads the manifest. Only as much of
// the snapshot is read as the manifest needs, normally the first 64 KiB
// chunk, and age authenticates every chunk it returns, so the date and
// sequence number are the ones the twin wrote.
func openArchive(r io.Reader, ids []age.Identity) (*archive, error) {
	plain, err := age.Decrypt(r, ids...)
	if err != nil {
		var none *age.NoIdentityMatchError
		if errors.As(err, &none) {
			return nil, ErrWrongWords
		}
		return nil, damaged("it doesn't decrypt (%v)", err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return nil, damaged("%v", err)
	}
	a := &archive{plain: plain, gz: gz, tr: tar.NewReader(gz)}
	hdr, err := a.tr.Next()
	if err != nil {
		return nil, damaged("%v", err)
	}
	if hdr.Name != nameManifest || hdr.Typeflag != tar.TypeReg || hdr.Size > maxManifest {
		return nil, damaged("it doesn't start with a manifest")
	}
	mb, err := io.ReadAll(io.LimitReader(a.tr, maxManifest))
	if err != nil {
		return nil, damaged("%v", err)
	}
	if err := json.Unmarshal(mb, &a.m); err != nil {
		return nil, damaged("the manifest doesn't read (%v)", err)
	}
	if a.m.Kind != identity.KindBackup {
		return nil, damaged("it isn't a Mirrin backup")
	}
	if a.m.Format > ManifestFormat {
		return nil, errors.New("this backup comes from a newer Mirrin; update Mirrin here, then try again")
	}
	if a.m.Format < ManifestFormat {
		return nil, damaged("unknown manifest format %d", a.m.Format)
	}
	if a.want, err = checkEntries(a.m.Contents); err != nil {
		return nil, damaged("%v", err)
	}
	return a, nil
}

// checkEntries is the rule every file a manifest lists must pass: a safe
// name, listed once, not the manifest itself, with a size and a SHA-256.
// The reader applies it before trusting a manifest, and the writer before
// it encrypts one, so Mirrin never saves a snapshot a restore would refuse.
func checkEntries(files []identity.ManifestFile) (map[string]identity.ManifestFile, error) {
	want := make(map[string]identity.ManifestFile, len(files))
	for _, f := range files {
		if !safeName(f.Path) || !utf8.ValidString(f.Path) || f.Path == nameManifest {
			return nil, fmt.Errorf("it lists an unsafe path (%q)", f.Path)
		}
		if _, dup := want[f.Path]; dup {
			return nil, fmt.Errorf("it lists %s twice", f.Path)
		}
		if f.Size < 0 || len(f.SHA256) != 64 {
			return nil, fmt.Errorf("its entry for %s is malformed", f.Path)
		}
		want[f.Path] = f
	}
	return want, nil
}

// each hands every file to fn after checking its name, then checks its size
// and SHA-256 against the manifest, then reads to the end, where age and
// gzip check that nothing was cut off. fn returns the writer for the file,
// or nil to discard it.
func (a *archive) each(fn func(f identity.ManifestFile, exec bool) (io.WriteCloser, error)) error {
	seen := map[string]bool{}
	for {
		hdr, err := a.tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return damaged("%v", err)
		}
		if !safeName(hdr.Name) {
			return damaged("it holds an unsafe path (%q)", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg {
			return damaged("%s isn't a plain file", hdr.Name)
		}
		f, ok := a.want[hdr.Name]
		if !ok || seen[hdr.Name] {
			return damaged("%s isn't in its manifest", hdr.Name)
		}
		seen[hdr.Name] = true
		if hdr.Size != f.Size {
			return damaged("%s has the wrong size", f.Path)
		}
		w, err := fn(f, hdr.Mode&0o111 != 0)
		if err != nil {
			return err
		}
		h := sha256.New()
		var dst io.Writer = h
		if w != nil {
			dst = io.MultiWriter(w, h)
		}
		n, err := io.Copy(dst, a.tr)
		if w != nil {
			if cerr := w.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrPermission) || isDiskError(err) {
				return err
			}
			return damaged("%s: %v", f.Path, err)
		}
		if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			return damaged("%s doesn't match its checksum", f.Path)
		}
	}
	for p := range a.want {
		if !seen[p] {
			return damaged("%s is missing", p)
		}
	}
	// The rest of the gzip stream and of the age payload: their checksums
	// and final chunk are only checked at the very end.
	if _, err := io.Copy(io.Discard, a.gz); err != nil {
		return damaged("%v", err)
	}
	if _, err := io.Copy(io.Discard, a.plain); err != nil {
		return damaged("%v", err)
	}
	return nil
}

// isDiskError is a write failure on this machine, not damage in the snapshot.
func isDiskError(err error) bool {
	var pe *os.PathError
	return errors.As(err, &pe)
}

// safeName reports whether an archive path stays inside the twin's home:
// relative, slash-separated, clean, with no ".." and no drive letters.
func safeName(name string) bool {
	return name != "" && !strings.Contains(name, `\`) && !strings.Contains(name, ":") &&
		path.Clean(name) == name && !path.IsAbs(name) && filepath.IsLocal(filepath.FromSlash(name)) &&
		!strings.HasPrefix(name, "../") && name != ".."
}

// restorable reports whether a file can go in a snapshot under name and
// come back on any machine Mirrin runs on: safeName (so no ':' or '\',
// which Finder and Linux allow), valid UTF-8 (the manifest is JSON, which
// would change any other bytes), and no path element Windows keeps for
// devices, whose filepath.IsLocal refuses them.
func restorable(name string) bool {
	if !safeName(name) || !utf8.ValidString(name) {
		return false
	}
	for _, el := range strings.Split(name, "/") {
		if windowsDevice(el) {
			return false
		}
	}
	return true
}

// windowsDevice reports whether a path element is a device name on
// Windows (CON, PRN, AUX, NUL, COM1-9, LPT1-9), with or without an
// extension. Older Windows reserve "con.txt" too, so this does.
func windowsDevice(el string) bool {
	base, _, _ := strings.Cut(el, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9'
}

// ReadManifest decrypts just enough of a snapshot to read its manifest.
func ReadManifest(r io.Reader, ids []age.Identity) (Manifest, error) {
	a, err := openArchive(r, ids)
	if err != nil {
		return Manifest{}, err
	}
	return a.m, nil
}

// VerifyArchive decrypts a whole snapshot and checks every file.
func VerifyArchive(r io.Reader, ids []age.Identity) (Manifest, error) {
	a, err := openArchive(r, ids)
	if err != nil {
		return Manifest{}, err
	}
	err = a.each(func(identity.ManifestFile, bool) (io.WriteCloser, error) { return nil, nil })
	return a.m, err
}

// extractArchive decrypts a snapshot into dir, checking every file. dir
// should be empty and private; on error it may hold part of the snapshot.
// With forWindows, names Windows refuses are changed (winnames.go), and
// the changes are returned.
func extractArchive(r io.Reader, ids []age.Identity, dir string, forWindows bool) (Manifest, []string, error) {
	a, err := openArchive(r, ids)
	if err != nil {
		return Manifest{}, nil, err
	}
	paths := make([]string, 0, len(a.m.Contents))
	for _, f := range a.m.Contents {
		paths = append(paths, f.Path)
	}
	rn := newRenamer(forWindows, paths)
	err = a.each(func(f identity.ManifestFile, exec bool) (io.WriteCloser, error) {
		dest := filepath.Join(dir, filepath.FromSlash(rn.name(f.Path)))
		if !within(dir, dest) {
			return nil, damaged("it holds an unsafe path (%q)", f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return nil, err
		}
		mode := os.FileMode(0o600)
		if exec {
			mode = 0o700
		}
		return os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	})
	return a.m, rn.renamed, err
}

// hashFile returns a file's size and SHA-256.
func hashFile(p string) (int64, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return n, hex.EncodeToString(h.Sum(nil)), err
}
