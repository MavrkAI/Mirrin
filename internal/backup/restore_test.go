package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/MavrkAI/Mirrin/internal/identity"
)

func TestRestoreOnANewMachine(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	dir := t.TempDir()
	e := engineFor(t, tw, p, dir)
	runAt(t, e, time.Now().Add(-time.Hour))
	oldToken, _ := os.ReadFile(filepath.Join(tw.data, "api.token"))

	newHome := filepath.Join(t.TempDir(), "elsewhere", ".mirrin")
	r, err := Restore(context.Background(), RestoreOptions{Target: nsFolder(dir, p.Namespace(), ""), Phrase: p, Home: newHome, HostLabel: "New Mac"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Aside != "" || len(r.Kept) != 0 {
		t.Fatalf("nothing was here to move aside: %+v", r)
	}
	for _, f := range []string{"config.yaml", "secrets.env", "data/memory.db", "personas/butler.yaml", "protocols/morning.yaml", "tools/shout/run.sh", "data/tls/acme-account.key"} {
		if !exists(filepath.Join(newHome, filepath.FromSlash(f))) {
			t.Errorf("%s wasn't restored", f)
		}
	}
	if st, err := os.Stat(filepath.Join(newHome, "tools", "shout", "run.sh")); err != nil || st.Mode()&0o100 == 0 {
		t.Errorf("a tool's script lost its execute bit: %v", err)
	}
	tok, err := os.ReadFile(filepath.Join(newHome, "data", "api.token"))
	if err != nil || len(strings.TrimSpace(string(tok))) < 32 || bytes.Equal(tok, oldToken) {
		t.Fatalf("a new api.token should be minted: %q %v", tok, err)
	}
	// The config followed the twin to its new home.
	cfg, _ := os.ReadFile(filepath.Join(newHome, "config.yaml"))
	if strings.Contains(string(cfg), tw.home) || !strings.Contains(string(cfg), filepath.Join(newHome, "data")) {
		t.Fatalf("config still points at the old home:\n%s", cfg)
	}
	if !strings.Contains(string(cfg), "# my twin") {
		t.Fatalf("the config's comments were lost:\n%s", cfg)
	}
	if !slices.Equal(r.Devices, []string{"Akshay's iPhone (0123abcd)", "Old iPad (89abcdef)"}) {
		t.Fatalf("device review: %v", r.Devices)
	}
	if !slices.ContainsFunc(r.Checklist, func(s string) bool { return strings.Contains(s, "whatsapp login") }) {
		t.Fatalf("checklist: %v", r.Checklist)
	}
	if r.Handover == "" {
		t.Fatalf("no handover marker: %s", r.HandoverNote)
	}
	st, _ := LoadState(filepath.Join(newHome, "data"))
	if st.Seq != 1 || st.Standby != nil {
		t.Fatalf("state after restore: %+v", st)
	}
	// The welcome page says what stayed on the old machine.
	if st.From == nil || *st.From != (MovedFrom{Host: "Test Mac", StandsBy: true, SignIns: true}) {
		t.Fatalf("where it came from: %+v", st.From)
	}
	if exists(filepath.Join(newHome, "data", standbyKeyFile)) {
		t.Fatal("the old machine's standby key came along")
	}
	// The owner just typed the words: the yearly question starts from now.
	if NudgeDue(st, time.Now().Add(30*24*time.Hour)) || !NudgeDue(st, time.Now().Add(366*24*time.Hour)) {
		t.Fatalf("a restored twin never asks about its words: %+v", st)
	}
}

func TestRestoreOverATwinMovesItAsideAndKeepsThisMachinesThings(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	dir := t.TempDir()
	e := engineFor(t, tw, p, dir)
	runAt(t, e, time.Now().Add(-time.Hour))
	// Something changes after the snapshot; the restore goes back before it.
	if err := os.WriteFile(filepath.Join(tw.home, "personas", "later.yaml"), []byte("name: Later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Now is fixed (so the name set aside is known) but follows the real
	// clock: a fixed date would make the snapshot look future-dated later.
	now := time.Now().UTC().Truncate(time.Second)
	r, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: tw.home, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if r.Aside != tw.home+".before-restore-"+now.Format("20060102-150405") || !exists(filepath.Join(r.Aside, "personas", "later.yaml")) {
		t.Fatalf("the twin that was here should be kept aside: %q", r.Aside)
	}
	if exists(filepath.Join(tw.home, "personas", "later.yaml")) {
		t.Fatal("the restore kept a file from after the snapshot")
	}
	for _, f := range []string{"models/ggml-small.en.bin", "data/chrome-profile/Default/Cookies", "data/whatsapp.db",
		"tts/kokoro-v1.0.onnx", "tts/venv/bin/python", "tts/hey_jeeves.onnx", "remote.yaml", "data/" + standbyKeyFile} {
		if !exists(filepath.Join(tw.home, filepath.FromSlash(f))) {
			t.Errorf("%s should stay with this machine", f)
		}
	}
	if !slices.Contains(r.Kept, "voice models") || !slices.Contains(r.Kept, "website sign-ins") || !slices.Contains(r.Kept, "the WhatsApp link") ||
		!slices.Contains(r.Kept, "the saved connection to your other twin") {
		t.Fatalf("kept: %v", r.Kept)
	}
	if r.Handover != "" || !r.SameMachine || !strings.Contains(r.HandoverNote, "this machine") {
		t.Fatalf("restoring on the same machine must not tell it to stand by: %q %q", r.Handover, r.HandoverNote)
	}
	if st, _ := LoadState(filepath.Join(tw.home, "data")); st.From != nil {
		t.Fatalf("it came from this machine, not another: %+v", st.From)
	}
	if exists(filepath.Join(tw.home, "data", "api.token")) {
		tok, _ := os.ReadFile(filepath.Join(tw.home, "data", "api.token"))
		if strings.TrimSpace(string(tok)) == plantedToken {
			t.Fatal("the old api.token survived the restore")
		}
	}
}

func TestRestoreRefusesWhileTheTwinRuns(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	runAt(t, e, time.Now())
	before := tree(t, tw.home)
	running := func() bool { return true }
	if _, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: tw.home, Running: running}); !errors.Is(err, ErrRunning) {
		t.Fatalf("got %v", err)
	}
	if !slices.Equal(before, tree(t, tw.home)) {
		t.Fatal("a refused restore changed the home")
	}
	if _, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: tw.home, Running: running, Force: true}); err != nil {
		t.Fatalf("--force: %v", err)
	}
}

func TestRestoreLeavesSessionsUnlessAsked(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	e.WithSessions = true
	runAt(t, e, time.Now())
	home := filepath.Join(t.TempDir(), ".mirrin")
	r, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(home, "data", "whatsapp.db")) || !r.SessionsLeft {
		t.Fatal("a WhatsApp session was restored without --with-sessions")
	}
	home2 := filepath.Join(t.TempDir(), ".mirrin")
	if _, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: home2, WithSessions: true}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(home2, "data", "whatsapp.db")) {
		t.Fatal("--with-sessions didn't restore the session")
	}
}

// craft writes a snapshot with the given manifest and tar entries.
func craft(t *testing.T, tgt Target, p Phrase, m Manifest, files map[string][]byte, order []string) string {
	t.Helper()
	rcpt, err := age.ParseHybridRecipient(p.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	aw, _ := age.Encrypt(&buf, rcpt)
	gz := gzip.NewWriter(aw)
	tw := tar.NewWriter(gz)
	mb, _ := json.Marshal(m)
	tw.WriteHeader(&tar.Header{Name: nameManifest, Mode: 0o600, Size: int64(len(mb)), Typeflag: tar.TypeReg})
	tw.Write(mb)
	for _, n := range order {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o600, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		tw.Write(files[n])
	}
	tw.Close()
	gz.Close()
	aw.Close()
	name := newName("snap", time.Now())
	if err := tgt.Put(context.Background(), name, &buf, int64(buf.Len())); err != nil {
		t.Fatal(err)
	}
	return name
}

func fileEntry(path string, body []byte) identity.ManifestFile {
	sum := sha256.Sum256(body)
	return identity.ManifestFile{Path: path, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
}

func TestPathsLeavingTheHomeAreRejected(t *testing.T) {
	p := fixedPhrase(t)
	root := t.TempDir()
	tgt := folderAt(t, filepath.Join(root, "backups"))
	body := []byte("pwned")
	for _, evil := range []string{"../evil", "/tmp/evil", "data/../../evil", `..\evil`, "C:/evil"} {
		m := Manifest{Format: 2, Kind: identity.KindBackup, ExportedAt: time.Now(), Contents: []identity.ManifestFile{fileEntry(evil, body)}}
		name := craft(t, tgt, p, m, map[string][]byte{evil: body}, []string{evil})
		home := filepath.Join(root, "home", ".mirrin")
		_, err := Restore(context.Background(), RestoreOptions{Target: tgt, Name: name, Phrase: p, Home: home})
		if !errors.Is(err, ErrDamaged) {
			t.Fatalf("%q: got %v", evil, err)
		}
		if exists(filepath.Join(root, "evil")) || exists(filepath.Join(root, "home", "evil")) || exists(home) {
			t.Fatalf("%q: something was written", evil)
		}
	}
	// A clean manifest with an unsafe tar entry next to it.
	good := []byte("name: x\n")
	m := Manifest{Format: 2, Kind: identity.KindBackup, ExportedAt: time.Now(), Contents: []identity.ManifestFile{fileEntry("config.yaml", good)}}
	name := craft(t, tgt, p, m, map[string][]byte{"config.yaml": good, "../evil": body}, []string{"config.yaml", "../evil"})
	if _, err := Restore(context.Background(), RestoreOptions{Target: tgt, Name: name, Phrase: p, Home: filepath.Join(root, "h2")}); !errors.Is(err, ErrDamaged) {
		t.Fatalf("unsafe tar entry: %v", err)
	}
	if exists(filepath.Join(root, "evil")) {
		t.Fatal("an unsafe entry was written")
	}
}

func TestAFileNotMatchingItsChecksumIsRejected(t *testing.T) {
	p := fixedPhrase(t)
	root := t.TempDir()
	tgt := folderAt(t, filepath.Join(root, "backups"))
	good := []byte("name: x\n")
	entry := fileEntry("config.yaml", []byte("name: y\n"))
	m := Manifest{Format: 2, Kind: identity.KindBackup, ExportedAt: time.Now(), Contents: []identity.ManifestFile{entry}}
	name := craft(t, tgt, p, m, map[string][]byte{"config.yaml": good}, []string{"config.yaml"})
	_, err := Verify(context.Background(), tgt, name, p)
	if !errors.Is(err, ErrDamaged) || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("got %v", err)
	}
	// A file the manifest lists but the archive lacks.
	m.Contents = append(m.Contents, fileEntry("personas/x.yaml", good))
	m.Contents[0] = fileEntry("config.yaml", good)
	name = craft(t, tgt, p, m, map[string][]byte{"config.yaml": good}, []string{"config.yaml"})
	if _, err := Verify(context.Background(), tgt, name, p); !errors.Is(err, ErrDamaged) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing file: %v", err)
	}
}

func TestANewerFormatAsksForAnUpdate(t *testing.T) {
	p := fixedPhrase(t)
	tgt := Folder(t.TempDir())
	name := craft(t, tgt, p, Manifest{Format: 3, Kind: identity.KindBackup}, nil, nil)
	if _, err := Verify(context.Background(), tgt, name, p); err == nil || !strings.Contains(err.Error(), "newer Mirrin") {
		t.Fatalf("got %v", err)
	}
}

// countingTarget counts the bytes read from snapshots.
type countingTarget struct {
	Target
	read *int64
}

type countingReader struct {
	io.ReadCloser
	n *int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	*c.n += int64(n)
	return n, err
}

func (c countingTarget) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	rc, err := c.Target.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return countingReader{rc, c.read}, nil
}

func TestListingDecryptsOnlyTheStart(t *testing.T) {
	tw := newTwin(t)
	// A big file that doesn't compress.
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = byte(i*7919 + i>>8)
	}
	sum := sha256.Sum256(big)
	for i := 0; i < len(big); i += 32 {
		sum = sha256.Sum256(sum[:])
		copy(big[i:], sum[:])
	}
	if err := os.WriteFile(filepath.Join(tw.home, "tools", "shout", "blob.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	runAt(t, e, time.Now())
	var read int64
	ls, err := List(context.Background(), countingTarget{e.Target, &read}, p)
	if err != nil || len(ls) != 1 || ls[0].Err != nil {
		t.Fatalf("%v %+v", err, ls)
	}
	if ls[0].Size < 2<<20 || read > 256<<10 {
		t.Fatalf("listing a %d-byte snapshot read %d bytes", ls[0].Size, read)
	}
}

// Going back to an older snapshot never reuses a number: the next backup
// carries on after the highest one there.
func TestSeqCarriesOnAfterRestoringAnOlderSnapshot(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	base := time.Now().Add(-72 * time.Hour)
	_, first := runAt(t, e, base)
	runAt(t, e, base.Add(24*time.Hour))
	runAt(t, e, base.Add(48*time.Hour))
	home := filepath.Join(t.TempDir(), ".mirrin")
	r, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Name: first, Phrase: p, Home: home})
	if err != nil || r.Manifest.Seq != 1 {
		t.Fatalf("%v %d", err, r.Manifest.Seq)
	}
	if !strings.Contains(r.Stale, "days old") {
		t.Fatalf("an old snapshot should say so: %q", r.Stale)
	}
	st, _ := LoadState(filepath.Join(home, "data"))
	if st.Seq != 3 {
		t.Fatalf("seq after restore: %d", st.Seq)
	}
}

// Rolling back on the same machine keeps its standby key, so every snapshot
// it took still names a key it holds, and restoring one of them again is
// still "from this machine" rather than a handover to itself.
func TestRestoringTwiceOnThisMachineKeepsItsStandbyKey(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	dir := t.TempDir()
	e := engineFor(t, tw, p, dir)
	runAt(t, e, time.Now().Add(-2*time.Hour))
	key := existingStandby(tw.data).Recipient().String()
	if err := UpdateState(tw.data, func(s *State) { s.Dismissed = []string{"handover-20260101T000000Z-00000000.age"} }); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		r, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: tw.home})
		if err != nil {
			t.Fatalf("restore %d: %v", i+1, err)
		}
		if r.Handover != "" || !r.SameMachine || !strings.Contains(r.HandoverNote, "came from this machine") {
			t.Fatalf("restore %d handed over to itself: %q %q", i+1, r.Handover, r.HandoverNote)
		}
		if got := existingStandby(tw.data); got == nil || got.Recipient().String() != key {
			t.Fatalf("restore %d lost this machine's standby key", i+1)
		}
		st, _ := LoadState(tw.data)
		if !slices.Contains(st.Dismissed, "handover-20260101T000000Z-00000000.age") {
			t.Fatalf("restore %d forgot the markers the owner dismissed: %v", i+1, st.Dismissed)
		}
		m, _ := runAt(t, engineFor(t, tw, p, dir), time.Now().Add(-time.Duration(1-i)*time.Hour))
		if m.HandoverTo != key {
			t.Fatalf("the backup after restore %d names a new key", i+1)
		}
	}
	objs, _ := e.Target.List(context.Background())
	for _, o := range objs {
		if !IsSnapshot(o.Name) {
			t.Fatalf("a marker was left: %s", o.Name)
		}
	}
}

// Moving the twin back to a machine it once left: the marker that told this
// machine to stand by is out of date, and must not make it stand by again.
func TestMovingBackIgnoresTheMarkerLeftForThisMachine(t *testing.T) {
	a := newTwin(t)
	p := fixedPhrase(t)
	dir := t.TempDir()
	ea := engineFor(t, a, p, dir)
	runAt(t, ea, time.Now().Add(-time.Hour))
	homeB := filepath.Join(t.TempDir(), ".mirrin")
	if r, err := Restore(context.Background(), RestoreOptions{Target: ea.Target, Phrase: p, Home: homeB, HostLabel: "Mac mini"}); err != nil || r.Handover == "" {
		t.Fatalf("%v %+v", err, r)
	}
	// A was off meanwhile; the owner brings the twin back to A.
	r, err := Restore(context.Background(), RestoreOptions{Target: ea.Target, Phrase: p, Home: a.home, Now: func() time.Time { return time.Now().Add(time.Minute) }})
	if err != nil || !r.SameMachine {
		t.Fatalf("%v %+v", err, r)
	}
	if h := schedulerFor(engineFor(t, a, p, dir)).CheckHandover(context.Background()); h != nil {
		t.Fatalf("A stood by for a marker older than its own restore: %+v", h)
	}
}

// Restoring from a copy (a USB disk) still reaches the old machine: the
// marker also goes to the target the old machine backs up to.
func TestTheMarkerGoesWhereTheOldMachineLooks(t *testing.T) {
	a := newTwin(t)
	p := fixedPhrase(t)
	dirA := t.TempDir()
	ea := engineFor(t, a, p, dirA)
	if err := SaveSettings(filepath.Join(a.home, "config.yaml"), ea.Settings); err != nil {
		t.Fatal(err)
	}
	runAt(t, ea, time.Now().Add(-time.Hour))
	usb := t.TempDir()
	if err := os.CopyFS(filepath.Join(usb, p.Namespace()), os.DirFS(filepath.Join(dirA, p.Namespace()))); err != nil {
		t.Fatal(err)
	}
	r, err := Restore(context.Background(), RestoreOptions{Target: Folder(filepath.Join(usb, p.Namespace())), Phrase: p, Home: filepath.Join(t.TempDir(), ".mirrin"), HostLabel: "New Mac"})
	if err != nil || r.Handover == "" {
		t.Fatalf("%v %q", err, r.HandoverNote)
	}
	h, err := CheckHandover(context.Background(), ea.Target, existingStandby(a.data), p.RecoveryPub(), nil, time.Time{})
	if err != nil || h == nil || h.HostLabel != "New Mac" {
		t.Fatalf("the old machine's own target has no marker: %+v %v", h, err)
	}

	// When that target can't be reached, the report says so instead of
	// promising the old machine will stand by.
	if err := os.RemoveAll(dirA); err != nil {
		t.Fatal(err)
	}
	r, err = Restore(context.Background(), RestoreOptions{Target: Folder(filepath.Join(usb, p.Namespace())), Phrase: p, Home: filepath.Join(t.TempDir(), ".mirrin"), HostLabel: "New Mac"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Handover != "" || !strings.Contains(r.HandoverNote, dirA) || exists(dirA) {
		t.Fatalf("an unreachable target: %q %q", r.Handover, r.HandoverNote)
	}
}

// Anyone who can write to the folder and knows backup.recipient can add a
// snapshot the words open. One dated in the future is never picked by
// itself: it would stay "the newest" for ever.
func TestAFutureDatedSnapshotIsNotPickedByItself(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	_, real := runAt(t, e, time.Now().Add(-time.Hour))
	body := []byte("name: Impostor\n")
	m := Manifest{Format: 2, Kind: identity.KindBackup, ExportedAt: time.Now().Add(90 * 24 * time.Hour), Seq: 999, Contents: []identity.ManifestFile{fileEntry("config.yaml", body)}}
	planted := craft(t, e.Target, p, m, map[string][]byte{"config.yaml": body}, []string{"config.yaml"})
	ls, err := List(context.Background(), e.Target, p)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Newest(ls, e.Target); err != nil || got != real {
		t.Fatalf("picked %s (%v), want %s", got, err, real)
	}
	r, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: filepath.Join(t.TempDir(), ".mirrin")})
	if err != nil || r.Name != real {
		t.Fatalf("restored %s (%v)", r.Name, err)
	}
	// Named, it can still be restored.
	if r, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Name: planted, Phrase: p, Home: filepath.Join(t.TempDir(), ".mirrin")}); err != nil || r.Name != planted {
		t.Fatalf("--snapshot: %v", err)
	}
}
