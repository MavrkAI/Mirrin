package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/homelock"
)

// homeVars name the twin's home, in order (HomeEnv): MIRRIN_HOME, then the
// names AntBot and openHuman gave it.
var homeVars = brand.EnvAliases("MIRRIN_HOME")

// userHomeDir is the user's own home folder. Tests point it at a temporary
// one, so they never look at the contributor's real homes.
var userHomeDir = os.UserHomeDir

// homeMemo is the home this process settled on, for the environment it had.
var homeMemo struct {
	sync.Mutex
	key, dir string
}

// Home returns the Mirrin home directory: the folder MIRRIN_HOME names (or
// ANTBOT_HOME or OPENHUMAN_HOME, from before the rename), else ~/.mirrin.
// The first call moves a home from before the rename into place
// (migrateHome). The answer then holds for the rest of the process, so a
// twin that found the old home in use never switches homes halfway.
func Home() string {
	key := homeKey()
	homeMemo.Lock()
	defer homeMemo.Unlock()
	if homeMemo.dir == "" || homeMemo.key != key {
		homeMemo.key, homeMemo.dir = key, resolveHome(true)
	}
	return homeMemo.dir
}

// CurrentHome is where the twin is right now: Home() without moving a home
// from before the rename into place. It moves nothing and doesn't settle
// Home(), so keys can be saved into an old home before it moves.
func CurrentHome() string { return resolveHome(false) }

// HomeEnv is the home a set of environment variables names: the first of
// MIRRIN_HOME, ANTBOT_HOME and OPENHUMAN_HOME that names one. One naming a
// default home (~/.mirrin, ~/.antbot or ~/.openhuman) counts as not set:
// every service AntBot installed spells out ANTBOT_HOME=~/.antbot, and that
// twin still moves to ~/.mirrin. "" means the default home.
func HomeEnv(getenv func(string) string) string {
	for _, k := range homeVars {
		if h := getenv(k); h != "" && !IsDefaultHome(h) {
			return h
		}
	}
	return ""
}

// homeKey is what Home's answer depends on.
func homeKey() string {
	parts := make([]string, 0, len(homeVars)+1)
	for _, k := range homeVars {
		parts = append(parts, os.Getenv(k))
	}
	user, _ := userHomeDir()
	return strings.Join(append(parts, user), "\x00")
}

func resolveHome(move bool) string {
	if h := HomeEnv(os.Getenv); h != "" {
		return h
	}
	user, err := userHomeDir()
	if err != nil || user == "" {
		return brand.HomeDirName
	}
	dir := filepath.Join(user, brand.HomeDirName)
	var legacy []string
	for _, d := range brand.LegacyHomeDirs {
		legacy = append(legacy, filepath.Join(user, d))
	}
	if move {
		return migrateHome(legacy, dir)
	}
	if !mayMigrateHome() || looksLikeTwin(dir) {
		return dir
	}
	if old := firstTwin(legacy); old != "" {
		return old
	}
	return dir
}

// IsDefaultHome reports whether dir is a home's default place: ~/.mirrin,
// or ~/.antbot or ~/.openhuman from before the rename.
func IsDefaultHome(dir string) bool {
	user, err := userHomeDir()
	if err != nil || user == "" {
		return false
	}
	for _, d := range append([]string{brand.HomeDirName}, brand.LegacyHomeDirs...) {
		if samePath(dir, filepath.Join(user, d)) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	return pathsEqual(filepath.Clean(a), filepath.Clean(b))
}

// How paths compare here: on Windows and macOS (whose disks ignore case
// unless set up otherwise) case doesn't count, and on Windows / and \ are
// alike. Tests set them anywhere.
var (
	foldCase    = runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	foldSlashes = runtime.GOOS == "windows"
)

// pathsEqual reports whether two paths, as written, name the same place.
func pathsEqual(a, b string) bool {
	if foldSlashes {
		a, b = strings.ReplaceAll(a, `\`, "/"), strings.ReplaceAll(b, `\`, "/")
	}
	if foldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// migrateInTests lets a test opt in to moving a legacy home. Under `go test`
// the move is otherwise off, so a test that reaches Home() without its own
// MIRRIN_HOME can never move a contributor's real twin.
var migrateInTests bool

// mayMigrateHome reports whether migrateHome may move a legacy home now.
func mayMigrateHome() bool {
	return migrateInTests || !testing.Testing()
}

// HoldHomeMove, when set, names what still needs a home from before the
// rename where it is, or says "": main sets it to the check for an old
// background service that runs an older program, which would start an
// empty twin in the old place at the next login if the home moved now.
// While it names one, the home stays put.
var HoldHomeMove func() string

// migrateHome moves the first home in legacy that holds a twin (~/.antbot,
// else ~/.openhuman) to dir (~/.mirrin), once, and returns the home to use.
// The old home stays where it is, and is the one to use, while a twin still
// runs from it (it holds the claim on its data folder: moving the folder
// under it would split the twin in two), while an old background service
// would still start in it (HoldHomeMove), and when the move fails. A dir
// that holds no twin, such as an empty folder made early, doesn't stop the
// move. Paths in the moved config.yaml that pointed into the old home are
// pointed into the new one, or the move is undone; the names of variables
// in it stay as they are.
func migrateHome(legacy []string, dir string) string {
	if !mayMigrateHome() || looksLikeTwin(dir) {
		return dir
	}
	old := firstTwin(legacy)
	if old == "" {
		return dir
	}
	if homelock.InUse(DataDirIn(old)) {
		fmt.Fprintf(os.Stderr, "A twin is still running from %s, so Mirrin uses that folder until it quits, then moves it to %s.\n", tilde(old), tilde(dir))
		return old
	}
	if HoldHomeMove != nil {
		if what := HoldHomeMove(); what != "" {
			fmt.Fprintf(os.Stderr, "%s still starts from %s, so Mirrin keeps using that folder until `mirrin service install` replaces it.\n", what, tilde(old))
			return old
		}
	}
	// Work out the new config.yaml first: a move that would leave it
	// pointing into the old folder doesn't start.
	rebase, err := planRebase(filepath.Join(old, "config.yaml"), old, homePrefixes(old, dir))
	if err == nil {
		err = removeEmptyDirs(dir)
	}
	if err == nil {
		err = os.Rename(old, dir)
	}
	if le, ok := err.(*os.LinkError); ok {
		err = le.Err // the paths are in the message already
	}
	if err == nil {
		if err = rebase.write(dir); err != nil && os.Rename(dir, old) != nil {
			fmt.Fprintf(os.Stderr, "warning: moved %s to %s, but couldn't update the paths in its config.yaml (%v); check the ones that pointed into %s\n", tilde(old), tilde(dir), err, tilde(old))
			return dir
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't move %s to %s (%v), so Mirrin keeps using %s.\n", tilde(old), tilde(dir), err, tilde(old))
		return old
	}
	fmt.Fprintf(os.Stderr, "moved %s to %s (%s is now called %s)\n", tilde(old), tilde(dir), brand.LegacyDisplayName(filepath.Base(old)), brand.DisplayName)
	return dir
}

// looksLikeTwin reports whether dir holds a twin: its settings, keys or data.
func looksLikeTwin(dir string) bool {
	for _, n := range []string{"config.yaml", "secrets.env", "data"} {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			return true
		}
	}
	return false
}

// firstTwin is the first of dirs that holds a twin, or "".
func firstTwin(dirs []string) string {
	for _, d := range dirs {
		if looksLikeTwin(d) {
			return d
		}
	}
	return ""
}

// removeEmptyDirs removes dir if it holds nothing but empty folders and
// empty lock files (config.yaml.lock, left by an edit that found no twin),
// and says what is in the way otherwise. A dir that isn't there is fine.
func removeEmptyDirs(dir string) error {
	es, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range es {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if err := removeEmptyDirs(p); err != nil {
				return err
			}
			continue
		}
		if !emptyLock(e) {
			return fmt.Errorf("%s is in the way", filepath.Join(tilde(dir), e.Name()))
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return os.Remove(dir)
}

// emptyLock reports whether e is an empty lock file, which holds nothing.
func emptyLock(e os.DirEntry) bool {
	if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".lock") {
		return false
	}
	info, err := e.Info()
	return err == nil && info.Size() == 0
}

// DataDirIn is the data folder of the twin in home: its config.yaml's
// data_dir, else home/data. Only that one setting is read, so a config
// this version can't load still answers.
func DataDirIn(home string) string {
	var c struct {
		DataDir string `yaml:"data_dir"`
	}
	if b, err := os.ReadFile(filepath.Join(home, "config.yaml")); err == nil {
		_ = yaml.Unmarshal(b, &c)
	}
	if d := strings.TrimSpace(c.DataDir); d != "" {
		return expandHome(d)
	}
	return filepath.Join(home, "data")
}

// tilde writes a path in the user's home as ~/…, for messages.
func tilde(p string) string {
	user, err := userHomeDir()
	if err != nil || user == "" {
		return p
	}
	rel, err := filepath.Rel(user, p)
	if err == nil && rel == "." {
		return "~"
	}
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return "~" + string(filepath.Separator) + rel
	}
	return p
}
