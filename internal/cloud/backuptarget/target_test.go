package backuptarget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/backup/targettest"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/cloud/cloudtest"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/reach"
)

// planted is a secret in the twin's config: it must reach storage only
// encrypted.
const planted = "sk-ant-planted-cloud-backup-4242"

// The control plane derives a namespace the way the backup engine does.
func TestNamespaceMatchesTheEngine(t *testing.T) {
	p, err := backup.NewPhrase()
	if err != nil {
		t.Fatal(err)
	}
	if got := entitle.Namespace(p.RecoveryKey().Public().(ed25519.PublicKey)); got != p.Namespace() {
		t.Fatalf("control plane %q, engine %q", got, p.Namespace())
	}
	if p.RecoveryPub() != entitle.EncodeKey(p.RecoveryKey().Public().(ed25519.PublicKey)) {
		t.Fatal("the recovery key travels in another encoding than config keeps it")
	}
}

func phrase(t *testing.T) backup.Phrase {
	t.Helper()
	p, err := backup.NewPhrase()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// linked links a new machine and binds new words' namespace to it.
func linked(t *testing.T, d *cloudtest.DevServer, dataDir string) (*cloud.Client, backup.Phrase) {
	t.Helper()
	c := d.Link(t, dataDir)
	p := phrase(t)
	if err := Bind(context.Background(), c, p); err != nil {
		t.Fatal(err)
	}
	return c, p
}

// The whole paid backup journey against `mirrin-cloud serve --dev` and its
// disk storage.
func TestAgainstTheDevControlPlane(t *testing.T) {
	d := cloudtest.StartDev(t, "")
	ctx := context.Background()

	t.Run("conformance", func(t *testing.T) {
		targettest.Run(t, func(t *testing.T) backup.Target {
			c, _ := linked(t, d, t.TempDir())
			return New(c)
		})
	})

	t.Run("before the words bind it", func(t *testing.T) {
		c := d.Link(t, t.TempDir())
		err := New(c).Put(ctx, targettest.Name("snap", 1), strings.NewReader("x"), 1)
		if err == nil || !strings.Contains(err.Error(), "mirrin backup target cloud") {
			t.Fatalf("Put before binding: %v", err)
		}
	})

	t.Run("only under this twin's namespace", func(t *testing.T) {
		c, p := linked(t, d, t.TempDir())
		wrong := NewFor(c, phrase(t).Namespace())
		if err := wrong.Put(ctx, targettest.Name("snap", 1), strings.NewReader("x"), 1); err == nil || !strings.Contains(err.Error(), "other words") {
			t.Fatalf("Put under other words: %v", err)
		}
		if _, err := wrong.List(ctx); err == nil {
			t.Fatal("List under other words")
		}
		right := NewFor(c, p.Namespace())
		if err := right.Put(ctx, targettest.Name("snap", 1), strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
		if objs, err := right.List(ctx); err != nil || len(objs) != 1 {
			t.Fatalf("List: %v %v", objs, err)
		}
	})

	t.Run("snapshot, recover and restore elsewhere, migrate, lapse", func(t *testing.T) {
		journey(t, d)
	})
}

func journey(t *testing.T, d *cloudtest.DevServer) {
	ctx := context.Background()
	// The old machine: a twin whose data folder holds its link.
	home := filepath.Join(t.TempDir(), ".mirrin")
	data := filepath.Join(home, "data")
	c, p := linked(t, d, data)
	oldInfo, _, _ := c.State().Info()
	oldPub, _ := c.PublicKey()
	conf := "name: Jeeves\ndata_dir: " + data + "\nllm:\n    api_key: " + planted +
		"\nbackup:\n    recipient: " + p.Recipient() + "\n    recovery_pub: " + p.RecoveryPub() + "\n    target: cloud\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := memory.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	cfg := &config.Config{Name: "Jeeves", DataDir: data, ProtocolsDir: filepath.Join(home, "protocols"), ToolsDir: filepath.Join(home, "tools")}
	settings := config.Backup{Recipient: p.Recipient(), RecoveryPub: p.RecoveryPub(), Target: backup.TargetCloud}
	e := &backup.Engine{Layout: backup.LayoutOf(cfg, home), Settings: settings, Target: New(c), Keep: backup.DefaultRetention,
		Twin: "Jeeves", HostLabel: "Old Mac", Version: "test"}
	m, err := e.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := New(c).List(ctx)
	if err != nil || len(objs) != 1 || !backup.IsSnapshot(objs[0].Name) {
		t.Fatalf("after one backup: %v, %v", objs, err)
	}

	// The storage holds ciphertext only: no byte of the planted secret, nor
	// of the device key, in any file it keeps.
	key, _ := os.ReadFile(filepath.Join(data, "cloud", "device.key"))
	stored := 0
	filepath.WalkDir(filepath.Join(d.DataDir, "storage"), func(path string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(path)
		stored++
		if bytes.Contains(b, []byte(planted)) || len(key) > 0 && bytes.Contains(b, key[40:80]) {
			t.Errorf("%s holds plain text", path)
		}
		if !strings.Contains(filepath.ToSlash(path), "/storage/ns/"+p.Namespace()+"/") && strings.HasSuffix(path, objs[0].Name) {
			t.Errorf("%s is not under the namespace", path)
		}
		return nil
	})
	if stored == 0 {
		t.Fatal("nothing reached the storage")
	}

	// A new machine: the wrong words recover nothing.
	wrong, err := cloud.New(t.TempDir(), d.Origin, d.Ent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, wrong, phrase(t), ""); err == nil || !strings.Contains(err.Error(), "no backups made with those words") {
		t.Fatalf("recovery with the wrong words: %v", err)
	}
	// The right words move the account here.
	linkDir := t.TempDir()
	c2, err := cloud.New(linkDir, d.Origin, d.Ent)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Recover(ctx, c2, p, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Handle != oldInfo.Handle || rec.Gen != oldInfo.Gen+1 || !rec.Entitled {
		t.Fatalf("recovered %+v from %+v", rec, oldInfo)
	}
	if cl, kind := c2.State().Current(time.Now()); kind != cloud.Active || cl.Gen != rec.Gen {
		t.Fatalf("new machine: %v %+v", kind, cl)
	}
	// The old machine hears it at its next refresh, and relays at their
	// next poll of the deny list.
	var sup *cloud.SupersededError
	if _, err := c.Refresh(ctx); !errors.As(err, &sup) || sup.Gen != rec.Gen {
		t.Fatalf("old machine's refresh: %v", err)
	}
	if !slices.ContainsFunc(d.DenyList(t).Keys, func(e entitle.Entry) bool { return e.Value == entitle.EncodeKey(oldPub) }) {
		t.Fatal("the old machine's key isn't on the deny list")
	}
	if err := New(c).Put(ctx, targettest.Name("snap", 2), strings.NewReader("x"), 1); err == nil || !strings.Contains(err.Error(), "stands by") {
		t.Fatalf("the old machine backing up: %v", err)
	}

	// Restore there from the words: the link the recovery made moves in
	// with the twin, and the handover marker goes where the restored
	// config's target is.
	newHome := filepath.Join(t.TempDir(), ".mirrin")
	prev := backup.OpenCloud
	backup.OpenCloud = func(string) (backup.Target, error) {
		cc, err := cloud.New(filepath.Join(newHome, "data"), d.Origin, d.Ent)
		if err != nil {
			return nil, err
		}
		return New(cc), nil
	}
	t.Cleanup(func() { backup.OpenCloud = prev })
	r, err := backup.Restore(ctx, backup.RestoreOptions{Target: New(c2), Phrase: p, Home: newHome, HostLabel: "New Mac",
		Settle: func(newData string) error {
			return os.Rename(filepath.Join(linkDir, "cloud"), filepath.Join(newData, "cloud"))
		}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Manifest.Seq != m.Seq || r.Handover == "" {
		t.Fatalf("restore report %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(newHome, "config.yaml")); !bytes.Contains(b, []byte(planted)) {
		t.Fatalf("the restored config: %s", b)
	}
	c3, err := cloud.New(filepath.Join(newHome, "data"), d.Origin, d.Ent)
	if err != nil {
		t.Fatal(err)
	}
	if _, kind := c3.State().Current(time.Now()); kind != cloud.Active {
		t.Fatalf("the restored twin's link: %v", kind)
	}

	// Migrate to a folder: every object, byte for byte as storage holds it.
	folder := t.TempDir()
	mig, err := backup.Migrate(ctx, New(c3), backup.Folder(folder), nil)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := New(c3).List(ctx)
	if len(mig.Copied) != len(all) || len(all) != 2 {
		t.Fatalf("migrated %v of %v", mig.Copied, all)
	}
	for _, o := range all {
		got, err := os.ReadFile(filepath.Join(folder, o.Name))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(d.DataDir, "storage", "ns", p.Namespace(), o.Name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: not the same bytes (%v)", o.Name, err)
		}
	}
	if again, err := backup.Migrate(ctx, New(c3), backup.Folder(folder), nil); err != nil || len(again.Copied) != 0 || len(again.Same) != 2 {
		t.Fatalf("migrating again: %+v %v", again, err)
	}

	// Lapsed: nothing new goes up, the engine names the free places, and
	// what is there still lists and opens.
	if st := d.Dev(t, "/dev/paid-through", `{"handle":"`+rec.Handle+`","paid_through":"`+time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339)+`"}`); st != 204 {
		t.Fatalf("lapse: %d", st)
	}
	if err := New(c3).Put(ctx, targettest.Name("snap", 3), strings.NewReader("x"), 1); !errors.Is(err, backup.ErrWritesClosed) {
		t.Fatalf("Put while lapsed: %v", err)
	}
	e.Target = New(c3)
	if _, err := e.Run(ctx); err == nil || !strings.Contains(err.Error(), "mirrin backup target folder") {
		t.Fatalf("a backup while lapsed: %v", err)
	}
	if objs, err := New(c3).List(ctx); err != nil || len(objs) != 2 {
		t.Fatalf("List while lapsed: %v %v", objs, err)
	}
	rc, err := New(c3).Get(ctx, all[0].Name)
	if err != nil {
		t.Fatalf("Get while lapsed: %v", err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()

	// Recovered while lapsed: the account moves, with no entitlement. The
	// link record alone still lists and fetches for the 90 days, takes no
	// new backups, and is Expired to the rest of the daemon, whose cloud
	// reach stops before it sends anything.
	lapsedDir := t.TempDir()
	c4, err := cloud.New(lapsedDir, d.Origin, d.Ent)
	if err != nil {
		t.Fatal(err)
	}
	rec4, err := Recover(ctx, c4, p, "")
	if err != nil {
		t.Fatalf("recovery while lapsed: %v", err)
	}
	if rec4.Entitled || rec4.Handle != rec.Handle || rec4.Gen != rec.Gen+1 {
		t.Fatalf("recovered while lapsed %+v", rec4)
	}
	if tok, _ := c4.State().Entitlement(); tok != "" {
		t.Fatal("an entitlement was saved for a lapsed account")
	}
	if _, kind := c4.State().Current(time.Now()); kind != cloud.Expired {
		t.Fatalf("a lapsed recovery's link is %v, want Expired", kind)
	}
	lt := New(c4)
	if objs, err := lt.List(ctx); err != nil || len(objs) != 2 {
		t.Fatalf("List after a lapsed recovery: %v %v", objs, err)
	}
	rc, err = lt.Get(ctx, all[0].Name)
	if err != nil {
		t.Fatalf("Get after a lapsed recovery: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if want, _ := os.ReadFile(filepath.Join(d.DataDir, "storage", "ns", p.Namespace(), all[0].Name)); !bytes.Equal(got, want) {
		t.Fatal("a lapsed recovery fetched other bytes")
	}
	if err := lt.Put(ctx, targettest.Name("snap", 4), strings.NewReader("x"), 1); !errors.Is(err, backup.ErrWritesClosed) {
		t.Fatalf("Put after a lapsed recovery: %v", err)
	}
	var stop *reach.CloudStop
	if _, err := reach.StartCloud(ctx, c4, nil, reach.Deps{DataDir: lapsedDir}); !errors.As(err, &stop) || stop.End != reach.CloudExpired {
		t.Fatalf("cloud reach on a lapsed recovery: %v", err)
	}
	// The machine it moved from stands by.
	if _, err := c3.Refresh(ctx); !errors.As(err, &sup) {
		t.Fatalf("the machine recovered away from: %v", err)
	}
}

// Migrating more snapshots to Cloud than its retention keeps: the ones it
// drops are reported as not kept, and migrating again copies nothing and
// fails on nothing, since Cloud never takes a name back.
func TestMigrateToCloudRespectsItsRetention(t *testing.T) {
	d := cloudtest.StartDev(t, "")
	ctx := context.Background()
	c, _ := linked(t, d, t.TempDir())
	src := backup.Folder(t.TempDir())
	now := time.Now().UTC()
	const n = 40
	for i := n - 1; i >= 0; i-- {
		name := fmt.Sprintf("snap-%s-%08x.age", now.AddDate(0, 0, -i).Format("20060102T150405Z"), i)
		if err := src.Put(ctx, name, strings.NewReader("ciphertext"), 10); err != nil {
			t.Fatal(err)
		}
	}
	dst := New(c)
	m, err := backup.Migrate(ctx, src, dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := dst.List(ctx)
	if len(m.Copied)+len(m.NotKept) != n || len(m.NotKept) == 0 || len(m.Copied) != len(kept) {
		t.Fatalf("first migrate: %d copied, %d not kept; Cloud holds %d", len(m.Copied), len(m.NotKept), len(kept))
	}
	again, err := backup.Migrate(ctx, src, dst, nil)
	if err != nil {
		t.Fatalf("migrating again: %v", err)
	}
	if len(again.Copied) != 0 || len(again.Same) != len(kept) || len(again.NotKept) != len(m.NotKept) {
		t.Fatalf("again: %d copied, %d same, %d not kept", len(again.Copied), len(again.Same), len(again.NotKept))
	}
	if k, ok := dst.(backup.Keeper); !ok || !strings.Contains(k.Keeps(), "7 days") {
		t.Fatal("Cloud doesn't say what it keeps")
	}
}
