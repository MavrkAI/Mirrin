package backup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/config"
	devreg "github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/identity"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// ErrRunning means a twin is running from the home a restore would replace.
var ErrRunning = errors.New("your twin is running here. Quit it from the menu bar (or run `mirrin service stop`), then restore again")

// ErrNoSnapshots means the target holds no snapshot these words open.
var ErrNoSnapshots = errors.New("no backups found there")

// RestoreOptions says what to restore, and where.
type RestoreOptions struct {
	Target Target
	// Name is the snapshot to restore; empty means the newest the words open.
	Name   string
	Phrase Phrase
	// Home is the twin's home on this machine (~/.mirrin).
	Home string
	// Force restores even though Running says a twin is running.
	Force   bool
	Running func() bool
	// WithSessions also restores a WhatsApp session the snapshot holds,
	// and signal-cli's data folder (signal.go).
	WithSessions bool
	// SignalDir is where signal-cli's data folder goes; empty means where
	// the restored config says (SignalDataDir).
	SignalDir string
	// HostLabel names this machine in the handover marker.
	HostLabel string
	Now       func() time.Time
	// Settle, if set, runs once the restored twin is in place, before the
	// handover marker is left, with its data folder: for what this machine
	// made to reach its backups, such as the paid service's link a
	// recovery made, which belongs with the restored twin.
	Settle func(dataDir string) error
}

// Report is what a restore did, for the owner.
type Report struct {
	Manifest Manifest
	Name     string
	// Aside is where the twin that was here went ("" when there was none).
	Aside string
	// Kept lists what stayed from this machine: voice models, sign-ins.
	Kept []string
	// Moved lists settings pointed at their new places.
	Moved []string
	// Devices are the devices that could reach the twin when the snapshot
	// was taken: the owner should revoke any they don't recognise.
	Devices []string
	// Checklist is what to do again on this machine.
	Checklist []string
	// Handover is the marker left for the old machine; HandoverNote says
	// why there is none.
	Handover, HandoverNote string
	// SameMachine is set when the snapshot came from this machine, so no
	// other machine has to stand by.
	SameMachine bool
	// Stale warns when the newest snapshot is old.
	Stale string
	// SessionsLeft is set when the snapshot held a WhatsApp or Signal
	// session that was left out (restore with --with-sessions to bring it).
	SessionsLeft bool
	// Signal is where signal-cli's data folder was restored ("" when it
	// wasn't).
	Signal string
	// Renamed lists files restored under another name, because this
	// system refuses a character in theirs ("a?b.txt is now a_b.txt").
	Renamed []string
}

// Restore replaces the twin in o.Home with a snapshot. Everything is
// decrypted into a staging folder beside the home and checked (every file's
// SHA-256, no path that leaves the folder, an intact memory database) before
// anything changes. The twin that was there is moved to
// <home>.before-restore-<time>, never deleted.
func Restore(ctx context.Context, o RestoreOptions) (Report, error) {
	var r Report
	if o.Running != nil && !o.Force && o.Running() {
		return r, ErrRunning
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	home, err := filepath.Abs(o.Home)
	if err != nil {
		return r, err
	}
	ls, err := List(ctx, o.Target, o.Phrase)
	if err != nil {
		return r, err
	}
	name := o.Name
	if name == "" {
		if name, err = newestAt(ls, o.Target, now()); err != nil {
			return r, err
		}
	}
	r.Name = name
	// Numbering carries on after the highest snapshot there, so a restored
	// twin's next backup never reuses a number.
	var maxSeq int64
	for _, l := range ls {
		if l.Err == nil {
			maxSeq = max(maxSeq, l.Manifest.Seq)
		}
	}
	ts := now().UTC().Format("20060102-150405")

	// 1. Decrypt and check, beside the home, on the same disk.
	if err := os.MkdirAll(filepath.Dir(home), 0o700); err != nil {
		return r, err
	}
	clearStaleStaging(home)
	stage := filepath.Join(filepath.Dir(home), "."+filepath.Base(home)+".restore-"+ts)
	if err := os.Mkdir(stage, 0o700); err != nil {
		return r, err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(stage)
		}
	}()
	rc, err := o.Target.Get(ctx, name)
	if err != nil {
		return r, fmt.Errorf("couldn't open %s in %s (%v)", name, o.Target, err)
	}
	m, renamed, err := extractArchive(rc, o.Phrase.Identities(), stage, windowsNames)
	rc.Close()
	r.Renamed = renamed
	if err != nil {
		return r, err
	}
	r.Manifest = m
	if err := quickCheck(filepath.Join(stage, filepath.FromSlash(nameMemory))); err != nil {
		return r, damaged("the memory in it doesn't pass SQLite's check (%v)", err)
	}
	if !o.WithSessions && exists(filepath.Join(stage, filepath.FromSlash(nameWhatsApp))) {
		if err := os.Remove(filepath.Join(stage, filepath.FromSlash(nameWhatsApp))); err != nil {
			return r, err
		}
		r.SessionsLeft = true
	}
	signalLeft := false
	if !o.WithSessions && exists(filepath.Join(stage, filepath.FromSlash(nameSignal))) {
		if err := os.Remove(filepath.Join(stage, filepath.FromSlash(nameSignal))); err != nil {
			return r, err
		}
		_ = os.Remove(filepath.Join(stage, "sessions"))
		r.SessionsLeft, signalLeft = true, true
	}
	if r.Moved, err = relocateConfig(filepath.Join(stage, nameConfig), m, home, stage); err != nil {
		return r, err
	}

	// 2. Swap: the twin that was here moves aside, the snapshot moves in.
	// Decrypting took a while: make sure no twin started meanwhile.
	if o.Running != nil && !o.Force && o.Running() {
		return r, ErrRunning
	}
	oldData := oldDataDir(home)
	// What this machine keeps rather than the twin (memory/machine.go): its
	// own pause, and runs that fell due before now aren't made up again (the
	// machine the snapshot came from ran them).
	var pausedThere bool
	if staged := filepath.Join(stage, filepath.FromSlash(nameMemory)); exists(staged) {
		carried := memory.MachineState(filepath.Join(oldData, "memory.db"))
		had, err := memory.SettleRestored(staged, carried, now())
		if err != nil {
			return r, fmt.Errorf("couldn't prepare the restored memory (%v); nothing was changed", err)
		}
		pausedThere = had[memory.KeyPaused] != "" && carried[memory.KeyPaused] == ""
	}
	mine := standbyKeys(home, oldData)
	oldState, _ := LoadState(oldData)
	maxSeq = max(maxSeq, oldState.Seq)
	if es, err := os.ReadDir(home); err == nil && len(es) == 0 {
		_ = os.Remove(home) // an empty folder: nothing to keep
	}
	if _, err := os.Lstat(home); err == nil {
		r.Aside = home + ".before-restore-" + ts
		for i := 2; exists(r.Aside); i++ { // two restores within a second
			r.Aside = fmt.Sprintf("%s.before-restore-%s-%d", home, ts, i)
		}
		if err := renameFolder(home, r.Aside); err != nil {
			r.Aside = ""
			return r, fmt.Errorf("couldn't move the twin that's here out of the way (%v); nothing was changed", err)
		}
	}
	if err := renameFolder(stage, home); err != nil {
		if r.Aside != "" {
			if back := renameFolder(r.Aside, home); back != nil {
				return r, fmt.Errorf("couldn't put the restored twin in place (%v); the twin that was here is in %s", err, r.Aside)
			}
			r.Aside = ""
		}
		return r, fmt.Errorf("couldn't put the restored twin in place (%v); nothing was changed", err)
	}
	committed = true
	newData := filepath.Join(home, "data")
	if err := os.MkdirAll(newData, 0o700); err != nil {
		return r, err
	}

	// 3. Keep what belongs to this machine rather than the twin.
	if r.Aside != "" {
		if within(home, oldData) {
			rel, _ := filepath.Rel(home, oldData)
			oldData = filepath.Join(r.Aside, rel)
		}
		r.Kept = carryOver(r.Aside, oldData, home, newData)
		// The twin set aside keeps its memory: forgetting a fact in the
		// restored one scrubs that copy too (memory.NoteCopy).
		if aside := filepath.Join(oldData, "memory.db"); within(r.Aside, aside) && exists(aside) {
			if st, err := memory.Open(newData); err == nil {
				_ = st.NoteCopy(aside)
				st.Close()
			}
		}
	}

	// Signal's session, when the snapshot holds one (signal.go).
	if exists(filepath.Join(home, filepath.FromSlash(nameSignal))) {
		dir := o.SignalDir
		if dir == "" {
			s, _ := LoadSettings(filepath.Join(home, nameConfig))
			dir = SignalDataDir(s)
		}
		if aside, err := restoreSignal(home, dir, now()); err != nil {
			r.Checklist = append(r.Checklist, "Signal's session couldn't be put back ("+plainError(err)+"); it is in "+filepath.Join(home, filepath.FromSlash(nameSignal)))
		} else {
			r.Signal = dir
			if aside != "" {
				r.Kept = append(r.Kept, "Signal's earlier data, in "+aside)
			}
		}
	}

	// 4. A new master key (data/api.token), which only this machine's menu
	// and terminal use; paired devices authenticate with their own keys in
	// devices.json, which came back with the twin (see the device review).
	if err := mintToken(newData); err != nil {
		r.Checklist = append(r.Checklist, "The local API token couldn't be renewed ("+plainError(err)+"); it's made when the twin starts")
	}
	// This machine is the twin as of now: a handover marker older than this
	// (left when the twin moved away from here) no longer applies. The
	// owner just typed the words, so the yearly question counts from now.
	at := now().UTC()
	dismissed := oldState.Dismissed
	if oldState.Standby != nil && !contains(dismissed, oldState.Standby.Name) {
		dismissed = append(dismissed, oldState.Standby.Name)
	}
	_ = SaveState(newData, State{Seq: max(m.Seq, maxSeq), Since: at, Nudged: at, RestoredAt: at, Dismissed: dismissed})

	if o.Settle != nil {
		if err := o.Settle(newData); err != nil {
			r.Checklist = append(r.Checklist, "Some of this machine's settings for its backups couldn't be moved in ("+plainError(err)+")")
		}
	}

	// 5. Tell the machine the snapshot came from to stand by.
	if k := existingStandby(newData); k != nil {
		mine = append(mine, k.Recipient().String())
	}
	switch {
	case m.HandoverTo == "":
		r.HandoverNote = "this snapshot doesn't name the machine it came from"
	case contains(mine, m.HandoverTo):
		r.SameMachine, r.HandoverNote = true, "it came from this machine"
	default:
		host := o.HostLabel
		if host == "" {
			host = HostLabel()
		}
		r.Handover, r.HandoverNote = leaveHandover(ctx, o, filepath.Join(home, nameConfig), m, host, now())
	}
	if !r.SameMachine {
		from := &MovedFrom{Host: m.HostLabel, StandsBy: r.Handover != "", SignIns: contains(m.Excluded, "data/chrome-profile")}
		_ = UpdateState(newData, func(s *State) { s.From = from })
	}

	r.Devices = devices(filepath.Join(newData, "devices.json"))
	r.Checklist = append(checklist(filepath.Join(home, nameConfig), home, r.SessionsLeft, signalLeft, r.Signal != ""), r.Checklist...)
	if pausedThere {
		r.Checklist = append(r.Checklist, "It was paused when this snapshot was taken; here it starts running. Pause it from the menu if you want it paused")
	}
	if age := now().Sub(m.ExportedAt); age > 48*time.Hour {
		r.Stale = fmt.Sprintf("This snapshot is %d days old. If you backed up since then, a newer one is missing from %s.", int(age.Hours()/24), o.Target)
	}
	return r, nil
}

// Newest picks the newest snapshot the words open from a List, or says
// why there is none: the words are wrong, the snapshots are damaged, or
// there are none. A snapshot dated more than a day from now is passed over
// (see Future).
func Newest(ls []Listed, t Target) (string, error) { return newestAt(ls, t, time.Now()) }

func newestAt(ls []Listed, t Target, now time.Time) (string, error) {
	wrong, future := false, 0
	for _, l := range ls {
		if l.Err == nil && Future(l.Manifest, now) {
			future++
			continue
		}
		if l.Err == nil {
			return l.Name, nil
		}
		if errors.Is(l.Err, ErrWrongWords) {
			wrong = true
		}
	}
	switch {
	case future > 0:
		return "", fmt.Errorf("every backup these words open in %s is dated after today; check this computer's date and time, or choose one with --snapshot", t)
	case wrong:
		return "", fmt.Errorf("%w (in %s)", ErrWrongWords, t)
	case len(ls) > 0:
		return "", ls[0].Err
	}
	return "", fmt.Errorf("%w: %s holds no snapshots made with these words", ErrNoSnapshots, t)
}

// Future reports whether a snapshot says it was made more than a day after
// now. Mirrin never writes one; anyone who knows backup.recipient and can
// write to the folder could, and it would stay "the newest" for ever, so a
// restore only takes one when it is named.
func Future(m Manifest, now time.Time) bool { return m.ExportedAt.After(now.Add(24 * time.Hour)) }

// standbyKeys are the standby recipients this machine holds or held: in
// the data folder of the twin in home, and in the twins earlier restores
// here moved aside. Beside the default home, the twin from before the
// rename (~/.antbot, left behind) and its asides count too: they were this
// machine's. A snapshot naming one of them came from this machine.
func standbyKeys(home, dataDir string) []string {
	dirs := []string{dataDir}
	for i, name := range homeNames(home) {
		if i > 0 {
			dirs = append(dirs, filepath.Join(filepath.Dir(home), name, "data"))
		}
		asides, _ := filepath.Glob(filepath.Join(filepath.Dir(home), name) + ".before-restore-*")
		for _, a := range asides {
			dirs = append(dirs, filepath.Join(a, "data"))
		}
	}
	var out []string
	for _, d := range dirs {
		if k := existingStandby(d); k != nil {
			out = append(out, k.Recipient().String())
		}
	}
	return out
}

// clearStaleStaging removes the decrypted copies restores that died midway
// left beside home (.<home>.restore-…, and AntBot's beside its home), once
// they are an hour old: a restore running now has a newer one.
func clearStaleStaging(home string) {
	for _, name := range homeNames(home) {
		old, _ := filepath.Glob(filepath.Join(filepath.Dir(home), "."+name+".restore-*"))
		for _, d := range old {
			if st, err := os.Lstat(d); err == nil && st.IsDir() && time.Since(st.ModTime()) > time.Hour {
				_ = os.RemoveAll(d)
			}
		}
	}
}

// isDefaultHome is config.IsDefaultHome; tests point it at a temporary home.
var isDefaultHome = config.IsDefaultHome

// homeNames are the names of home and of the homes beside it that held this
// machine's twin before: home's own first, then, for the default home, the
// homes from before the rename (.antbot, .openhuman).
func homeNames(home string) []string {
	names := []string{filepath.Base(home)}
	if !isDefaultHome(home) {
		return names
	}
	for _, n := range append([]string{brand.HomeDirName}, brand.LegacyHomeDirs...) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

// leaveHandover leaves the marker where the machine the snapshot came from
// looks for one: the target its config names (the config just restored),
// and the one this restore read from when that is another, such as a copy
// on a USB disk. It returns the marker's name, or why the old machine
// won't see one.
func leaveHandover(ctx context.Context, o RestoreOptions, cfgPath string, m Manifest, host string, now time.Time) (string, string) {
	write := func(t Target) (string, error) {
		return writeHandover(ctx, t, o.Phrase, m.HandoverTo, host, m.Seq, now)
	}
	watched, where := o.Target, o.Target.String()
	var openErr error
	if s, err := LoadSettings(cfgPath); err == nil && s.Recipient != "" {
		where = Where(s)
		watched, openErr = OpenTarget(s, o.Phrase.Namespace())
	}
	if openErr == nil && sameTarget(watched, o.Target) {
		// The target as the restored twin opens it: for the paid service,
		// with the link Settle moved in.
		n, err := write(watched)
		if err != nil {
			return "", "Mirrin couldn't leave it a note in " + where + ": " + plainError(err)
		}
		return n, ""
	}
	_, _ = write(o.Target) // the copy gets one too; the old machine doesn't look there
	if openErr != nil {
		return "", "Mirrin couldn't leave it a note in " + where + ", where it looks for one: " + plainError(openErr)
	}
	n, err := write(watched)
	if err != nil {
		return "", "Mirrin couldn't leave it a note in " + where + ", where it looks for one: " + plainError(err)
	}
	return n, ""
}

// sameTarget reports whether two targets keep their objects in one place.
func sameTarget(a, b Target) bool {
	fa, okA := a.(*folder)
	fb, okB := b.(*folder)
	if okA && okB {
		return realPath(fa.dir) == realPath(fb.dir)
	}
	return a.String() == b.String()
}

// quickCheck runs SQLite's quick_check on a database, if it is there.
func quickCheck(p string) error {
	if !exists(p) {
		return nil
	}
	db, err := sql.Open("sqlite", memory.FileURI(p)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New(res)
	}
	return nil
}

// oldDataDir is the data folder of the twin in home, from its config.
func oldDataDir(home string) string {
	_, dir := identity.LocalAPI(home)
	return dir
}

// carryOver moves into the restored home what a snapshot leaves out because
// it belongs to this machine: voice models, logs, the browser profile, the
// WhatsApp link, the saved connection to a twin elsewhere, and the standby
// key every snapshot this machine took names. On a new machine there is
// nothing to move.
func carryOver(oldHome, oldData, home, newData string) []string {
	var kept []string
	move := func(from, to, label string) {
		if !exists(from) || exists(to) {
			return
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return
		}
		if os.Rename(from, to) == nil && label != "" && !contains(kept, label) {
			kept = append(kept, label)
		}
	}
	for _, d := range []struct{ name, label string }{{"models", "voice models"}, {"tts", "voice models"}, {"sounds", ""}, {"logs", ""}} {
		from, to := filepath.Join(oldHome, d.name), filepath.Join(home, d.name)
		if !exists(to) {
			move(from, to, d.label)
			continue
		}
		// The snapshot brought part of it (trained wake models): add the rest.
		es, _ := os.ReadDir(from)
		for _, e := range es {
			move(filepath.Join(from, e.Name()), filepath.Join(to, e.Name()), d.label)
		}
	}
	move(filepath.Join(oldHome, "remote.yaml"), filepath.Join(home, "remote.yaml"), "the saved connection to your other twin")
	move(filepath.Join(oldData, standbyKeyFile), filepath.Join(newData, standbyKeyFile), "")
	move(filepath.Join(oldData, "chrome-profile"), filepath.Join(newData, "chrome-profile"), "website sign-ins")
	if !exists(filepath.Join(newData, "whatsapp.db")) {
		for _, f := range []string{"whatsapp.db", "whatsapp.db-wal", "whatsapp.db-shm"} {
			move(filepath.Join(oldData, f), filepath.Join(newData, f), "the WhatsApp link")
		}
	}
	return kept
}

// mintToken writes a fresh local API token, as api.LoadOrCreateToken would.
func mintToken(dataDir string) error {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dataDir, "api.token"), []byte(hex.EncodeToString(buf)+"\n"))
}

// relocateConfig points the restored config at where things now are: the
// folders and sign-in files restored into home, and paths under the old home
// or the old user's home moved to this machine's. It changes nothing when
// the snapshot comes from this home.
func relocateConfig(path string, m Manifest, home, stage string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, nil // restored as it was; `mirrin doctor` will say what's wrong with it
	}
	root := doc.Content[0]
	var moved []string
	changed := false
	set := func(keys []string, to string) {
		n := find(root, keys)
		if n == nil || n.Kind != yaml.ScalarNode || n.Value == "" || n.Value == to {
			return
		}
		if !strings.HasPrefix(n.Value, "~") && !within(m.Home, n.Value) {
			moved = append(moved, fmt.Sprintf("%s: %s is now %s", strings.Join(keys, "."), n.Value, to))
		}
		n.Value, n.Style, changed = to, 0, true
	}
	has := func(name string) bool { return exists(filepath.Join(stage, filepath.FromSlash(name))) }
	set([]string{"data_dir"}, filepath.Join(home, "data"))
	set([]string{"protocols_dir"}, filepath.Join(home, "protocols"))
	set([]string{"tools_dir"}, filepath.Join(home, "tools"))
	if has(nameToken) {
		set([]string{"skills", "calendar", "token_file"}, filepath.Join(home, nameToken))
	}
	if has(nameCreds) {
		set([]string{"skills", "calendar", "credentials_file"}, filepath.Join(home, nameCreds))
	}
	if n := find(root, []string{"channels", "voice", "wake_model"}); n != nil && filepath.IsAbs(n.Value) && !within(m.Home, n.Value) && has("tts/"+filepath.Base(n.Value)) {
		set([]string{"channels", "voice", "wake_model"}, filepath.Join(home, "tts", filepath.Base(n.Value)))
	}
	// A config from before settings were saved in layers has the zone the
	// old machine's system had at install written in; there, one equal to
	// the system's zone followed the system (config.upgrade). Here, whose
	// system zone may be another, it keeps doing so.
	if m.Zone != "" && layeredVersionOf(root) < config.LayeredVersion {
		if tz := find(root, []string{"user", "timezone"}); tz != nil && tz.Kind == yaml.ScalarNode && strings.TrimSpace(tz.Value) == m.Zone {
			tz.Value, tz.Style, changed = "Local", 0, true
		}
	}
	// A config from before the rename names the home as ~/.antbot (copied
	// from the example config, say): that is this home now.
	for _, d := range brand.LegacyHomeDirs {
		for _, sep := range []string{"/", `\`} {
			if config.RebasePaths(root, "~"+sep+d, home) {
				changed = true
			}
		}
	}
	user, _ := os.UserHomeDir()
	walkScalars(root, func(n *yaml.Node) {
		for _, pair := range [][2]string{{m.Home, home}, {m.UserHome, user}} {
			from, to := pair[0], pair[1]
			if from == "" || to == "" || from == to {
				continue
			}
			if n.Value == from || strings.HasPrefix(n.Value, from+string(filepath.Separator)) || strings.HasPrefix(n.Value, from+"/") {
				n.Value, changed = to+n.Value[len(from):], true
				return
			}
		}
	})
	if !changed {
		return moved, nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}
	return moved, writeFileAtomic(path, out)
}

// layeredVersionOf is a config document's config_version (0 for a full
// dump from before settings were layered).
func layeredVersionOf(root *yaml.Node) int {
	v := find(root, []string{"config_version"})
	if v == nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(v.Value))
	return n
}

// find returns the value node at keys in a mapping, or nil.
func find(n *yaml.Node, keys []string) *yaml.Node {
	for _, k := range keys {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == k {
				next = n.Content[i+1]
			}
		}
		n = next
	}
	return n
}

// walkScalars calls fn for every string value (not keys) under n.
func walkScalars(n *yaml.Node, fn func(*yaml.Node)) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			walkScalars(n.Content[i], fn)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			walkScalars(c, fn)
		}
	case yaml.ScalarNode:
		if n.Tag == "" || n.Tag == "!!str" {
			fn(n)
		}
	}
}

// devices lists the paired devices a restored registry (devices.json) still
// lets in, each with the id `mirrin devices revoke` takes. Devices already
// cut off, and browsers on the old computer itself (which only its own
// loopback address accepted), can't reach the twin and aren't listed.
func devices(p string) []string {
	if !exists(p) {
		return nil
	}
	reg, err := devreg.Open(p)
	if err != nil {
		return []string{"(devices.json doesn't read; run `mirrin devices` once your twin is running)"}
	}
	var out []string
	for _, d := range reg.List() {
		if d.Revoked() || d.Local() {
			continue
		}
		id := d.ID
		if len(id) > 8 {
			id = id[:8]
		}
		out = append(out, fmt.Sprintf("%s (%s)", d.Name, id))
	}
	sort.Strings(out)
	return out
}

// checklist is what to set up again on this machine, from the restored
// config and what the restored home holds.
func checklist(cfgPath, home string, sessionsLeft, signalLeft, signalBack bool) []string {
	cfg := config.Default()
	if b, err := os.ReadFile(cfgPath); err == nil {
		_ = yaml.Unmarshal(b, cfg)
	}
	data := cfg.DataDir
	if data == "" || !exists(data) {
		data = filepath.Join(home, "data")
	}
	var out []string
	if cfg.Channels.WhatsApp.Enabled && !exists(filepath.Join(data, "whatsapp.db")) {
		s := "WhatsApp: run `mirrin whatsapp login` and scan the code"
		if sessionsLeft {
			s += " (or restore again with --with-sessions, once WhatsApp is off on the old machine)"
		}
		out = append(out, s)
	}
	if cfg.Skills.Browser.Enabled && !exists(filepath.Join(data, "chrome-profile")) {
		out = append(out, "Websites: sign in again when the twin's browser asks")
	}
	s := cfg.Skills
	if (s.Calendar.Enabled || s.Gmail.Enabled || s.Drive.Enabled) && !exists(s.Calendar.TokenFile) {
		out = append(out, "Google: run `mirrin calendar login`")
	}
	if cfg.Channels.Signal.Enabled && !signalBack {
		s := "Signal: link signal-cli on this machine to " + cfg.Channels.Signal.Account
		if signalLeft {
			s += " (or restore again with --with-sessions, once Signal is off on the old machine)"
		}
		out = append(out, s)
	}
	if (cfg.Channels.Voice.Enabled || cfg.Channels.Voice.Mode == "wake") && !exists(cfg.Channels.Voice.WhisperModel) {
		out = append(out, "Voice: run `mirrin voice setup`")
	}
	if cfg.API.Remote {
		// Paired devices keep their own keys (data/devices.json came back
		// with the twin); only this machine's menu and terminal use the
		// master key, which is new. The device list above is the safeguard.
		out = append(out, "Other devices keep their own keys: point them at this machine's address, and cut off any you don't recognise above with `mirrin devices revoke <id>`")
	}
	return out
}
