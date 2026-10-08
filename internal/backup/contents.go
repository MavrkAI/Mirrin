package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/homelock"
)

// Layout is where a twin's files are on this machine. Archive paths are
// fixed ("data/memory.db", "protocols/…"), whichever folder config names,
// so a snapshot restores into any home.
type Layout struct {
	Home         string
	DataDir      string
	ProtocolsDir string
	ToolsDir     string
	// GoogleToken and GoogleCredentials are the OAuth files.
	GoogleToken       string
	GoogleCredentials string
	// KokoroDir holds the voice engine, and the wake-word models the owner
	// trained (docs/wake-word.md puts them there).
	KokoroDir string
	// WakeModel is the configured wake-word model, when it is a file path.
	WakeModel string
	// SignalDir is signal-cli's data folder, for backup.signal (signal.go).
	SignalDir string
}

// LayoutOf reads the layout from a loaded config.
func LayoutOf(cfg *config.Config, home string) Layout {
	l := Layout{
		Home:              home,
		DataDir:           cfg.DataDir,
		ProtocolsDir:      cfg.ProtocolsDir,
		ToolsDir:          cfg.ToolsDir,
		GoogleToken:       cfg.Skills.Calendar.TokenFile,
		GoogleCredentials: cfg.Skills.Calendar.CredentialsFile,
		KokoroDir:         cfg.Channels.Voice.KokoroDir,
	}
	l.SignalDir = SignalDataDir(cfg.Backup)
	if m := cfg.Channels.Voice.WakeModel; m != "" && filepath.IsAbs(m) {
		l.WakeModel = m
	}
	if l.DataDir == "" {
		l.DataDir = filepath.Join(home, "data")
	}
	for _, p := range []*string{&l.Home, &l.DataDir, &l.ProtocolsDir, &l.ToolsDir, &l.KokoroDir} {
		if *p == "" {
			continue
		}
		if abs, err := filepath.Abs(*p); err == nil {
			*p = abs
		}
	}
	return l
}

// Archive names of what a snapshot holds.
const (
	nameManifest = "manifest.json"
	nameConfig   = "config.yaml"
	nameSecrets  = "secrets.env"
	nameMemory   = "data/memory.db"
	nameWhatsApp = "data/whatsapp.db"
	nameToken    = "google-token.json"
	nameCreds    = "google-credentials.json"
)

// dataFiles are the files in the data folder a snapshot carries (besides
// the databases): paired devices, push subscriptions and keys.
var dataFiles = []string{"devices.json", "push.json", "vapid.pem"}

// dataDirs are the folders in the data folder a snapshot carries whole:
// the TLS keys and ACME account, so certificate pins survive a restore.
var dataDirs = []string{"tls"}

// skipDirs are never copied from personas, protocols or tools: they are
// rebuilt by installing, and can be huge.
var skipDirs = map[string]bool{"node_modules": true, ".venv": true, "venv": true, "__pycache__": true}

// isWakeModel reports whether a file in the voice folder is a wake-word
// model, not Kokoro's own model. hey_maverick.onnx counts: releases don't
// carry it any more, so voice setup can't put it back (docs/wake-word.md).
func isWakeModel(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return (ext == ".onnx" || ext == ".tflite") && !strings.HasPrefix(strings.ToLower(name), "kokoro")
}

// item is one file a snapshot takes: from src, as name.
type item struct {
	name string
	src  string
	db   bool // copied through SQLite (VACUUM INTO), not as a file
	tree bool // a folder, copied as one tar (signal.go)
}

// collect lists what a snapshot of l takes, and what it leaves out.
//
// It works from an allowlist, so nothing lands in a backup by accident:
// config.yaml (with its secrets) and secrets.env, the memory, personas,
// protocols, custom tools, trained wake models, paired devices, push keys,
// the TLS folder and the Google sign-in files. Never the browser profile
// (its cookies are bound to this Mac's keychain, whose Chrome Safe Storage
// secret Mirrin never reads), the local API token, cloud device keys,
// models, logs or screenshots. The WhatsApp session only with sessions.
//
// A file whose name couldn't be restored on every system (see restorable)
// is left out and listed in badNames, so the owner can rename it; one such
// name would otherwise make the whole snapshot unrestorable.
func collect(l Layout, withSessions bool) (items []item, excluded, badNames []string, err error) {
	for _, d := range []struct{ dir, name string }{{l.ProtocolsDir, "protocols"}, {l.ToolsDir, "tools"}} {
		if d.dir == "" {
			continue
		}
		for _, own := range []string{l.Home, l.DataDir} {
			if within(d.dir, own) {
				return nil, nil, nil, fmt.Errorf("the %s folder (%s) also holds the twin's own files; point %s_dir in config.yaml at a folder of its own, then back up again", d.name, d.dir, d.name)
			}
		}
	}
	addFile := func(name, src string) {
		if st, err := os.Stat(src); err == nil && st.Mode().IsRegular() {
			items = append(items, item{name: name, src: src})
		}
	}
	addFile(nameConfig, filepath.Join(l.Home, "config.yaml"))
	addFile(nameSecrets, filepath.Join(l.Home, "secrets.env"))
	if exists(filepath.Join(l.DataDir, "memory.db")) {
		items = append(items, item{name: nameMemory, src: filepath.Join(l.DataDir, "memory.db"), db: true})
	}
	if exists(filepath.Join(l.DataDir, "whatsapp.db")) {
		if withSessions {
			items = append(items, item{name: nameWhatsApp, src: filepath.Join(l.DataDir, "whatsapp.db"), db: true})
		} else {
			excluded = append(excluded, nameWhatsApp)
		}
	}
	for _, f := range dataFiles {
		addFile("data/"+f, filepath.Join(l.DataDir, f))
	}
	if l.GoogleToken != "" {
		addFile(nameToken, l.GoogleToken)
	}
	if l.GoogleCredentials != "" {
		addFile(nameCreds, l.GoogleCredentials)
	}
	// Wake-word models, which no release reinstalls: in the voice folder
	// (whose engine and voices `mirrin voice setup` reinstalls, so they
	// aren't backed up), or wherever wake_model points. They restore into
	// tts/.
	if l.KokoroDir != "" {
		if es, err := os.ReadDir(l.KokoroDir); err == nil {
			for _, e := range es {
				if isWakeModel(e.Name()) && e.Type().IsRegular() {
					addFile("tts/"+e.Name(), filepath.Join(l.KokoroDir, e.Name()))
				}
			}
			excluded = append(excluded, "tts")
		}
	}
	if l.WakeModel != "" && isWakeModel(filepath.Base(l.WakeModel)) && !within(l.KokoroDir, l.WakeModel) {
		addFile("tts/"+filepath.Base(l.WakeModel), l.WakeModel)
	}
	trees := []struct{ dir, name string }{
		{filepath.Join(l.Home, "personas"), "personas"},
		{l.ProtocolsDir, "protocols"},
		{l.ToolsDir, "tools"},
	}
	for _, d := range dataDirs {
		trees = append(trees, struct{ dir, name string }{filepath.Join(l.DataDir, d), "data/" + d})
	}
	for _, t := range trees {
		if t.dir == "" {
			continue
		}
		got, skipped, bad, err := walkTree(t.dir, t.name, l)
		if err != nil {
			return nil, nil, nil, err
		}
		items = append(items, got...)
		excluded = append(excluded, skipped...)
		badNames = append(badNames, bad...)
	}
	// Every name, wherever it came from (a trained wake model's too).
	kept := items[:0]
	for _, it := range items {
		if restorable(it.name) {
			kept = append(kept, it)
		} else {
			badNames = append(badNames, printable(it.name))
		}
	}
	items = kept
	excluded = append(append(excluded, badNames...), leftOut(l, items)...)
	for i, x := range excluded {
		excluded[i] = printable(x) // the manifest is JSON
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	sort.Strings(excluded)
	sort.Strings(badNames)
	return items, dedupe(excluded), dedupe(badNames), nil
}

// printable is a name as the manifest and the owner see it: bytes that
// aren't UTF-8 shown as U+FFFD.
func printable(name string) string { return strings.ToValidUTF8(name, "\uFFFD") }

// walkTree lists the regular files under dir as prefix/…, skipping links,
// rebuildable folders and anything denied. A folder whose name couldn't be
// restored everywhere is skipped whole and named in bad.
func walkTree(dir, prefix string, l Layout) (items []item, skipped, bad []string, err error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil, nil
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		name := prefix
		if rel != "." {
			name = path.Join(prefix, filepath.ToSlash(rel))
		}
		switch {
		case d.IsDir() && skipDirs[d.Name()]:
			skipped = append(skipped, name)
			return filepath.SkipDir
		case d.IsDir():
			if denied(p, l) {
				skipped = append(skipped, name)
				return filepath.SkipDir
			}
			if !restorable(name) {
				bad = append(bad, printable(name))
				return filepath.SkipDir
			}
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			skipped = append(skipped, name)
			return nil
		case !d.Type().IsRegular() || d.Name() == ".DS_Store" || strings.HasPrefix(d.Name(), ".partial-"):
			return nil
		case denied(p, l):
			skipped = append(skipped, name)
			return nil
		}
		items = append(items, item{name: name, src: p})
		return nil
	})
	return items, skipped, bad, err
}

// denied is what a snapshot never takes, wherever it turns up.
func denied(p string, l Layout) bool {
	for _, d := range []string{
		filepath.Join(l.DataDir, "chrome-profile"),
		filepath.Join(l.DataDir, "api.token"),
		filepath.Join(l.DataDir, "cloud"),
		filepath.Join(l.DataDir, standbyKeyFile),
		filepath.Join(l.Home, "models"),
		filepath.Join(l.Home, "tts"),
		filepath.Join(l.Home, "logs"),
		filepath.Join(l.Home, "remote.yaml"),
	} {
		if within(d, p) {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(p))
	return base == "api.token" || base == "device.key" || strings.Contains(base, "safe storage")
}

// leftOut names what sits in the home and data folders that a snapshot
// doesn't take, for the manifest's excluded list.
func leftOut(l Layout, items []item) []string {
	took := map[string]bool{}
	for _, it := range items {
		took[strings.SplitN(it.name, "/", 2)[0]] = true
		if strings.HasPrefix(it.name, "data/") {
			took["data/"+strings.SplitN(strings.TrimPrefix(it.name, "data/"), "/", 2)[0]] = true
		}
	}
	var out []string
	if es, err := os.ReadDir(l.Home); err == nil {
		for _, e := range es {
			n := e.Name()
			if strings.HasPrefix(n, ".") || took[n] || n == "data" || n == "personas" || n == "protocols" || n == "tools" ||
				within(filepath.Join(l.Home, n), l.DataDir) {
				continue
			}
			out = append(out, n)
		}
	}
	if es, err := os.ReadDir(l.DataDir); err == nil {
		for _, e := range es {
			n := e.Name()
			if strings.HasPrefix(n, ".") || took["data/"+n] || isSQLiteSidecar(n) || n == stateFile || n == lockName || n == homelock.Name {
				continue
			}
			if ext := filepath.Ext(n); !e.IsDir() && ext != "" && ext != ".db" && ext != ".json" && ext != ".token" {
				n = "*" + ext // screenshots, voice clips: one line each
			}
			out = append(out, "data/"+n)
		}
	}
	return out
}

func isSQLiteSidecar(n string) bool {
	return strings.HasSuffix(n, "-wal") || strings.HasSuffix(n, "-shm") || strings.HasSuffix(n, "-journal")
}

// within reports whether p is dir or inside it.
func within(dir, p string) bool {
	if dir == "" || p == "" {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}
