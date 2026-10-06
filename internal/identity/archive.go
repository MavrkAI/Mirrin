package identity

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Size limits for archive entries. An entry over its limit stops the import
// before anything changes, rather than arriving cut short.
var (
	maxFileSize int64 = 64 << 20
	maxDBSize   int64 = 64 << 30
)

// importable reports whether an archive path is something a twin is made of:
// its settings, memory, personas, and the protocol files Mirrin reads
// (protocols/*.yaml, and in each pack its *.yaml, protocols/*.yaml and
// personas/*.yaml, plus its README and LICENSE). Everything else (custom
// tools, remote.yaml, tokens, .git folders, scripts) is left out on export
// and skipped on import.
//
// That keeps what an import writes to the twin's own files, but an archive
// is still personal input, not something to take from others: its settings
// can add MCP servers and commands and change where requests go.
func importable(name string) bool {
	switch name {
	case "manifest.yaml", "config.yaml", "memory.yaml", "data/memory.db":
		return true
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if strings.HasPrefix(p, ".") {
			return false // .git, a pack being installed, hidden files
		}
	}
	file := parts[len(parts)-1]
	switch {
	case len(parts) == 2 && (parts[0] == "personas" || parts[0] == "protocols"):
		return isYAML(file)
	case len(parts) == 4 && parts[0] == "protocols" && parts[1] == "packs":
		return isYAML(file) || isPackDoc(file)
	case len(parts) == 5 && parts[0] == "protocols" && parts[1] == "packs":
		return (parts[3] == "protocols" || parts[3] == "personas") && isYAML(file)
	}
	return false
}

func isYAML(file string) bool {
	ext := strings.ToLower(path.Ext(file))
	return ext == ".yaml" || ext == ".yml"
}

// isPackDoc reports whether a file at a pack's root is its README or LICENSE.
func isPackDoc(file string) bool {
	ext := strings.ToLower(path.Ext(file))
	base := strings.ToUpper(strings.TrimSuffix(file, path.Ext(file)))
	return (base == "README" || base == "LICENSE") && (ext == "" || ext == ".md" || ext == ".txt")
}

// sizeLimit is the most an archive entry may hold.
func sizeLimit(name string) int64 {
	if name == "data/memory.db" {
		return maxDBSize
	}
	return maxFileSize
}

// checkProtocolsDir refuses a protocols folder that is the twin's home or data
// folder, or holds one of them. Its YAML files travel as they are, so the
// twin's config.yaml or remote.yaml would travel as protocols, secrets and all.
func checkProtocolsDir(protocolsDir, home, dataDir string) error {
	pd := realPath(protocolsDir)
	for _, d := range []string{home, dataDir} {
		if _, inside := under(realPath(d), pd); inside {
			return fmt.Errorf("the protocols folder (%s) also holds the twin's own files; point protocols_dir in config.yaml at a folder of its own", protocolsDir)
		}
	}
	return nil
}

// realPath is p made absolute with every link resolved, as far as it exists.
func realPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// safeName reports whether an archive path stays inside the twin's home.
func safeName(name string) bool {
	return name != "" && !strings.Contains(name, `\`) && path.Clean(name) == name &&
		!path.IsAbs(name) && filepath.IsLocal(filepath.FromSlash(name))
}

// entry is one file in an archive: bytes, or a file on disk streamed in.
type entry struct {
	name string
	data []byte
	path string
}

// fits checks the entry against the limit import applies, so an export never
// makes an archive that import then refuses.
func (e entry) fits() error {
	size := int64(len(e.data))
	if e.path != "" {
		st, err := os.Stat(e.path)
		if err != nil {
			return err
		}
		size = st.Size()
	}
	if limit := sizeLimit(e.name); size > limit {
		where := e.name
		if e.path != "" && e.name != "data/memory.db" {
			where = e.path
		}
		return fmt.Errorf("%s is too big to move (%d MB; the most is %d MB)", where, size>>20, limit>>20)
	}
	return nil
}

func (e entry) write(tw *tar.Writer) error {
	hdr := &tar.Header{Name: e.name, Mode: 0o600, Typeflag: tar.TypeReg, ModTime: time.Now()}
	if e.path == "" {
		hdr.Size = int64(len(e.data))
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(e.data)
		return err
	}
	f, err := os.Open(e.path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	hdr.Size, hdr.ModTime = st.Size(), st.ModTime()
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, st.Size())
	return err
}

// writeArchive writes the manifest and entries as a tar.gz to w, including
// the final flushes, whose errors (a full disk) are errors.
func writeArchive(w io.Writer, m Manifest, entries []entry) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	mb, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	for _, e := range append([]entry{{name: "manifest.yaml", data: mb}}, entries...) {
		if err := e.write(tw); err != nil {
			return fmt.Errorf("%s: %w", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// writeArchiveFile writes the archive next to out and renames it into place,
// so a failed export never leaves a partial file.
func writeArchiveFile(out string, m Manifest, entries []entry) (err error) {
	f, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	// CreateTemp makes the file 0600, so only the owner can read the archive.
	if err = writeArchive(f, m, entries); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}

// listFiles returns the files a twin reads in dir (personas, or protocols and
// their packs) as entries named prefix/…. It looks only where the loaders
// look, so a protocols_dir pointing at a big folder sends nothing else. A
// file linked from elsewhere travels, as the loaders read it; a linked folder
// inside does not, as they skip it. Inside a pack no link travels at all,
// file or folder: the loaders refuse them there (a cloned pack could point
// one at secrets.env). skip is the archive being written.
func listFiles(dir, prefix, skip string) ([]entry, error) {
	dirs := []string{""}
	if prefix == "protocols" {
		packs, err := os.ReadDir(filepath.Join(dir, "packs"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, p := range packs {
			if p.IsDir() {
				dirs = append(dirs, path.Join("packs", p.Name()), path.Join("packs", p.Name(), "protocols"), path.Join("packs", p.Name(), "personas"))
			}
		}
	}
	var out []entry
	for _, rel := range dirs {
		d := filepath.Join(dir, filepath.FromSlash(rel))
		inPack := rel != ""
		if st, err := os.Lstat(d); inPack && err == nil && st.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		es, err := os.ReadDir(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			name := path.Join(prefix, rel, e.Name())
			p := filepath.Join(d, e.Name())
			if e.IsDir() || !importable(name) || sameFile(p, skip) || inPack && e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
				continue
			}
			out = append(out, entry{name: name, path: p})
		}
	}
	return out, nil
}

func sameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && os.SameFile(sa, sb)
}

// unpack stages the archive's importable files under stage. It returns their
// names and the names it skipped. Anything unsafe or damaged is an error.
func unpack(r io.Reader, stage string) (names, skipped []string, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, errNotArchive
	}
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("the archive is damaged (%v); nothing was imported", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		name := hdr.Name
		if !safeName(name) {
			return nil, nil, fmt.Errorf("the archive has an unsafe path (%q); nothing was imported", name)
		}
		if !hdr.FileInfo().Mode().IsRegular() || !importable(name) {
			skipped = append(skipped, name)
			continue
		}
		if limit := sizeLimit(name); hdr.Size > limit {
			return nil, nil, fmt.Errorf("%s in the archive is too big (%d MB); nothing was imported", name, hdr.Size>>20)
		}
		if err := stageFile(filepath.Join(stage, filepath.FromSlash(name)), tr, hdr.Size); err != nil {
			return nil, nil, fmt.Errorf("the archive is cut short or damaged at %s (%v); nothing was imported", name, err)
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	// Read to the end so gzip checks its checksum.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return nil, nil, fmt.Errorf("the archive is damaged (%v); nothing was imported", err)
	}
	return names, skipped, nil
}

func stageFile(dest string, r io.Reader, size int64) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.CopyN(f, r, size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeFileAtomic replaces dest with data by writing a temporary file beside
// it and renaming it into place, so dest is never left half written.
func writeFileAtomic(dest string, data []byte) error {
	return replaceFile(dest, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// copyFileAtomic replaces dest with a copy of src.
func copyFileAtomic(dest, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return replaceFile(dest, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

func replaceFile(dest string, fill func(io.Writer) error) (err error) {
	// Write through a link (a config kept in a dotfiles repo) rather than
	// swapping the link for a copy.
	if st, err := os.Lstat(dest); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		if real, err := filepath.EvalSymlinks(dest); err == nil {
			dest = real
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err = fill(f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
