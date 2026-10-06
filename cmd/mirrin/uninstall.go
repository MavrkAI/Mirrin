package main

// mirrin uninstall takes Mirrin off this machine: the background service (and
// AntBot's or openHuman's, from before the rename), the program where the
// installers put it, Mirrin.app, and leftovers of updates, under the old
// names too. Only what is Mirrin's goes: programs that answer as Mirrin (or
// as AntBot or openHuman did), apps with its bundle identifiers, and nothing
// another twin's service still runs. The twin itself, in ~/.mirrin,
// is kept unless its owner types "delete", and a copy is offered first.

import (
	"bufio"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/identity"
	"github.com/MavrkAI/Mirrin/internal/service"
)

const uninstallUsage = "usage: mirrin uninstall [--yes]"

// uninstallServices is what uninstalling needs from the service manager.
type uninstallServices interface {
	Installed() bool // this home's Mirrin service
	Remove() error
	LegacyInstalled() bool // AntBot's or openHuman's, from before the rename
	RemoveLegacy(out io.Writer) error
	// Foreign is a background service installed for another twin (another
	// MIRRIN_HOME): where its definition is, and the program it keeps
	// running ("" when that can't be read).
	Foreign() (unit, program string, ok bool)
}

type uninstaller struct {
	out      io.Writer
	ask      *prompter // nil without a terminal
	yes      bool      // --yes: remove the program and the service, keep the twin, ask nothing
	home     string    // the twin: ~/.mirrin
	dataDir  string    // its memory, which a config can put outside home
	userHome string    // where a copy of the twin is saved
	goos     string
	exe      string   // this program, links resolved
	others   []string // other mirrin programs on PATH, under the old names too, links resolved
	appDirs  []string // where Mirrin.app may be (macOS)
	svc      uninstallServices
	// command runs `<program> version`, which tells Mirrin's programs from
	// others that share a name.
	command func(ctx context.Context, name string, args ...string) (string, error)
	pathEnv string      // PATH, to point out what the installers added to it
	zdotdir string      // $ZDOTDIR, where zsh keeps .zshrc
	running func() bool // a twin is running from this home
	export  func(file string) error
	// removeRunning deletes a program that is still running, once it exits
	// (Windows can't delete a running .exe).
	removeRunning func(path string) error
	startDetached func(name string, args ...string) error
	today         func() time.Time
	wait          time.Duration // how long a stopped twin gets to exit
	// backups is where encrypted backups go when they are set up ("" if
	// not): they outlive the uninstall, keys and sign-ins included.
	backups string
}

// removal is one thing uninstalling deletes.
type removal struct {
	path, label string
	inno        bool // a Windows setup folder, removed by its own uninstaller
	onPath      bool // a program an installer put on PATH, in its folder
}

func uninstallCmd(args []string) error {
	yes := false
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			yes = true
		default:
			return fmt.Errorf("unknown option %s\n%s", a, uninstallUsage)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	home := config.Home()
	_, dataDir := identity.LocalAPI(home)
	userHome, _ := os.UserHomeDir()
	backups := ""
	if s, err := backup.LoadSettings(filepath.Join(home, "config.yaml")); err == nil && s.Recipient != "" {
		backups = backup.Where(s)
	}
	u := &uninstaller{
		out: os.Stdout, yes: yes, home: home, dataDir: dataDir, userHome: userHome,
		goos: runtime.GOOS, exe: exe, svc: localServices{}, command: runCommand,
		pathEnv: os.Getenv("PATH"), zdotdir: os.Getenv("ZDOTDIR"),
		running: twinRunning,
		export: func(file string) error {
			_, err := identity.Export(home, file, true)
			return err
		},
		removeRunning: removeAfterExit, startDetached: startDetached, today: time.Now, wait: 10 * time.Second,
		backups: backups,
	}
	if isTerminal() {
		u.ask = newPrompter(os.Stdin, os.Stdout)
	}
	for _, name := range append([]string{brand.Name}, brand.LegacyNames...) {
		if p, err := exec.LookPath(name); err == nil {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				u.others = append(u.others, r)
			}
		}
	}
	if runtime.GOOS == "darwin" {
		u.appDirs = []string{"/Applications"}
		if userHome != "" {
			u.appDirs = append(u.appDirs, filepath.Join(userHome, "Applications"))
		}
	}
	return u.run()
}

func (u *uninstaller) say(format string, a ...any) { fmt.Fprintf(u.out, format+"\n", a...) }

// Where Mirrin's apps are, by name, and their bundle identifiers: the
// current one first, then those from before the rename. An app that only
// shares the name (AntBot and OpenHuman are other products' names too) is
// someone else's and stays.
var appNames = []struct{ name, id, label string }{
	{"Mirrin.app", "com.mirrin.mavrk", "the app"},
	{"AntBot.app", "com.antbot.mavrk", "the old AntBot app"}, // rename:keep
	{"OpenHuman.app", "com.openhuman.mavrk", "the old openHuman app"},
}

// updateLeftovers are what an interrupted update leaves beside the program,
// and what Windows updates move aside, under each name the program has had.
func updateLeftovers() []string {
	var out []string
	for _, n := range append([]string{brand.Name}, brand.LegacyNames...) {
		out = append(out, "."+n+".new", n+".new.exe", n+".old*.exe")
	}
	return out
}

// plan lists what will be removed, and what can only be removed another way.
// shared is set when another twin's service keeps Mirrin's other copies.
func (u *uninstaller) plan() (items []removal, notes []string, shared bool) {
	seen := map[string]bool{}
	add := func(r removal) {
		if !seen[r.path] {
			seen[r.path] = true
			items = append(items, r)
		}
	}
	leftovers := func() {
		// What an interrupted update leaves beside the program, and what
		// Windows updates move aside.
		dir := filepath.Dir(u.exe)
		for _, pattern := range updateLeftovers() {
			matches, _ := filepath.Glob(filepath.Join(dir, pattern))
			for _, p := range matches {
				if fileExists(p) {
					add(removal{path: p, label: "a leftover from an update"})
				}
			}
		}
	}

	if unit, program, ok := u.svc.Foreign(); ok {
		// Another twin's service keeps a copy of Mirrin running, so the
		// programs and apps stay; only this program goes, when it isn't
		// that copy.
		inno := u.goos == "windows" && fileExists(filepath.Join(filepath.Dir(u.exe), "unins000.exe"))
		if program != "" && !sameFile(u.exe, program) && appBundle(u.exe) == "" && !homebrewPath(u.exe) && !inno {
			add(removal{path: u.exe, label: "the program", onPath: true})
			leftovers()
		}
		if program == "" {
			program = "a copy of Mirrin"
		}
		notes = append(notes, fmt.Sprintf("Another Mirrin twin's background service (%s) still runs %s, so that and Mirrin's other copies stay. To remove them, run `mirrin uninstall` from that twin (without MIRRIN_HOME).", unit, program))
		return items, notes, true
	}

	program := func(p string) {
		old := oldProgramName(p)
		switch {
		case homebrewPath(p):
			note := "Homebrew installed " + p + ". Remove it with: brew uninstall " + strings.TrimSuffix(filepath.Base(p), ".exe")
			if !seen[note] {
				seen[note] = true
				notes = append(notes, note)
			}
		case appBundle(p) != "":
			add(removal{path: appBundle(p), label: "the app"})
		case u.goos == "windows" && fileExists(filepath.Join(filepath.Dir(p), "unins000.exe")):
			add(removal{path: filepath.Dir(p), label: "the Mirrin folder, with its own uninstaller", inno: true, onPath: true})
		case old != "":
			add(removal{path: p, label: "the old " + brand.LegacyDisplayName(old) + " program", onPath: true})
		default:
			add(removal{path: p, label: "the program", onPath: true})
		}
	}
	program(u.exe)
	for _, p := range u.others {
		if !seen[p] && !sameFile(p, u.exe) && fileExists(p) && u.ours(p) {
			program(p)
		}
	}
	leftovers()
	for _, d := range u.appDirs {
		for _, a := range appNames {
			if p := filepath.Join(d, a.name); isDir(p) && bundleID(u.command, p) == a.id {
				add(removal{path: p, label: a.label})
			}
		}
	}
	return items, notes, false
}

// ours reports whether a program found on PATH is Mirrin, or AntBot or
// openHuman from before the rename, rather than another program with the
// same name: asked its version, it must answer as Mirrin does ("mirrin
// v0.3.0", "antbot v0.3.0" or "openhuman dev").
func (u *uninstaller) ours(p string) bool {
	name, v := reportedVersion(context.Background(), u.command, p, 5*time.Second)
	return ourName(name) && (v == "dev" || strings.HasPrefix(v, "v"))
}

// oldProgramName is the name from before the rename a program is called by
// ("antbot" for antbot.exe), or "" for Mirrin's own name and any other.
func oldProgramName(p string) string {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(p)), ".exe")
	for _, n := range brand.LegacyNames {
		if strings.HasPrefix(base, n) {
			return n
		}
	}
	return ""
}

// bundleID is a macOS app's CFBundleIdentifier, or "". command reads a
// binary plist (with plutil).
func bundleID(command func(ctx context.Context, name string, args ...string) (string, error), app string) string {
	plist := filepath.Join(app, "Contents", "Info.plist")
	b, err := os.ReadFile(plist)
	if err != nil {
		return ""
	}
	if strings.HasPrefix(string(b), "bplist") {
		// A binary plist: plutil reads it.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := command(ctx, "plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-", plist)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	}
	return plistString(b, "CFBundleIdentifier")
}

// plistString is the string a top-level key of an XML plist holds, or "".
func plistString(b []byte, key string) string {
	dec := xml.NewDecoder(strings.NewReader(string(b)))
	depth, lastKey := 0, ""
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			text.Reset()
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			switch {
			case t.Name.Local == "key" && depth == 3: // plist > dict > key
				lastKey = strings.TrimSpace(text.String())
			case t.Name.Local == "string" && depth == 3 && lastKey == key:
				return strings.TrimSpace(text.String())
			case depth == 3:
				lastKey = ""
			}
			depth--
		}
	}
}

// twinFolderProblem says why the twin's folder isn't one to delete as a
// whole, or "". MIRRIN_HOME set to the home folder, or to /, would otherwise
// take everything in it along.
func (u *uninstaller) twinFolderProblem() string {
	h := filepath.Clean(u.home)
	switch {
	case !filepath.IsAbs(h):
		return "isn't a full path"
	case filepath.Dir(h) == h:
		return "is a whole disk"
	case u.userHome != "" && within(filepath.Clean(u.userHome), h):
		return "holds your home folder"
	}
	for _, name := range []string{"config.yaml", "secrets.env", "data"} {
		if _, err := os.Stat(filepath.Join(h, name)); err == nil {
			return ""
		}
	}
	return "doesn't look like a Mirrin twin (it has no config.yaml, secrets.env or data)"
}

// pathNotes says where the installers put a folder Mirrin was removed from
// on PATH, since that stays: install.sh adds a line to the shell's startup
// file, and install.ps1 and the setup program add it to the user's Path.
func (u *uninstaller) pathNotes(dirs []string) []string {
	var notes []string
	if u.goos == "windows" {
		for _, d := range dirs {
			for _, p := range strings.Split(u.pathEnv, ";") {
				if strings.EqualFold(strings.TrimRight(p, `\/`), strings.TrimRight(d, `\/`)) {
					notes = append(notes, fmt.Sprintf("%s is still on your PATH. If nothing else you use is in that folder, remove it: search the Start menu for \u201cEdit environment variables for your account\u201d, open Path and delete that entry.", d))
					break
				}
			}
		}
		return notes
	}
	for _, a := range u.installerPaths(dirs) {
		notes = append(notes, installerPathNote(a))
	}
	return notes
}

// installerMarkers are the comment lines install.sh writes above its PATH
// line: Mirrin's, and AntBot's from before the rename.
var installerMarkers = []string{
	"# Added by the Mirrin installer",
	"# Added by the AntBot installer", // rename:keep
}

// pathAdd is a line install.sh added to a shell startup file, below marker.
type pathAdd struct{ rc, dir, marker string }

// installer is who added a line, in the owner's words: "The Mirrin installer".
func (a pathAdd) installer() string {
	return "The " + strings.TrimSuffix(strings.TrimPrefix(a.marker, "# Added by the "), " installer") + " installer"
}

func installerPathNote(a pathAdd) string {
	return fmt.Sprintf("%s put %s on your PATH in %s. If nothing else you use is in that folder, delete the line \u201c%s\u201d there and the line after it.", a.installer(), a.dir, a.rc, a.marker)
}

// installerPaths are the lines install.sh added to put dirs on PATH, in the
// shell startup files it writes to.
func (u *uninstaller) installerPaths(dirs []string) []pathAdd {
	zsh := u.userHome
	if u.zdotdir != "" {
		zsh = u.zdotdir
	}
	rcs := []string{filepath.Join(zsh, ".zshrc")}
	for _, f := range []string{".bashrc", ".bash_profile", ".bash_login", ".profile", filepath.Join(".config", "fish", "config.fish")} {
		rcs = append(rcs, filepath.Join(u.userHome, f))
	}
	var out []pathAdd
	for _, rc := range rcs {
		for _, d := range dirs {
			if marker := installerAddedPath(rc, d); marker != "" {
				out = append(out, pathAdd{rc, d, marker})
			}
		}
	}
	return out
}

// tidyPath deals with what the installers added to PATH for the folders
// Mirrin was removed from. Asked in a terminal (never with --yes), it offers
// to remove install.sh's lines, keeping a backup of the file first;
// otherwise it says where they are.
func (u *uninstaller) tidyPath(dirs []string) {
	if u.goos == "windows" || u.ask == nil || u.yes {
		for _, n := range u.pathNotes(dirs) {
			u.say("%s", n)
		}
		return
	}
	backedUp := map[string]bool{} // a second line in the same file keeps the first copy
	for _, a := range u.installerPaths(dirs) {
		backup := a.rc + ".mirrin-backup"
		if confirm(u.ask, fmt.Sprintf("%s put %s on your PATH in %s. Remove that line? (A copy of the file is kept in %s.)", a.installer(), a.dir, a.rc, backup), false) {
			keep := backup
			if backedUp[a.rc] {
				keep = ""
			}
			err := removeInstallerPath(a.rc, a.dir, keep)
			if err == nil {
				backedUp[a.rc] = true
			}
			if err != nil {
				u.say("Couldn't change %s (%v). Delete the line \u201c%s\u201d there and the line after it yourself.", a.rc, err, a.marker)
				continue
			}
			u.say("Removed it from %s; the file as it was is in %s. Open a new terminal for the change to take effect.", a.rc, backup)
			continue
		}
		u.say("%s", installerPathNote(a))
	}
}

// removeInstallerPath deletes, from rc, install.sh's marker line (Mirrin's
// or AntBot's) and the line after it that puts dir on PATH (and the blank
// line it wrote before them), after copying rc to backup (unless backup is
// ""). Everything else stays as it was, and the file keeps its mode. When
// rc is a symlink (into a dotfiles repo, say), the file it points to
// changes and the link stays.
func removeInstallerPath(rc, dir, backup string) error {
	if real, err := filepath.EvalSymlinks(rc); err == nil {
		rc = real
	}
	b, err := os.ReadFile(rc)
	if err != nil {
		return err
	}
	st, err := os.Stat(rc)
	if err != nil {
		return err
	}
	if backup != "" {
		if err := os.WriteFile(backup, b, st.Mode().Perm()); err != nil {
			return fmt.Errorf("couldn't keep a copy: %w", err)
		}
	}
	lines := strings.SplitAfter(string(b), "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		if slices.Contains(installerMarkers, strings.TrimSpace(lines[i])) && i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if next == `export PATH="`+dir+`:$PATH"` || next == "fish_add_path "+dir {
				if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) == "" {
					out = out[:n-1]
				}
				i++
				continue
			}
		}
		out = append(out, lines[i])
	}
	tmp := rc + ".mirrin-tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "")), st.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Rename(tmp, rc); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// installerAddedPath is the marker line above which install.sh added dir to
// PATH in rc, or "" when it didn't: its marker line, then the line that
// names dir.
func installerAddedPath(rc, dir string) string {
	f, err := os.Open(rc)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	marker := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if marker != "" && (line == `export PATH="`+dir+`:$PATH"` || line == "fish_add_path "+dir) {
			return marker
		}
		marker = ""
		if slices.Contains(installerMarkers, line) {
			marker = line
		}
	}
	return ""
}

func (u *uninstaller) run() error {
	items, notes, shared := u.plan()
	svc, legacy := u.svc.Installed(), u.svc.LegacyInstalled()
	haveTwin := isDir(u.home) && !emptyDir(u.home)
	keepTwin := "" // why the twin's folder can't be deleted from here
	if haveTwin {
		keepTwin = u.twinFolderProblem()
	}
	keepNote := func() {
		u.say("Your twin is set to be in %s, which %s, so Mirrin won't delete that folder. If you want your twin's files gone, delete them yourself.", u.home, keepTwin)
	}
	if !svc && !legacy && len(items) == 0 {
		for _, n := range notes {
			u.say("%s", n)
		}
		switch {
		case haveTwin && keepTwin != "":
			keepNote()
		case haveTwin:
			u.say("Your twin is in %s; delete that folder to remove it.", u.home)
		}
		return nil
	}

	u.say("This removes Mirrin from this computer:")
	if svc {
		u.say("  - the background service")
	}
	if legacy {
		u.say("  - the old background service from before the rename")
	}
	for _, it := range items {
		u.say("  - %s: %s", it.label, it.path)
	}
	for _, n := range notes {
		u.say("%s", n)
	}
	switch {
	case haveTwin && keepTwin != "":
		keepNote()
	case haveTwin:
		size := ""
		if n := dirSize(u.home); n > 0 {
			size = " (" + humanSize(n) + ")"
		}
		u.say("Your twin (its memory, personas, protocols, settings and saved keys) is in %s%s. It stays unless you choose to delete it.", u.home, size)
	}

	if u.ask == nil && !u.yes {
		return fmt.Errorf("mirrin uninstall asks before it removes anything. Run it in a terminal, or add --yes to remove the program and the service and keep your twin in %s", u.home)
	}
	if !u.yes && !confirm(u.ask, "Remove Mirrin?", false) {
		u.say("Nothing was changed.")
		return nil
	}

	deleteTwin, copyPath := false, ""
	if haveTwin && !u.yes && keepTwin == "" {
		undone := "This can't be undone."
		if u.backups != "" {
			undone = "Your encrypted backups aren't touched."
		}
		answer := u.ask.line(fmt.Sprintf("Delete your twin too? %s Type delete to delete %s, or press Enter to keep it: ", undone, u.home))
		if strings.EqualFold(strings.TrimSpace(answer), "delete") {
			deleteTwin = true
			if u.backups != "" {
				u.say("Your encrypted backups in %s stay where they are. They hold your keys and sign-ins too, and `mirrin restore` with your 12 words brings your twin back from them. To get rid of them as well, delete that folder yourself.", u.backups)
			}
			u.say("A copy has your memory, conversations, personas, protocols and settings. API keys and sign-ins (WhatsApp, Google, the browser) aren't in it, so you'd add them again after `mirrin identity import`.")
			if confirm(u.ask, "Save a copy first, so `mirrin identity import` can bring your twin back?", true) {
				copyPath = u.copyPath()
				if err := u.export(copyPath); err != nil {
					return fmt.Errorf("couldn't save a copy of your twin (%v), so nothing was removed", err)
				}
				u.say("Saved a copy of your twin in %s.", copyPath)
			}
		} else {
			u.say("Keeping your twin.")
		}
	}

	if svc {
		if err := u.svc.Remove(); err != nil {
			hint := ""
			if u.goos == "windows" {
				hint = " Open a terminal as administrator and run `mirrin uninstall` again."
			}
			return fmt.Errorf("couldn't remove the background service (%v), so nothing else was removed.%s", err, hint)
		}
		u.say("Removed the background service.")
	}
	if legacy {
		if err := u.svc.RemoveLegacy(u.out); err != nil {
			u.say("The old background service from before the rename is still installed (%v).", err)
		}
	}
	if u.stillRunning() {
		done := ""
		if svc {
			done = "The background service is removed, but "
		}
		return fmt.Errorf("%sMirrin is still running (in the menu bar or a terminal). Quit it, then run `mirrin uninstall` again", done)
	}

	var whatsappLinked bool
	if deleteTwin {
		whatsappLinked = fileExists(filepath.Join(u.dataDir, "whatsapp.db"))
		if err := os.RemoveAll(u.home); err != nil {
			return fmt.Errorf("couldn't delete %s: %w", u.home, err)
		}
		u.say("Deleted your twin (%s).", u.home)
	}

	var inno []removal
	var binDirs []string // folders a program was removed from
	binDir := func(it removal) {
		if !it.onPath {
			return
		}
		d := filepath.Dir(it.path)
		if it.inno {
			d = it.path
		}
		for _, b := range binDirs {
			if samePath(b, d) {
				return
			}
		}
		binDirs = append(binDirs, d)
	}
	failed := false
	for _, it := range items {
		if it.inno {
			inno = append(inno, it)
			binDir(it)
			continue
		}
		if u.goos == "windows" && samePath(it.path, u.exe) {
			// This very program: Windows deletes it once it has exited.
			if err := u.removeRunning(it.path); err != nil {
				failed = true
				u.say("Couldn't arrange to remove %s (%s): %v. Delete it yourself once this window closes.", it.label, it.path, err)
				continue
			}
			u.say("Removing %s (%s) as soon as this command ends.", it.label, it.path)
			binDir(it)
			continue
		}
		if err := os.RemoveAll(it.path); err != nil {
			failed = true
			u.say("Couldn't remove %s (%s): %v. Delete it yourself; it may need an administrator.", it.label, it.path, err)
			continue
		}
		u.say("Removed %s (%s).", it.label, it.path)
		binDir(it)
	}

	u.say("")
	switch {
	case failed:
		u.say("Mirrin is mostly uninstalled; see above for what's left.")
	case shared:
		u.say("This copy of Mirrin is uninstalled; the other twin's service and its copies stay, as said above.")
	default:
		u.say("Mirrin is uninstalled.")
	}
	switch {
	case haveTwin && !deleteTwin && keepTwin == "":
		u.say("Your twin is still in %s, so installing Mirrin again brings it back as it was. To delete it, delete that folder.", u.home)
	case deleteTwin:
		if !within(u.dataDir, u.home) && isDir(u.dataDir) {
			u.say("Your config kept its memory in %s, outside %s, so that folder is still there; delete it too if you want it gone.", u.dataDir, u.home)
		}
		if whatsappLinked {
			u.say("Your phone still lists this computer under WhatsApp → Linked devices; remove it there.")
		}
		u.say("Google and the chat apps you connected may still list Mirrin among their connected apps; remove it there too if you like.")
	}
	if copyPath != "" {
		u.say("Your twin's copy is in %s.", copyPath)
	}
	u.tidyPath(binDirs)
	for _, it := range inno {
		if err := u.startDetached(filepath.Join(it.path, "unins000.exe"), "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART"); err != nil {
			u.say("Couldn't start Mirrin's own uninstaller (%v). Remove Mirrin from Settings → Apps.", err)
			continue
		}
		u.say("Mirrin's own uninstaller is removing %s; it finishes in a moment.", it.path)
	}
	return nil
}

// confirm asks a yes/no question; def is the answer to a bare Enter.
func confirm(p *prompter, question string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	switch strings.ToLower(strings.TrimSpace(p.line(question + " " + hint + " "))) {
	case "":
		return def
	case "y", "yes":
		return true
	}
	return false
}

// stillRunning waits a little for a twin the service stopped to exit, and
// reports whether one is still running from this home.
func (u *uninstaller) stillRunning() bool {
	deadline := time.Now().Add(u.wait)
	for u.running() {
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// copyPath is a new file in the owner's home folder for the twin's copy.
func (u *uninstaller) copyPath() string {
	base := filepath.Join(u.userHome, "Mirrin-twin-"+u.today().Format("2006-01-02"))
	p := base + ".tar.gz"
	for i := 2; fileExists(p); i++ {
		p = base + "-" + strconv.Itoa(i) + ".tar.gz"
	}
	return p
}

// sameFile reports whether two paths are the same program, through links.
func sameFile(a, b string) bool {
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(sa, sb)
	}
	return samePath(a, b)
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func emptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 10<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// removeAfterExit deletes a Windows program once this process (which is
// running it) has exited, and its folder if that leaves it empty.
func removeAfterExit(path string) error {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := fmt.Sprintf("Wait-Process -Id %d -ErrorAction SilentlyContinue; Remove-Item -LiteralPath %s -Force; Remove-Item -LiteralPath %s -ErrorAction SilentlyContinue",
		os.Getpid(), q(path), q(filepath.Dir(path)))
	return startDetached("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
}

// startDetached starts a program that carries on after mirrin exits.
func startDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// localServices is this machine's service manager.
type localServices struct{}

func (localServices) Installed() bool {
	installed, _ := service.State()
	return installed
}

func (localServices) Remove() error {
	// Control only reads the config when installing.
	return service.Control(config.Default(), "uninstall", nil, io.Discard)
}

// Foreign is another twin's background service: Mirrin's, or one from
// before the rename.
func (localServices) Foreign() (unit, program string, ok bool) {
	if unit, program, ok = service.ForeignUnit(); ok {
		return unit, program, ok
	}
	return service.ForeignLegacyUnit()
}

func (localServices) LegacyInstalled() bool {
	if _, _, foreign := service.ForeignLegacyUnit(); foreign {
		return false // another twin's; Foreign reports it
	}
	return service.LegacyInstalled()
}

// RemoveLegacy also keeps any keys the old services' definitions held.
func (localServices) RemoveLegacy(out io.Writer) error { return service.RemoveLegacy(out) }
