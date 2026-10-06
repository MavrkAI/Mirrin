package identity

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// A backup's manifest is JSON in the shape docs/backup-format.md gives.
func TestBackupManifestJSONShape(t *testing.T) {
	m := Manifest{Format: 2, Kind: KindBackup, WithSecrets: true, ExportedAt: time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC),
		Seq: 412, Twin: "Mirrin", HostLabel: "Akshay's MacBook Pro", Version: "0.4.0",
		Contents: []ManifestFile{{Path: "data/memory.db", Size: 10, SHA256: strings.Repeat("a", 64)}},
		Excluded: []string{"data/chrome-profile", "data/whatsapp.db"},
		Files:    []string{"identity-only"}, Secrets: []string{"llm.api_key"}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"format", "kind", "secrets", "created", "seq", "twin", "host_label", "version", "files", "excluded"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%s missing: %s", k, b)
		}
	}
	if got["secrets"] != true || got["created"] != "2026-09-27T03:30:00Z" {
		t.Errorf("secrets/created: %s", b)
	}
	files, _ := got["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["sha256"] == nil {
		t.Errorf("files: %s", b)
	}
	if strings.Contains(string(b), "identity-only") || strings.Contains(string(b), "llm.api_key") {
		t.Errorf("identity fields leaked into the backup manifest: %s", b)
	}
	var back Manifest
	if err := json.Unmarshal(b, &back); err != nil || back.Seq != 412 || len(back.Contents) != 1 || !back.WithSecrets {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

// An identity export's manifest.yaml is unchanged by the backup fields:
// format 2, no secrets, none of the backup-only keys.
func TestIdentityManifestStaysAsItWas(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("name: Ava\nllm:\n  api_key: sk-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	m, err := Export(home, out, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != 2 || m.Kind != "" || m.WithSecrets {
		t.Fatalf("manifest: %+v", m)
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"kind:", "seq:", "contents:", "with_secrets:", "excluded:", "home:", "handover_to:"} {
		if strings.Contains(string(b), k) {
			t.Errorf("manifest.yaml gained %s:\n%s", k, b)
		}
	}
	if len(m.Secrets) == 0 {
		t.Fatal("the export should list the key it left out")
	}
}

// Backup settings belong to the machine: an export never carries
// backup.recipient (with it, anyone who can write to the backup folder
// could add a snapshot the words open), and an import keeps this machine's
// own, so two machines never back up into one folder.
func TestBackupSettingsStayWithTheMachine(t *testing.T) {
	src := twinHome(t, func(c *config.Config) {
		c.Backup = config.Backup{Recipient: "age1pq1source", RecoveryPub: "source-pub", Target: "folder", Path: "/Volumes/Source"}
	})
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	if _, err := Export(src, out, false); err != nil {
		t.Fatal(err)
	}
	if cfg := readArchive(t, out)["config.yaml"]; strings.Contains(string(cfg), "backup") || strings.Contains(string(cfg), "age1pq1source") {
		t.Fatalf("the export carries the backup settings:\n%s", cfg)
	}
	// An archive from an older export that still has them doesn't bring them.
	withBackup(t, out, "backup:\n    recipient: age1pq1source\n    recovery_pub: source-pub\n    target: folder\n    path: /Volumes/Source\n")
	dst := twinHome(t, func(c *config.Config) {
		c.Backup = config.Backup{Recipient: "age1pq1mine", RecoveryPub: "mine-pub", Target: "icloud"}
	})
	if _, err := Import(dst, out); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(t, dst).Backup; got.Recipient != "age1pq1mine" || got.Target != "icloud" {
		t.Fatalf("the import changed this machine's backups: %+v", got)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	t.Setenv("MIRRIN_HOME", fresh)
	if _, err := Import(fresh, out); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(t, fresh).Backup; got.Recipient != "" {
		t.Fatalf("a fresh machine got another's backup keys: %+v", got)
	}
}

// withBackup appends yaml to the config.yaml in the archive at path, as an
// export made before backup settings were left out would carry them.
func withBackup(t *testing.T, path, extra string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		if hdr.Name == "config.yaml" {
			b = append(b, extra...)
			hdr.Size = int64(len(b))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		tw.Write(b)
	}
	f.Close()
	tw.Close()
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg := readArchive(t, path)["config.yaml"]; !strings.Contains(string(cfg), "age1pq1source") {
		t.Fatal("the archive wasn't changed")
	}
}
