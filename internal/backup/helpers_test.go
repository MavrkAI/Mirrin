package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// Secrets planted in a test twin, which must never appear in a snapshot.
const (
	plantedToken       = "0123456789abcdef0123456789abcdef0123456789abcdef"
	plantedSafeStorage = "chrome-safe-storage-secret-5f3c"
	plantedCookie      = "session-cookie-9a8b7c"
	plantedDeviceKey   = "cloud-device-key-77aa"
	plantedAPIKey      = "sk-ant-planted-key-123"
)

// testTwin is a twin's home with every kind of file in it.
type testTwin struct {
	home, data string
	layout     Layout
}

func newTwin(t *testing.T) testTwin {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".mirrin")
	data := filepath.Join(home, "data")
	tw := testTwin{home: home, data: data}
	write := func(rel, body string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("config.yaml", "# my twin\nname: Jeeves\ndata_dir: "+data+"\nprotocols_dir: "+filepath.Join(home, "protocols")+
		"\ntools_dir: "+filepath.Join(home, "tools")+"\nllm:\n    provider: anthropic\n    api_key: "+plantedAPIKey+
		"\nchannels:\n    whatsapp:\n        enabled: true\n", 0o600)
	write("secrets.env", "ANTHROPIC_API_KEY="+plantedAPIKey+"\n", 0o600)
	write("personas/butler.yaml", "name: Butler\n", 0o600)
	write("protocols/morning.yaml", "name: morning\nprompt: hi\n", 0o600)
	write("tools/shout/tool.yaml", "name: shout\n", 0o600)
	write("tools/shout/run.sh", "#!/bin/sh\necho HI\n", 0o700)
	write("tools/shout/node_modules/huge.js", "x", 0o600)
	write("models/ggml-small.en.bin", "model", 0o600)
	write("tts/hey_maverick.onnx", "wake model from an earlier release", 0o600)
	write("tts/kokoro-v1.0.onnx", "voice engine", 0o600)
	write("tts/venv/bin/python", "py", 0o700)
	write("tts/hey_jeeves.onnx", "trained wake model", 0o600)
	write("logs/mirrin.log", "log", 0o600)
	write("remote.yaml", "token: "+plantedToken+"\n", 0o600)
	write("google-token.json", `{"refresh_token":"g"}`, 0o600)
	write("data/api.token", plantedToken+"\n", 0o600)
	write("data/chrome-profile/Default/Cookies", plantedCookie, 0o600)
	write("data/chrome-profile/Local State", `{"os_crypt":{"encrypted_key":"`+plantedSafeStorage+`"}}`, 0o600)
	write("data/cloud/device.key", plantedDeviceKey, 0o600)
	// The registry as internal/devices keeps it: two devices that can reach
	// the twin, one already cut off, and a browser on this computer.
	write("data/devices.json", `{"version":1,"devices":[`+
		`{"id":"0123abcd00000001","name":"Akshay's iPhone","kind":"pwa","scopes":["view","chat","approve"],"created":"2026-09-01T00:00:00Z"},`+
		`{"id":"89abcdef00000002","name":"Old iPad","kind":"kiosk","scopes":["view"],"created":"2026-09-01T00:00:00Z"},`+
		`{"id":"fedcba9800000003","name":"Lost phone","kind":"pwa","scopes":["view"],"created":"2026-09-01T00:00:00Z","revoked_at":"2026-09-10T00:00:00Z"},`+
		`{"id":"5555555500000004","name":"Chrome on this Mac","kind":"local","scopes":["view","chat","approve","admin"],"created":"2026-09-01T00:00:00Z"}]}`, 0o600)
	write("data/tls/acme-account.key", "acme", 0o600)
	write("data/browser-1.png", "png", 0o600)
	// A photo from a chat app (media): kept 30 days, never backed up.
	write("data/media/2026-09/ab12.jpg", "jpeg", 0o600)
	st, err := memory.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Remember(context.Background(), "Akshay", "likes flat whites", "test"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	// A WhatsApp session is a SQLite database too.
	makeDB(t, filepath.Join(data, "whatsapp.db"))
	tw.layout = Layout{Home: home, DataDir: data, ProtocolsDir: filepath.Join(home, "protocols"), ToolsDir: filepath.Join(home, "tools"),
		GoogleToken: filepath.Join(home, "google-token.json"), KokoroDir: filepath.Join(home, "tts")}
	return tw
}

// makeDB makes dst a small SQLite database.
func makeDB(t *testing.T, dst string) {
	t.Helper()
	dir := t.TempDir()
	st, err := memory.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), "whatsapp", "session"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := vacuumInto(context.Background(), filepath.Join(dir, "memory.db"), dst); err != nil {
		t.Fatal(err)
	}
}

// engineFor is an engine for tw that keeps snapshots in a folder.
func engineFor(t *testing.T, tw testTwin, p Phrase, dir string) *Engine {
	t.Helper()
	return &Engine{
		Layout:    tw.layout,
		Settings:  config.Backup{Recipient: p.Recipient(), RecoveryPub: p.RecoveryPub(), Target: TargetFolder, Path: dir},
		Target:    nsFolder(dir, p.Namespace(), ""),
		Keep:      DefaultRetention,
		Twin:      "Jeeves",
		HostLabel: "Test Mac",
		Version:   "test",
	}
}

// fixedPhrase is a public test vector: never a real twin's words.
func fixedPhrase(t *testing.T) Phrase {
	t.Helper()
	p, err := ParsePhrase("legal winner thank year wave sausage worth useful legal winner thank yellow")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// entries decrypts a snapshot with the library and returns its files.
func entries(t *testing.T, r io.Reader, ids ...age.Identity) (names []string, files map[string][]byte) {
	t.Helper()
	plain, err := age.Decrypt(r, ids...)
	if err != nil {
		t.Fatal(err)
	}
	return untar(t, plain)
}

func untar(t *testing.T, r io.Reader) ([]string, map[string][]byte) {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		files[h.Name] = b
	}
	return names, files
}

func manifestOf(t *testing.T, files map[string][]byte) Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(files[nameManifest], &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// snapshotPath is where the folder target keeps a snapshot.
func snapshotPath(e *Engine, name string) string {
	return filepath.Join(e.Target.(*folder).dir, name)
}

// runAt takes a snapshot as if at t.
func runAt(t *testing.T, e *Engine, at time.Time) (Manifest, string) {
	t.Helper()
	e.Now = func() time.Time { return at }
	m, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := LoadState(e.Layout.DataDir)
	return m, st.LastName
}

// folderAt is a folder target in dir, made first: a target never makes the
// folder the owner chose.
func folderAt(t *testing.T, dir string) Target {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return Folder(dir)
}
