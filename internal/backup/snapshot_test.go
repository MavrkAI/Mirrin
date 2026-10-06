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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/MavrkAI/Mirrin/internal/identity"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

func TestSnapshotHoldsTheTwinAndNoMachineSecrets(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	m, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Seq != 1 || m.Kind != identity.KindBackup || !m.WithSecrets || m.Format != 2 || m.Twin != "Jeeves" || m.HostLabel != "Test Mac" {
		t.Fatalf("manifest: %+v", m)
	}
	st, _ := LoadState(tw.data)
	f, err := os.Open(snapshotPath(e, st.LastName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	id, _ := p.AgeIdentity()
	names, files := entries(t, f, id)
	if names[0] != nameManifest {
		t.Fatalf("manifest isn't first: %v", names)
	}
	// hey_maverick.onnx too: releases no longer carry it, so a backup is the
	// only way it comes back on a new machine.
	for _, want := range []string{"config.yaml", "secrets.env", "data/memory.db", "personas/butler.yaml", "protocols/morning.yaml",
		"tools/shout/tool.yaml", "tools/shout/run.sh", "google-token.json", "data/devices.json", "data/tls/acme-account.key", "tts/hey_jeeves.onnx", "tts/hey_maverick.onnx"} {
		if !slices.Contains(names, want) {
			t.Errorf("%s missing from the snapshot: %v", want, names)
		}
	}
	for _, name := range names {
		for _, never := range []string{"chrome-profile", "api.token", "cloud/", "whatsapp.db", "models/", "tts/kokoro", "tts/venv", "logs/", "remote.yaml", "node_modules", ".png", "media/", standbyKeyFile} {
			if strings.Contains(name, never) {
				t.Errorf("%s is in the snapshot", name)
			}
		}
	}
	for name, body := range files {
		for _, secret := range []string{plantedToken, plantedSafeStorage, plantedCookie, plantedDeviceKey} {
			if bytes.Contains(body, []byte(secret)) {
				t.Errorf("%s holds a machine secret", name)
			}
		}
		for _, w := range []string{strings.Join(p.Words(), " "), hex.EncodeToString(p.k[:])} {
			if bytes.Contains(body, []byte(w)) {
				t.Errorf("%s holds the words", name)
			}
		}
	}
	if !bytes.Contains(files["config.yaml"], []byte(plantedAPIKey)) {
		t.Error("the config's own secrets should travel in a backup")
	}
	for _, x := range []string{"data/chrome-profile", "data/whatsapp.db", "data/api.token", "models", "logs", "remote.yaml", "tools/shout/node_modules"} {
		if !slices.Contains(m.Excluded, x) {
			t.Errorf("excluded list lacks %s: %v", x, m.Excluded)
		}
	}
	// Every file's checksum is in the manifest.
	got := manifestOf(t, files)
	for _, c := range got.Contents {
		sum := sha256.Sum256(files[c.Path])
		if hex.EncodeToString(sum[:]) != c.SHA256 || int64(len(files[c.Path])) != c.Size {
			t.Errorf("%s doesn't match its manifest entry", c.Path)
		}
	}
	if got.HandoverTo == "" || !strings.HasPrefix(got.HandoverTo, "age1pq1") {
		t.Errorf("no standby recipient: %q", got.HandoverTo)
	}
}

func TestSessionsOnlyWhenAsked(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	e.WithSessions = true
	m, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range m.Contents {
		names = append(names, c.Path)
	}
	if !slices.Contains(names, "data/whatsapp.db") || slices.Contains(m.Excluded, "data/whatsapp.db") {
		t.Fatalf("sessions asked for but not taken: %v", names)
	}
	for _, n := range names {
		if strings.Contains(n, "chrome-profile") {
			t.Fatal("the browser profile is never a session to take")
		}
	}
}

// The stock age tool opens a snapshot with the key `mirrin backup key --age`
// prints. Without age installed, the library reads the same key text.
func TestStockAgeDecryptsASnapshot(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := LoadState(tw.data)
	snap := snapshotPath(e, st.LastName)
	id, _ := p.AgeIdentity()
	keyText := id.String() + "\n"
	var plain []byte
	bin, err := exec.LookPath("age")
	if env := os.Getenv("AGE_BIN"); env != "" { // a stock age that isn't on PATH
		bin, err = env, nil
	}
	if err == nil {
		keyFile := filepath.Join(t.TempDir(), "key.txt")
		if err := os.WriteFile(keyFile, []byte(keyText), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(bin, "-d", "-i", keyFile, snap).Output()
		if err != nil {
			t.Fatalf("stock age -d: %v", err)
		}
		plain = out
		t.Log("decrypted with", bin)
	} else {
		ids, err := age.ParseIdentities(strings.NewReader(keyText))
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(snap)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		r, err := age.Decrypt(f, ids...)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(r); err != nil {
			t.Fatal(err)
		}
		plain = buf.Bytes()
		t.Log("age not installed; decrypted with filippo.io/age from the printed key")
	}
	names, files := untar(t, bytes.NewReader(plain))
	if names[0] != nameManifest || manifestOf(t, files).Kind != "backup" {
		t.Fatalf("not a snapshot: %v", names)
	}
}

func TestSnapshotWhileWritingRestoresIntact(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	dir := t.TempDir()
	e := engineFor(t, tw, p, dir)
	st, err := memory.Open(tw.data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var wrote atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := st.Remember(context.Background(), "load", fmt.Sprintf("fact number %d %s", i, strings.Repeat("x", 200)), "test"); err == nil {
				wrote.Add(1)
			}
		}
	}()
	for wrote.Load() < 20 {
		time.Sleep(time.Millisecond)
	}
	_, err = e.Run(context.Background())
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	newHome := filepath.Join(t.TempDir(), ".mirrin")
	r, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: newHome, HostLabel: "New Mac"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", memory.FileURI(filepath.Join(newHome, "data", "memory.db"))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var res string
	var n int
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
		t.Fatalf("integrity_check: %v %q", err, res)
	}
	if err := db.QueryRow(`SELECT count(*) FROM facts`).Scan(&n); err != nil || n < 20 {
		t.Fatalf("facts: %d %v", n, err)
	}
	if r.Manifest.Seq != 1 {
		t.Fatalf("seq %d", r.Manifest.Seq)
	}
}

func TestFlippedByteIsDamagedAndChangesNothing(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := LoadState(tw.data)
	snap := snapshotPath(e, st.LastName)
	b, err := os.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{len(b) / 2, len(b) - 5} {
		bad := bytes.Clone(b)
		bad[at] ^= 0x01
		if err := os.WriteFile(snap, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(context.Background(), e.Target, st.LastName, p); !errors.Is(err, ErrDamaged) || !strings.Contains(err.Error(), "snapshot damaged") {
			t.Fatalf("byte %d: verify said %v", at, err)
		}
		// A restore over an existing twin leaves it exactly as it was.
		before := tree(t, tw.home)
		_, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Name: st.LastName, Phrase: p, Home: tw.home})
		if !errors.Is(err, ErrDamaged) {
			t.Fatalf("byte %d: restore said %v", at, err)
		}
		if after := tree(t, tw.home); !slices.Equal(before, after) {
			t.Fatalf("a damaged restore changed the home:\n%v\n%v", before, after)
		}
		if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(tw.home), ".*restore*")); len(leftovers) > 0 {
			t.Fatalf("staging left behind: %v", leftovers)
		}
	}
}

// tree lists every file under dir with its hash.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(dir, p)
		out = append(out, rel+" "+hex.EncodeToString(sum[:8]))
		return nil
	})
	return out
}

func TestWrongWordsOpenNothing(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	other, _ := NewPhrase()
	ls, err := List(context.Background(), e.Target, other)
	if err != nil || len(ls) != 1 || !errors.Is(ls[0].Err, ErrWrongWords) {
		t.Fatalf("list with other words: %v %+v", err, ls)
	}
	before := tree(t, tw.home)
	if _, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: other, Home: tw.home}); !errors.Is(err, ErrWrongWords) {
		t.Fatalf("restore with other words: %v", err)
	}
	if !slices.Equal(before, tree(t, tw.home)) {
		t.Fatal("wrong words changed the home")
	}
}

func TestListReadsOnlyTheManifest(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	base := time.Date(2026, 9, 20, 3, 30, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		runAt(t, e, base.Add(time.Duration(i)*24*time.Hour))
	}
	// Cut the newest snapshot short after its first chunk: listing still
	// reads its manifest; verifying it finds the damage.
	st, _ := LoadState(tw.data)
	snap := snapshotPath(e, st.LastName)
	b, _ := os.ReadFile(snap)
	if len(b) > 70<<10 {
		if err := os.WriteFile(snap, b[:66<<10], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ls, err := List(context.Background(), e.Target, p)
	if err != nil || len(ls) != 3 {
		t.Fatalf("%v %d", err, len(ls))
	}
	if ls[0].Manifest.Seq != 3 || ls[2].Manifest.Seq != 1 || ls[0].Err != nil {
		t.Fatalf("order: %d %d %v", ls[0].Manifest.Seq, ls[2].Manifest.Seq, ls[0].Err)
	}
	if !ls[0].Manifest.ExportedAt.Equal(base.Add(48 * time.Hour)) {
		t.Fatalf("authenticated date: %v", ls[0].Manifest.ExportedAt)
	}
}

func TestAProtocolsFolderHoldingTheHomeIsRefused(t *testing.T) {
	tw := newTwin(t)
	tw.layout.ProtocolsDir = tw.home
	e := engineFor(t, tw, fixedPhrase(t), t.TempDir())
	if _, err := e.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "protocols_dir") {
		t.Fatalf("got %v", err)
	}
}

func TestReadBackCatchesATargetThatLoses(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	e.Target = lossy{e.Target}
	if _, err := e.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "didn't read back") {
		t.Fatalf("got %v", err)
	}
	st, _ := LoadState(tw.data)
	if !st.LastGood.IsZero() || st.LastError == "" {
		t.Fatalf("a bad copy was counted as good: %+v", st)
	}
}

// lossy stores one byte less than it is given.
type lossy struct{ Target }

func (l lossy) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	var buf bytes.Buffer
	buf.ReadFrom(r)
	b := buf.Bytes()
	return l.Target.Put(ctx, name, bytes.NewReader(b[:len(b)-1]), -1)
}

// A run cut off by a power cut leaves plain copies in the home; the next
// run clears them, so a fact forgotten meanwhile doesn't linger there.
func TestStaleStagingIsCleared(t *testing.T) {
	tw := newTwin(t)
	stale := filepath.Join(tw.home, ".backup-123", "files", "data")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "memory.db"), []byte("old copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := engineFor(t, tw, fixedPhrase(t), t.TempDir())
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(tw.home, ".backup-*")); len(left) != 0 {
		t.Fatalf("staging left: %v", left)
	}
}

// A file whose name a restore can't recreate on every system is left out
// and named, instead of making every snapshot unrestorable. Finder stores
// "Meeting 10/30" as "Meeting 10:30".
func TestUnrestorableNamesAreLeftOutAndNamed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows doesn't allow these names")
	}
	tw := newTwin(t)
	bad := []string{"protocols/Meeting 10:30.yaml", `protocols/back\slash.yaml`, "protocols/10:30/inside.yaml"}
	for _, n := range bad {
		p := filepath.Join(tw.home, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("name: x\nprompt: y\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wantOut := []string{"protocols/Meeting 10:30.yaml", `protocols/back\slash.yaml`, "protocols/10:30"}
	// A name that isn't UTF-8, where the file system allows one (APFS doesn't).
	if err := os.WriteFile(filepath.Join(tw.home, "protocols", "caf\xe9.yaml"), []byte("x"), 0o600); err == nil {
		wantOut = append(wantOut, "protocols/caf�.yaml")
	}
	p := fixedPhrase(t)
	dir := t.TempDir()
	e := engineFor(t, tw, p, dir)
	m, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := LoadState(tw.data)
	for _, x := range wantOut {
		if !slices.Contains(m.Excluded, x) {
			t.Errorf("excluded list lacks %q: %v", x, m.Excluded)
		}
		if !slices.Contains(st.LeftOut, x) {
			t.Errorf("the state doesn't name %q for the owner: %v", x, st.LeftOut)
		}
	}
	if _, err := Verify(context.Background(), e.Target, st.LastName, p); err != nil {
		t.Fatalf("the snapshot doesn't verify: %v", err)
	}
	home := filepath.Join(t.TempDir(), ".mirrin")
	if _, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: home}); err != nil {
		t.Fatalf("the snapshot doesn't restore: %v", err)
	}
	if !exists(filepath.Join(home, "protocols", "morning.yaml")) {
		t.Fatal("the rest of the protocols didn't come back")
	}
	// Renamed, it goes in the next backup, and the note goes away.
	if err := os.Rename(filepath.Join(tw.home, "protocols", "Meeting 10:30.yaml"), filepath.Join(tw.home, "protocols", "Meeting 10-30.yaml")); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(tw.home, "protocols", `back\slash.yaml`))
	os.RemoveAll(filepath.Join(tw.home, "protocols", "10:30"))
	os.Remove(filepath.Join(tw.home, "protocols", "caf\xe9.yaml"))
	m, err = e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := LoadState(tw.data); len(st.LeftOut) != 0 || !slices.ContainsFunc(m.Contents, func(f identity.ManifestFile) bool { return f.Path == "protocols/Meeting 10-30.yaml" }) {
		t.Fatalf("after renaming: left out %v", st.LeftOut)
	}
}

func TestRestorableNames(t *testing.T) {
	for name, want := range map[string]bool{
		"protocols/morning.yaml":       true,
		"protocols/concert.yaml":       true,
		"tools/com10/run.sh":           true,
		"protocols/Meeting 10:30.yaml": false,
		`protocols/back\slash.yaml`:    false,
		"protocols/caf\xe9.yaml":       false,
		"protocols/con.yaml":           false, // reserved on Windows
		"tools/NUL":                    false,
		"tools/lpt1.txt":               false,
		"../evil":                      false,
		"/etc/passwd":                  false,
		"manifest.json":                true, // a name; checkEntries keeps it for the manifest itself
	} {
		if got := restorable(name); got != want {
			t.Errorf("restorable(%q) = %v", name, got)
		}
	}
}

// The writer checks its manifest by the rule the reader applies, so it
// can never save a snapshot a restore would call damaged.
func TestTheWriterAndReaderShareOneRule(t *testing.T) {
	sum := strings.Repeat("0", 64)
	for _, files := range [][]identity.ManifestFile{
		{{Path: "protocols/a:b.yaml", SHA256: sum}},
		{{Path: "manifest.json", SHA256: sum}},
		{{Path: "config.yaml", SHA256: sum}, {Path: "config.yaml", SHA256: sum}},
		{{Path: "config.yaml", SHA256: "x"}},
	} {
		if _, err := checkEntries(files); err == nil {
			t.Errorf("%+v passed", files)
		}
	}
	if _, err := checkEntries([]identity.ManifestFile{{Path: "config.yaml", SHA256: sum}}); err != nil {
		t.Fatal(err)
	}
}
