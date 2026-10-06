// Package identity moves a twin between machines. An identity is everything
// that makes your twin yours: config (minus secrets), memory (facts, portrait,
// conversations), personas, protocols and their variables. It is a tar.gz you
// own; nothing about it lives on a server.
//
// Export leaves out every secret and writes paths relative to the twin's
// home. Import checks the whole archive before it changes anything, saves
// what it replaces under backups/, keeps this machine's keys and
// machine-specific settings, and replaces each file atomically. An archive is
// personal: its settings can add commands and endpoints, so import your own.
package identity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// Format is the archive layout this version writes. Import also reads format 1.
const Format = 2

// Manifest describes an export (manifest.yaml) or an encrypted backup
// (manifest.json, the first entry of the snapshot; see manifest.go and
// docs/backup-format.md). Both are format 2.
type Manifest struct {
	Format int `yaml:"format" json:"format"`
	// Kind is KindBackup for a backup; identity archives leave it empty.
	Kind       string    `yaml:"kind,omitempty" json:"kind,omitempty"`
	ExportedAt time.Time `yaml:"exported_at" json:"created"`
	// Seq numbers one machine's backups in order. Like the date, it is
	// inside the encryption, so a snapshot can't be re-dated or reordered.
	Seq  int64  `yaml:"seq,omitempty" json:"seq,omitempty"`
	Twin string `yaml:"twin" json:"twin"`
	// HostLabel names the machine a backup was taken on.
	HostLabel string `yaml:"host_label,omitempty" json:"host_label,omitempty"`
	// Version is the Mirrin version that wrote a backup.
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	Persona string `yaml:"persona" json:"persona,omitempty"`
	User    string `yaml:"user" json:"user,omitempty"`
	// OS is the operating system the twin was exported from.
	OS string `yaml:"os,omitempty" json:"os,omitempty"`
	// Conversations is set when the archive holds the whole memory database.
	Conversations bool     `yaml:"conversations,omitempty" json:"conversations,omitempty"`
	Files         []string `yaml:"files" json:"-"`
	// Contents lists a backup's files, each with its size and SHA-256.
	Contents []ManifestFile `yaml:"contents,omitempty" json:"files,omitempty"`
	// Secrets names the settings left out because they held keys or passwords.
	Secrets []string `yaml:"secrets,omitempty" json:"-"`
	// WithSecrets is set when the archive holds keys and passwords: a
	// backup does, an identity export never does.
	WithSecrets bool `yaml:"with_secrets,omitempty" json:"secrets"`
	// Excluded lists what a backup left out on purpose.
	Excluded []string `yaml:"excluded,omitempty" json:"excluded,omitempty"`
	// Home and UserHome are where the twin lived, so a restore on another
	// machine can move its paths.
	Home     string `yaml:"-" json:"home,omitempty"`
	UserHome string `yaml:"-" json:"user_home,omitempty"`
	// Zone is the system's time zone where a backup was taken, so a restored
	// config from before settings were saved in layers keeps following the
	// system rather than taking the old machine's zone for a pinned one.
	Zone string `yaml:"-" json:"zone,omitempty"`
	// HandoverTo is the age recipient of the machine that took a backup;
	// restoring it elsewhere leaves a marker only that machine can read.
	HandoverTo string `yaml:"-" json:"handover_to,omitempty"`
	// SignedIn lists sign-ins the source machine had (whatsapp, google,
	// browser); they stay with that machine.
	SignedIn []string `yaml:"signed_in,omitempty" json:"signed_in,omitempty"`
	// Packs records where installed packs came from, so they can be reconnected.
	Packs []PackSource `yaml:"packs,omitempty" json:"packs,omitempty"`
}

// Result is what an import did, for the CLI to report.
type Result struct {
	Manifest Manifest
	// Backup is the folder holding what the import replaced; empty when it replaced nothing.
	Backup string
	// Missing lists secrets the twin used that this machine does not have yet.
	Missing []string
	// MCPEnv lists, by MCP server, the env values that stayed on the old
	// machine. They are left out in case they are secret; some are not.
	MCPEnv map[string][]string
	// Warnings are things to check: an endpoint the archive changed where
	// this machine's own key would now be sent.
	Warnings []string
	// Relink lists sign-ins to do again on this machine.
	Relink []string
	// Packs are the packs that arrived as copies and couldn't be cloned
	// again from where they came from (offline, say), with that source.
	Packs []PackSource
	// Reconnected lists the packs cloned again from their source, which
	// update here as they did on the old machine.
	Reconnected []string
	// Skipped lists archive entries this version does not import.
	Skipped []string
}

var errNotArchive = errors.New("that file isn't a Mirrin identity archive (make one with `mirrin identity export`)")

// Export writes the identity archive to out. home is ~/.mirrin.
func Export(home, out string, includeConversations bool) (Manifest, error) {
	m := Manifest{Format: Format, ExportedAt: time.Now().UTC(), OS: runtime.GOOS}
	raw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return m, fmt.Errorf("there's no twin at %s to export yet; run `mirrin init` first", home)
	}
	if err != nil {
		return m, err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return m, fmt.Errorf("config.yaml doesn't read (%v); fix it, then export again", err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	m.Twin, _ = cfg["name"].(string)
	m.Persona, _ = cfg["persona"].(string)
	if u, ok := cfg["user"].(map[string]any); ok {
		m.User, _ = u["name"].(string)
	}
	dataDir := dirSetting(cfg, "data_dir", home, "data")
	protocolsDir := dirSetting(cfg, "protocols_dir", home, "protocols")
	if err := checkProtocolsDir(protocolsDir, home, dataDir); err != nil {
		return m, fmt.Errorf("%v, then export again", err)
	}
	googleToken := filepath.Join(home, "google-token.json")
	if v, ok := lookup(cfg, []string{"skills", "calendar", "token_file"}); ok {
		if s, _ := v.(string); s != "" {
			googleToken = expandTilde(s)
		}
	}
	for name, p := range map[string]string{
		"whatsapp": filepath.Join(dataDir, "whatsapp.db"),
		"google":   googleToken,
		"browser":  filepath.Join(dataDir, "chrome-profile"),
	} {
		if exists(p) {
			m.SignedIn = append(m.SignedIn, name)
		}
	}
	sort.Strings(m.SignedIn)

	// Backup settings describe this machine's backups: with
	// backup.recipient, anyone who can write to the backup folder could add
	// a snapshot the words open, and an import that kept them would back up
	// into the same folder as the machine the twin came from.
	delete(cfg, "backup")
	m.Secrets = scrubSecrets(cfg)
	makePortable(cfg, home)
	cb, err := yaml.Marshal(cfg)
	if err != nil {
		return m, err
	}
	entries := []entry{{name: "config.yaml", data: cb}}

	// Memory: facts and portrait always; the whole database with conversations.
	if dbPath := filepath.Join(dataDir, "memory.db"); exists(dbPath) {
		if includeConversations {
			// The snapshot goes beside the archive, not in data_dir, which may
			// be a synced folder that would upload every conversation.
			tmp, err := os.MkdirTemp(filepath.Dir(out), "."+filepath.Base(out)+".memory-")
			if err != nil {
				return m, err
			}
			defer os.RemoveAll(tmp)
			snap := filepath.Join(tmp, "memory.db")
			if err := snapshotDB(dbPath, snap); err != nil {
				return m, fmt.Errorf("couldn't copy the memory (%v)", err)
			}
			entries = append(entries, entry{name: "data/memory.db", path: snap})
			m.Conversations = true
		} else {
			b, err := exportFacts(dbPath)
			if err != nil {
				return m, fmt.Errorf("couldn't read the memory (%v)", err)
			}
			entries = append(entries, entry{name: "memory.yaml", data: b})
		}
	}
	for _, d := range []struct{ dir, prefix string }{{filepath.Join(home, "personas"), "personas"}, {protocolsDir, "protocols"}} {
		es, err := listFiles(d.dir, d.prefix, out)
		if err != nil {
			return m, err
		}
		entries = append(entries, es...)
	}
	m.Packs = packSources(protocolsDir)
	for _, e := range entries {
		if err := e.fits(); err != nil {
			return m, err
		}
		m.Files = append(m.Files, e.name)
	}
	if err := writeArchiveFile(out, m, entries); err != nil {
		return m, err
	}
	return m, nil
}

// placement is one file an import puts in place: a staged file or bytes.
type placement struct {
	name, src, dest string
	data            []byte
}

// Import brings the twin in archive in to home. Nothing changes unless the
// whole archive checks out; what it replaces is saved under home/backups.
// The twin should not be running: its next settings save would overwrite the
// imported config.
func Import(home, in string) (Result, error) {
	var r Result
	f, err := os.Open(in)
	if err != nil {
		return r, err
	}
	defer f.Close()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return r, err
	}
	stage, err := os.MkdirTemp(home, ".import-")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(stage)
	staged := func(name string) string { return filepath.Join(stage, filepath.FromSlash(name)) }

	names, skipped, err := unpack(f, stage)
	if err != nil {
		return r, err
	}
	r.Skipped = skipped
	has := map[string]bool{}
	for _, n := range names {
		has[n] = true
	}
	mb, err := os.ReadFile(staged("manifest.yaml"))
	if err != nil || yaml.Unmarshal(mb, &r.Manifest) != nil || r.Manifest.Format < 1 {
		return r, errNotArchive
	}
	m := r.Manifest
	if m.Format > Format {
		return r, errors.New("this archive comes from a newer Mirrin; update Mirrin here, then import again")
	}

	// 1. Work everything out, and check it, before changing anything.
	cfgPath := filepath.Join(home, "config.yaml")
	cur := readSettings(cfgPath)
	settings := cur
	var newConfig []byte
	var cfg *config.Config
	if has["config.yaml"] {
		b, err := os.ReadFile(staged("config.yaml"))
		if err != nil {
			return r, err
		}
		var incoming map[string]any
		if err := yaml.Unmarshal(b, &incoming); err != nil {
			return r, fmt.Errorf("the settings in this archive don't read (%v); nothing was changed", err)
		}
		merged := mergeSettings(cur, incoming, m, home, &r)
		out, err := yaml.Marshal(merged)
		if err != nil {
			return r, err
		}
		if cfg, err = checkSettings(out); err != nil {
			return r, fmt.Errorf("the settings in this archive wouldn't load here (%v); nothing was changed", err)
		}
		settings, newConfig = merged, out
	}
	dataDir := dirSetting(settings, "data_dir", home, "data")
	protocolsDir := dirSetting(settings, "protocols_dir", home, "protocols")

	var facts factsDoc
	switch {
	case has["data/memory.db"]:
		if err := checkDB(staged("data/memory.db")); err != nil {
			return r, fmt.Errorf("the memory in this archive is damaged (%v); nothing was changed", err)
		}
	case has["memory.yaml"]:
		b, err := os.ReadFile(staged("memory.yaml"))
		if err == nil {
			facts, err = parseFacts(b)
		}
		if err != nil {
			return r, fmt.Errorf("the memory in this archive doesn't read (%v); nothing was changed", err)
		}
	}

	var files []placement
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, "personas/"):
			files = append(files, placement{name: n, src: staged(n), dest: filepath.Join(home, filepath.FromSlash(n))})
		case strings.HasPrefix(n, "protocols/"):
			rel := strings.TrimPrefix(n, "protocols/")
			if pack := packOf(rel); pack != "" && isGitPack(protocolsDir, pack) {
				continue // installed here from git already; it updates itself
			}
			files = append(files, placement{name: n, src: staged(n), dest: filepath.Join(protocolsDir, filepath.FromSlash(rel))})
		}
	}
	for _, p := range m.Packs {
		if !p.valid() || isGitPack(protocolsDir, p.Dir) || !hasPrefixed(names, "protocols/packs/"+p.Dir+"/") {
			continue
		}
		r.Packs = append(r.Packs, p)
		files = append(files, placement{
			name: "protocols/packs/" + p.Dir + "/" + provenanceFile,
			dest: filepath.Join(protocolsDir, "packs", p.Dir, provenanceFile),
			data: p.provenanceJSON(),
		})
	}
	if hasPrefixed(names, "protocols/") {
		if err := checkProtocolsDir(protocolsDir, home, dataDir); err != nil {
			return r, fmt.Errorf("%v, then import again; nothing was changed", err)
		}
	}

	// 2. Save what the import replaces.
	bk := backup{home: home}
	var berr error
	if newConfig != nil && exists(cfgPath) {
		berr = bk.file("config.yaml", cfgPath)
	}
	if db := filepath.Join(dataDir, "memory.db"); berr == nil && exists(db) {
		switch {
		case has["data/memory.db"]:
			berr = bk.db("data/memory.db", db)
		case has["memory.yaml"]:
			// Facts are only added; the portrait is the one thing replaced.
			berr = bk.facts("memory.yaml", db)
		}
	}
	for _, p := range files {
		if berr == nil && exists(p.dest) {
			berr = bk.file(p.name, p.dest)
		}
	}
	r.Backup = bk.dir
	if berr != nil {
		return r, fmt.Errorf("couldn't save a backup before importing (%v); nothing was changed", berr)
	}

	// 3. Apply: memory, then personas and protocols, then the config last.
	fail := func(err error) error {
		if r.Backup != "" {
			return fmt.Errorf("the import stopped partway (%v); what it replaced is saved in %s", err, r.Backup)
		}
		return fmt.Errorf("the import stopped partway (%v)", err)
	}
	// The copy of the memory the import replaces is noted with the memory,
	// so forgetting a fact later scrubs it too (memory.NoteCopy).
	var memCopy string
	switch {
	case r.Backup == "":
	case has["data/memory.db"] && exists(filepath.Join(r.Backup, "data", "memory.db")):
		memCopy = filepath.Join(r.Backup, "data", "memory.db")
	case has["memory.yaml"] && exists(filepath.Join(r.Backup, "memory.yaml")):
		memCopy = filepath.Join(r.Backup, "memory.yaml")
	}
	switch {
	case has["data/memory.db"]:
		// The restore is one transaction and comes first, so when it fails
		// nothing has changed yet and the backup is not needed.
		if err := restoreDB(context.Background(), dataDir, staged("data/memory.db")); err != nil {
			if r.Backup != "" {
				os.RemoveAll(r.Backup)
				os.Remove(filepath.Dir(r.Backup)) // backups/, when this made it
				r.Backup = ""
			}
			return r, fmt.Errorf("couldn't restore the memory from this archive (%v); nothing was changed", err)
		}
	case has["memory.yaml"]:
		if err := importFacts(dataDir, facts); err != nil {
			return r, fail(fmt.Errorf("memory: %w", err))
		}
	}
	if memCopy != "" {
		if err := noteCopy(dataDir, memCopy); err != nil {
			return r, fail(err)
		}
	}
	for _, p := range files {
		var err error
		if p.data != nil {
			err = writeFileAtomic(p.dest, p.data)
		} else {
			err = copyFileAtomic(p.dest, p.src)
		}
		if err != nil {
			return r, fail(err)
		}
	}
	if newConfig != nil {
		if err := writeFileAtomic(cfgPath, newConfig); err != nil {
			return r, fail(err)
		}
		// The record of what the persona asked of the voice settings described
		// the config just replaced (or, restored with the conversations, the
		// old machine's paths): without it the next start tells the persona's
		// values from the user's by the persona files alone.
		if err := forgetPersonaVoice(dataDir); err != nil {
			return r, fail(err)
		}
	}
	if cfg != nil {
		r.Relink = relink(cfg, m, dataDir)
	}
	here, _ := cur["protocol_registry"].(string)
	reconnectPacks(protocolsDir, strings.TrimSpace(here), &r)
	pruneBackups(home)
	return r, nil
}

// noteCopy records a copy of the memory in dataDir kept at path.
func noteCopy(dataDir, path string) error {
	st, err := memory.Open(dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.NoteCopy(path)
}

// forgetPersonaVoice removes the persona voice record from the memory in
// dataDir, if there is one.
func forgetPersonaVoice(dataDir string) error {
	if !exists(filepath.Join(dataDir, "memory.db")) {
		return nil
	}
	st, err := memory.Open(dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.Unset(context.Background(), memory.PersonaVoiceKey)
}

// relink lists the sign-ins that stay with a machine, for the ones this twin
// had and this machine lacks.
func relink(cfg *config.Config, m Manifest, dataDir string) []string {
	had := func(s string) bool { return m.Format < 2 || contains(m.SignedIn, s) }
	var out []string
	if cfg.Channels.WhatsApp.Enabled && had("whatsapp") && !exists(filepath.Join(dataDir, "whatsapp.db")) {
		out = append(out, "WhatsApp: run `mirrin whatsapp login` and scan the code")
	}
	s := cfg.Skills
	if (s.Calendar.Enabled || s.Gmail.Enabled || s.Drive.Enabled) && had("google") && !exists(expandTilde(s.Calendar.TokenFile)) {
		out = append(out, "Google: run `mirrin calendar login`")
	}
	if cfg.Channels.Signal.Enabled {
		out = append(out, "Signal: link signal-cli on this machine to "+cfg.Channels.Signal.Account)
	}
	if cfg.Channels.Voice.Enabled && !exists(expandTilde(cfg.Channels.Voice.WhisperModel)) {
		out = append(out, "Voice: run `mirrin voice setup`")
	}
	if m.Format >= 2 && contains(m.SignedIn, "browser") && !exists(filepath.Join(dataDir, "chrome-profile")) {
		out = append(out, "Websites: sign in again when the twin's browser asks")
	}
	return out
}

// backup saves files an import is about to replace in home/backups/<time>.
// Only the newest few are kept (pruneBackups).
type backup struct{ home, dir string }

func (b *backup) ensure() error {
	if b.dir != "" {
		return nil
	}
	root := filepath.Join(b.home, "backups")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	ts := time.Now().Format("20060102-150405")
	for i := nextBackupIndex(root, ts); ; i++ {
		dir := filepath.Join(root, ts)
		if i > 1 {
			dir = fmt.Sprintf("%s-%d", dir, i)
		}
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			b.dir = dir
			return nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
}

func (b *backup) file(name, src string) error {
	if err := b.ensure(); err != nil {
		return err
	}
	return copyFileAtomic(filepath.Join(b.dir, filepath.FromSlash(name)), src)
}

func (b *backup) db(name, src string) error {
	if err := b.ensure(); err != nil {
		return err
	}
	dest := filepath.Join(b.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	return snapshotDB(src, dest)
}

func (b *backup) facts(name, db string) error {
	data, err := exportFacts(db)
	if err != nil {
		return err
	}
	if err := b.ensure(); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(b.dir, name), data)
}

// packOf returns the pack a path under protocols/ belongs to, if any.
func packOf(rel string) string {
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) == 3 && parts[0] == "packs" {
		return parts[1]
	}
	return ""
}

// isGitPack reports whether a pack is installed here from git.
func isGitPack(protocolsDir, pack string) bool {
	return exists(filepath.Join(protocolsDir, "packs", pack, ".git"))
}

func hasPrefixed(names []string, prefix string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// expandTilde resolves a leading ~ the way config.Load does.
func expandTilde(p string) string {
	if rel, ok := tokenRel(p, "~"); ok {
		if user, err := os.UserHomeDir(); err == nil {
			return filepath.Join(user, filepath.FromSlash(rel))
		}
	}
	return p
}
