package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeServices struct {
	installed, legacy      bool
	removeErr, legacyErr   error
	removed, legacyRemoved bool
	calls                  []string
	// foreignUnit is another twin's service definition, which runs foreignProgram.
	foreignUnit, foreignProgram string
}

func (f *fakeServices) Installed() bool { return f.installed }
func (f *fakeServices) Foreign() (string, string, bool) {
	return f.foreignUnit, f.foreignProgram, f.foreignUnit != ""
}
func (f *fakeServices) LegacyInstalled() bool { return f.legacy }
func (f *fakeServices) Remove() error {
	f.calls = append(f.calls, "remove")
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = true
	return nil
}
func (f *fakeServices) RemoveLegacy(out io.Writer) error {
	f.calls = append(f.calls, "remove legacy")
	if f.legacyErr != nil {
		return f.legacyErr
	}
	f.legacyRemoved = true
	// A machine can have both: AntBot's service and openHuman's before it.
	fmt.Fprintln(out, "Removed the old AntBot background service.")
	fmt.Fprintln(out, "Removed the old openHuman background service.")
	return nil
}

type uninstallTest struct {
	u        *uninstaller
	out      *bytes.Buffer
	svc      *fakeServices
	exported []string
	running  bool
	later    []string // programs left to delete after exit
	detached []string
}

// newUninstallTest has Mirrin installed in temp folders: the program, an
// app folder with Mirrin.app, and a twin with a memory file.
func newUninstallTest(t *testing.T, answers string) *uninstallTest {
	t.Helper()
	root := t.TempDir()
	ut := &uninstallTest{out: &bytes.Buffer{}, svc: &fakeServices{installed: true}}
	home := filepath.Join(root, "home", ".mirrin")
	writeTestFile(t, filepath.Join(home, "config.yaml"), []byte("name: Mirrin\n"))
	writeTestFile(t, filepath.Join(home, "data", "memory.db"), bytes.Repeat([]byte("m"), 3<<20))
	exe := filepath.Join(root, "bin", "mirrin")
	writeTestFile(t, exe, program(oldTag))
	apps := filepath.Join(root, "Applications")
	writeApp(t, filepath.Join(apps, "Mirrin.app"), "com.mirrin.mavrk")
	ut.u = &uninstaller{
		out: ut.out, home: home, dataDir: filepath.Join(home, "data"), userHome: filepath.Join(root, "home"),
		goos: "darwin", exe: exe, appDirs: []string{apps}, svc: ut.svc, command: (&fakeCommands{}).run,
		running: func() bool { return ut.running },
		export: func(file string) error {
			ut.exported = append(ut.exported, file)
			return os.WriteFile(file, []byte("twin"), 0o600)
		},
		removeRunning: func(p string) error { ut.later = append(ut.later, p); return nil },
		startDetached: func(name string, args ...string) error {
			ut.detached = append(ut.detached, name+" "+strings.Join(args, " "))
			return nil
		},
		today: func() time.Time { return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC) },
	}
	if answers != "-" {
		ut.u.ask = newPrompter(strings.NewReader(answers), ut.out)
	}
	return ut
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// writeApp lays out a macOS app with the bundle identifier id, as
// packaging/macos/Info.plist writes it: Mirrin.app holds mirrin, and
// AntBot.app antbot.
func writeApp(t *testing.T, app, id string) {
	t.Helper()
	name := strings.TrimSuffix(filepath.Base(app), ".app")
	writeTestFile(t, filepath.Join(app, "Contents", "MacOS", strings.ToLower(name)), program(oldTag))
	writeTestFile(t, filepath.Join(app, "Contents", "Info.plist"), []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>`+name+`</string>
	<key>CFBundleIdentifier</key><string>`+id+`</string>
	<key>LSUIElement</key><true/>
</dict>
</plist>
`))
}

func (ut *uninstallTest) intact(t *testing.T) {
	t.Helper()
	for _, p := range []string{ut.u.exe, filepath.Join(ut.u.appDirs[0], "Mirrin.app"), filepath.Join(ut.u.dataDir, "memory.db")} {
		if !exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
}

func TestUninstallAsksFirst(t *testing.T) {
	ut := newUninstallTest(t, "\n") // Enter: the default is no
	if err := ut.u.run(); err != nil {
		t.Fatal(err)
	}
	out := ut.out.String()
	for _, want := range []string{"This removes Mirrin from this computer:", "  - the background service", "  - the program: " + ut.u.exe, "  - the app: ", "(3.0 MB)", "Remove Mirrin? [y/N]", "Nothing was changed."} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	ut.intact(t)
	if len(ut.svc.calls) > 0 {
		t.Fatalf("touched the service: %v", ut.svc.calls)
	}

	// Without a terminal it needs --yes, and says what that keeps.
	ut = newUninstallTest(t, "-")
	err := ut.u.run()
	if err == nil || !strings.Contains(err.Error(), "add --yes") || !strings.Contains(err.Error(), "keep your twin in "+ut.u.home) {
		t.Fatalf("err = %v", err)
	}
	ut.intact(t)
}

func TestUninstallYesKeepsTheTwin(t *testing.T) {
	ut := newUninstallTest(t, "-")
	ut.u.yes = true
	ut.svc.legacy = true
	if err := ut.u.run(); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if !ut.svc.removed || !ut.svc.legacyRemoved {
		t.Fatalf("services: %+v", ut.svc)
	}
	if exists(ut.u.exe) || exists(filepath.Join(ut.u.appDirs[0], "Mirrin.app")) {
		t.Fatal("the program or the app is still there")
	}
	if !exists(filepath.Join(ut.u.dataDir, "memory.db")) {
		t.Fatal("--yes deleted the twin")
	}
	out := ut.out.String()
	for _, want := range []string{"Removed the background service.", "Removed the old AntBot background service.", "Removed the old openHuman background service.", "Mirrin is uninstalled.", "Your twin is still in " + ut.u.home + ", so installing Mirrin again brings it back"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Delete your twin") {
		t.Error("--yes asked a question")
	}
}

func TestUninstallDeletesTheTwinOnlyWhenTyped(t *testing.T) {
	for _, answer := range []string{"\n", "yes\n", "y\n", "delete it\n"} {
		ut := newUninstallTest(t, "y\n"+answer)
		if err := ut.u.run(); err != nil {
			t.Fatal(err)
		}
		if !exists(filepath.Join(ut.u.dataDir, "memory.db")) {
			t.Fatalf("answering %q deleted the twin", answer)
		}
		if exists(ut.u.exe) || !strings.Contains(ut.out.String(), "Keeping your twin.") {
			t.Fatalf("answering %q:\n%s", answer, ut.out)
		}
	}

	ut := newUninstallTest(t, "y\n DELETE \n\n") // delete, and save a copy (the default)
	writeTestFile(t, filepath.Join(ut.u.dataDir, "whatsapp.db"), []byte("device"))
	writeTestFile(t, filepath.Join(ut.u.userHome, "Mirrin-twin-2026-09-27.tar.gz"), []byte("an older copy"))
	if err := ut.u.run(); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if exists(ut.u.home) {
		t.Fatal("the twin is still there")
	}
	want := filepath.Join(ut.u.userHome, "Mirrin-twin-2026-09-27-2.tar.gz")
	if len(ut.exported) != 1 || ut.exported[0] != want {
		t.Fatalf("exported %v, want %s (next to, not over, the older copy)", ut.exported, want)
	}
	out := ut.out.String()
	for _, w := range []string{"Type delete to delete", "API keys and sign-ins (WhatsApp, Google, the browser) aren't in it", "Save a copy first", "Saved a copy of your twin in " + want, "Deleted your twin", "WhatsApp → Linked devices", "Your twin's copy is in " + want} {
		if !strings.Contains(out, w) {
			t.Errorf("want %q in:\n%s", w, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(ut.u.userHome, "Mirrin-twin-2026-09-27.tar.gz")); string(b) != "an older copy" {
		t.Fatal("overwrote an older copy")
	}

	// Declining the copy still deletes, and says nothing about one.
	ut = newUninstallTest(t, "y\ndelete\nn\n")
	if err := ut.u.run(); err != nil {
		t.Fatal(err)
	}
	if exists(ut.u.home) || len(ut.exported) != 0 || strings.Contains(ut.out.String(), "copy is in") {
		t.Fatalf("declined copy:\n%s", ut.out)
	}
}

// When anything goes wrong before the twin is deleted, the twin stays.
func TestUninstallStopsBeforeLosingAnything(t *testing.T) {
	ut := newUninstallTest(t, "y\ndelete\ny\n")
	ut.u.export = func(string) error { return errors.New("disk full") }
	err := ut.u.run()
	if err == nil || !strings.Contains(err.Error(), "couldn't save a copy of your twin (disk full), so nothing was removed") {
		t.Fatalf("err = %v", err)
	}
	ut.intact(t)
	if len(ut.svc.calls) > 0 {
		t.Fatalf("touched the service: %v", ut.svc.calls)
	}

	ut = newUninstallTest(t, "y\ndelete\nn\n")
	ut.svc.removeErr = errors.New("access denied")
	ut.u.goos = "windows"
	err = ut.u.run()
	if err == nil || !strings.Contains(err.Error(), "couldn't remove the background service (access denied), so nothing else was removed") || !strings.Contains(err.Error(), "as administrator") {
		t.Fatalf("err = %v", err)
	}
	ut.intact(t)

	// A twin still running (the menu bar app, say) would write the twin
	// back, so nothing is deleted under it.
	ut = newUninstallTest(t, "y\ndelete\nn\n")
	ut.running = true
	err = ut.u.run()
	if err == nil || !strings.Contains(err.Error(), "The background service is removed, but Mirrin is still running") {
		t.Fatalf("err = %v", err)
	}
	ut.intact(t)
}

func TestUninstallFindsEveryCopy(t *testing.T) {
	ut := newUninstallTest(t, "-")
	ut.u.yes = true
	root := filepath.Dir(filepath.Dir(ut.u.exe))
	oldBin := filepath.Join(root, "old", "openhuman")
	writeTestFile(t, oldBin, []byte("openhuman v0.2.0\n"))
	antBin := filepath.Join(root, "gopath", "bin", "antbot")
	writeTestFile(t, antBin, []byte("antbot v0.3.0-12-gabc1234\n"))
	otherCLI := filepath.Join(root, "gopath", "bin", "mirrin")
	writeTestFile(t, otherCLI, program("dev"))
	oldApp := filepath.Join(ut.u.appDirs[0], "OpenHuman.app")
	writeApp(t, oldApp, "com.openhuman.mavrk")
	antApp := filepath.Join(ut.u.appDirs[0], "AntBot.app")
	writeApp(t, antApp, "com.antbot.mavrk")
	leftover := filepath.Join(filepath.Dir(ut.u.exe), ".mirrin.new")
	writeTestFile(t, leftover, []byte("half an update"))
	movedAside := filepath.Join(filepath.Dir(ut.u.exe), "mirrin.old-1727430000.exe")
	writeTestFile(t, movedAside, program("v9.9.7"))
	// What an AntBot update left beside the program, under its names.
	antLeftover := filepath.Join(filepath.Dir(ut.u.exe), ".antbot.new")
	writeTestFile(t, antLeftover, []byte("half an update"))
	antAside := filepath.Join(filepath.Dir(ut.u.exe), "antbot.old.exe")
	writeTestFile(t, antAside, []byte("antbot v0.3.0\n"))
	brewed := filepath.Join(root, "homebrew", "Cellar", "mirrin", "9.9.8", "bin", "mirrin")
	writeTestFile(t, brewed, program(oldTag))
	ut.u.others = []string{ut.u.exe, otherCLI, oldBin, antBin, brewed, filepath.Join(root, "gone", "mirrin")}
	if err := ut.u.run(); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	for _, p := range []string{ut.u.exe, otherCLI, oldBin, antBin, oldApp, antApp, leftover, movedAside, antLeftover, antAside} {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	out := ut.out.String()
	for _, want := range []string{"the old openHuman program: " + oldBin, "the old AntBot program: " + antBin, "the old openHuman app: " + oldApp, "the old AntBot app: " + antApp, "a leftover from an update", "Homebrew installed " + brewed + ". Remove it with: brew uninstall mirrin"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	if !exists(brewed) {
		t.Error("removed Homebrew's copy")
	}
	if n := strings.Count(out, "  - the program: "+ut.u.exe); n != 1 {
		t.Errorf("listed the running program %d times:\n%s", n, out)
	}
	// The plan reads the same every time: newest name first.
	a, ant, o := strings.Index(out, "  - the app: "), strings.Index(out, "  - the old AntBot app: "), strings.Index(out, "  - the old openHuman app: ")
	if a < 0 || ant < a || o < ant {
		t.Errorf("the apps are listed out of order:\n%s", out)
	}
}

// OpenHuman and AntBot are also other products' names, and any program can
// be called mirrin: only programs that answer as Mirrin does (or as AntBot
// and openHuman did), and apps with Mirrin's bundle identifiers, are
// removed, whether or not it asks first.
func TestUninstallLeavesOtherProductsAlone(t *testing.T) {
	for _, yes := range []bool{true, false} {
		answers := "y\n\n" // remove Mirrin, keep the twin
		if yes {
			answers = "-"
		}
		ut := newUninstallTest(t, answers)
		ut.u.yes = yes
		root := filepath.Dir(filepath.Dir(ut.u.exe))
		theirApp := filepath.Join(ut.u.appDirs[0], "OpenHuman.app")
		writeApp(t, theirApp, "com.example.openhuman")
		userApps := filepath.Join(ut.u.userHome, "Applications")
		unlabelled := filepath.Join(userApps, "Mirrin.app")
		writeTestFile(t, filepath.Join(unlabelled, "Contents", "MacOS", "Mirrin"), []byte("someone's"))
		ut.u.appDirs = append(ut.u.appDirs, userApps)
		antApp := filepath.Join(userApps, "AntBot.app")
		writeApp(t, antApp, "dev.antbot.desktop")
		theirs := filepath.Join(root, "other", "openhuman")
		writeTestFile(t, theirs, []byte("OpenHuman desktop 1.4.2\n"))
		sameName := filepath.Join(root, "tools", "mirrin")
		writeTestFile(t, sameName, []byte("mirrin 2.0 (a mirror tool)\n"))
		antName := filepath.Join(root, "agents", "antbot")
		writeTestFile(t, antName, []byte("antbot 2.0 (ant colony simulator)\n"))
		silent := filepath.Join(root, "silent", "mirrin")
		writeTestFile(t, silent, nil)
		ut.u.others = []string{theirs, sameName, antName, silent}
		if err := ut.u.run(); err != nil {
			t.Fatalf("yes=%v: %v\n%s", yes, err, ut.out)
		}
		for _, p := range []string{theirApp, unlabelled, antApp, theirs, sameName, antName, silent} {
			if !exists(p) {
				t.Errorf("yes=%v: removed %s, which isn't Mirrin's", yes, p)
			}
			if strings.Contains(ut.out.String(), p+"\n") {
				t.Errorf("yes=%v: listed %s:\n%s", yes, p, ut.out)
			}
		}
		if exists(ut.u.exe) || exists(filepath.Join(ut.u.appDirs[0], "Mirrin.app")) {
			t.Errorf("yes=%v: Mirrin itself is still there:\n%s", yes, ut.out)
		}
	}
}

// A service installed for another twin (the owner's, seen from a command
// run with MIRRIN_HOME set) keeps its copy of Mirrin running, so only this
// program goes, and only when it isn't that copy.
func TestUninstallLeavesAnotherTwinsService(t *testing.T) {
	ut := newUninstallTest(t, "-")
	ut.u.yes = true
	ut.svc.installed = false // State doesn't count another home's service
	root := filepath.Dir(filepath.Dir(ut.u.exe))
	theirs := filepath.Join(root, "local", "bin", "mirrin")
	writeTestFile(t, theirs, program(oldTag))
	ut.u.others = []string{theirs}
	ut.svc.foreignUnit, ut.svc.foreignProgram = "/Users/me/Library/LaunchAgents/mirrin.plist", theirs
	if err := ut.u.run(); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if exists(ut.u.exe) {
		t.Error("kept this program, which no service runs")
	}
	for _, p := range []string{theirs, filepath.Join(ut.u.appDirs[0], "Mirrin.app")} {
		if !exists(p) {
			t.Errorf("removed %s, which another twin's service may run", p)
		}
	}
	if !strings.Contains(ut.out.String(), "Another Mirrin twin's background service (/Users/me/Library/LaunchAgents/mirrin.plist) still runs "+theirs) ||
		!strings.Contains(ut.out.String(), "run `mirrin uninstall` from that twin (without MIRRIN_HOME)") {
		t.Fatalf("output:\n%s", ut.out)
	}
	if len(ut.svc.calls) > 0 {
		t.Fatalf("touched a service: %v", ut.svc.calls)
	}

	// When that service runs this very program, nothing is removed.
	ut = newUninstallTest(t, "-")
	ut.u.yes, ut.svc.installed = true, false
	ut.svc.foreignUnit, ut.svc.foreignProgram = "/home/me/.config/systemd/user/mirrin.service", ut.u.exe
	if err := ut.u.run(); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	ut.intact(t)
	if out := ut.out.String(); !strings.Contains(out, "still runs "+ut.u.exe) || strings.Contains(out, "Removed") {
		t.Fatalf("output:\n%s", out)
	}
}

// MIRRIN_HOME set to the home folder, or to a folder that isn't a twin's,
// is never offered for deletion.
func TestUninstallWontDeleteAFolderThatIsntATwin(t *testing.T) {
	ut := newUninstallTest(t, "y\ndelete\nn\n")
	ut.u.home = ut.u.userHome
	if err := ut.u.run(); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	out := ut.out.String()
	if !exists(ut.u.userHome) || !exists(filepath.Join(ut.u.dataDir, "memory.db")) {
		t.Fatal("deleted the home folder")
	}
	if !strings.Contains(out, "which holds your home folder, so Mirrin won't delete that folder. If you want your twin's files gone, delete them yourself.") ||
		strings.Contains(out, "Type delete") || strings.Contains(out, "To delete it, delete that folder") {
		t.Fatalf("output:\n%s", out)
	}

	notATwin := t.TempDir()
	writeTestFile(t, filepath.Join(notATwin, "thesis.pdf"), []byte("years of work"))
	ut = newUninstallTest(t, "y\ndelete\nn\n")
	ut.u.home = notATwin
	if err := ut.u.run(); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(notATwin, "thesis.pdf")) || !strings.Contains(ut.out.String(), "doesn't look like a Mirrin twin") {
		t.Fatalf("output:\n%s", ut.out)
	}

	u := &uninstaller{userHome: "/Users/me"}
	for home, want := range map[string]string{
		"/":                    "is a whole disk",
		".mirrin":              "isn't a full path",
		"/Users":               "holds your home folder",
		"/Users/me":            "holds your home folder",
		"/Users/me/":           "holds your home folder",
		"/Users/me/.mirrin/..": "holds your home folder",
	} {
		u.home = home
		if got := u.twinFolderProblem(); got != want {
			t.Errorf("home %q: %q, want %q", home, got, want)
		}
	}
	u.home = filepath.Dir(ut.u.dataDir)
	if got := u.twinFolderProblem(); got != "" {
		t.Errorf("a real twin: %q", got)
	}
}

// The installers' PATH changes stay, so uninstall says where they are.
func TestUninstallPointsOutWhatTheInstallerAddedToPath(t *testing.T) {
	ut := newUninstallTest(t, "-")
	ut.u.yes = true
	bin := filepath.Dir(ut.u.exe)
	zshrc := filepath.Join(ut.u.userHome, ".zshrc")
	writeTestFile(t, zshrc, []byte("alias ll='ls -l'\n\n# Added by the Mirrin installer\nexport PATH=\""+bin+":$PATH\"\n"))
	bashrc := filepath.Join(ut.u.userHome, ".bashrc")
	writeTestFile(t, bashrc, []byte("export PATH=\""+bin+":$PATH\"\n")) // the owner's own line
	if err := ut.u.run(); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	out := ut.out.String()
	if !strings.Contains(out, "The Mirrin installer put "+bin+" on your PATH in "+zshrc+".") || strings.Contains(out, bashrc) {
		t.Fatalf("output:\n%s", out)
	}

	ut = newUninstallTest(t, "-")
	ut.u.yes, ut.u.goos, ut.u.appDirs = true, "windows", nil
	bin = filepath.Dir(ut.u.exe)
	ut.u.pathEnv = `C:\Windows\system32;` + bin + `\;C:\Tools`
	if err := ut.u.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), bin+" is still on your PATH.") {
		t.Fatalf("output:\n%s", ut.out)
	}
}

// The AntBot installer's PATH lines, from before the rename, are found and
// removed like the Mirrin installer's.
func TestUninstallRemovesTheAntBotInstallersPathLine(t *testing.T) {
	ut := newUninstallTest(t, "y\n\ny\n")
	bin := filepath.Dir(ut.u.exe)
	zshrc := filepath.Join(ut.u.userHome, ".zshrc")
	orig := "alias ll='ls -l'\n\n# Added by the AntBot installer\nexport PATH=\"" + bin + ":$PATH\"\nexport EDITOR=vi\n"
	writeTestFile(t, zshrc, []byte(orig))
	if err := ut.u.run(); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if got, _ := os.ReadFile(zshrc); string(got) != "alias ll='ls -l'\nexport EDITOR=vi\n" {
		t.Fatalf(".zshrc is now %q\n%s", got, ut.out)
	}
	if !strings.Contains(ut.out.String(), "The AntBot installer put "+bin+" on your PATH in "+zshrc+". Remove that line?") {
		t.Fatalf("output:\n%s", ut.out)
	}

	// Not asked, it says which line to delete.
	ut = newUninstallTest(t, "-")
	ut.u.yes = true
	bin = filepath.Dir(ut.u.exe)
	fish := filepath.Join(ut.u.userHome, ".config", "fish", "config.fish")
	writeTestFile(t, fish, []byte("# Added by the AntBot installer\nfish_add_path "+bin+"\n"))
	if err := ut.u.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), "The AntBot installer put "+bin+" on your PATH in "+fish+". If nothing else you use is in that folder, delete the line \u201c# Added by the AntBot installer\u201d") {
		t.Fatalf("output:\n%s", ut.out)
	}
}

// Asked, uninstall removes the installer's PATH lines after keeping a copy
// of the file; not asked, or told no, it changes nothing there.
func TestUninstallRemovesTheInstallersPathLinesWhenAsked(t *testing.T) {
	rcText := func(bin string) string {
		return "alias ll='ls -l'\n\n# Added by the Mirrin installer\nexport PATH=\"" + bin + ":$PATH\"\nexport EDITOR=vi\n"
	}
	// Remove Mirrin, keep the twin, remove the PATH line.
	ut := newUninstallTest(t, "y\n\ny\n")
	bin := filepath.Dir(ut.u.exe)
	zshrc := filepath.Join(ut.u.userHome, ".zshrc")
	writeTestFile(t, zshrc, []byte(rcText(bin)))
	if err := ut.u.run(); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	got, _ := os.ReadFile(zshrc)
	if string(got) != "alias ll='ls -l'\nexport EDITOR=vi\n" {
		t.Fatalf(".zshrc is now %q\n%s", got, ut.out)
	}
	if b, err := os.ReadFile(zshrc + ".mirrin-backup"); err != nil || string(b) != rcText(bin) {
		t.Fatalf("backup %q, %v", b, err)
	}
	if !strings.Contains(ut.out.String(), "Removed it from "+zshrc) {
		t.Fatalf("output:\n%s", ut.out)
	}

	// Told no (a bare Enter), and with --yes, nothing changes.
	for _, answers := range []string{"y\n\n\n", "-"} {
		ut = newUninstallTest(t, answers)
		ut.u.yes = answers == "-"
		bin = filepath.Dir(ut.u.exe)
		zshrc = filepath.Join(ut.u.userHome, ".zshrc")
		writeTestFile(t, zshrc, []byte(rcText(bin)))
		if err := ut.u.run(); err != nil {
			t.Fatalf("%v\n%s", err, ut.out)
		}
		if got, _ := os.ReadFile(zshrc); string(got) != rcText(bin) {
			t.Fatalf("%q: .zshrc changed to %q", answers, got)
		}
		if exists(zshrc + ".mirrin-backup") {
			t.Fatalf("%q: wrote a backup without being asked", answers)
		}
		if !strings.Contains(ut.out.String(), "The Mirrin installer put "+bin+" on your PATH in "+zshrc) {
			t.Fatalf("%q: output:\n%s", answers, ut.out)
		}
	}
}

// Homebrew's copy is Homebrew's to remove; with nothing else installed,
// uninstall just says how.
func TestUninstallLeavesHomebrewToHomebrew(t *testing.T) {
	ut := newUninstallTest(t, "-")
	ut.svc.installed = false
	ut.u.appDirs = nil
	ut.u.exe = "/opt/homebrew/Cellar/mirrin/9.9.8/bin/mirrin"
	if err := ut.u.run(); err != nil {
		t.Fatal(err)
	}
	out := ut.out.String()
	if !strings.Contains(out, "brew uninstall mirrin") || !strings.Contains(out, "Your twin is in "+ut.u.home) || strings.Contains(out, "Remove Mirrin?") {
		t.Fatalf("output:\n%s", out)
	}
}

// Windows can't delete a running program, so the running mirrin.exe is
// deleted once mirrin exits, and a folder the setup program made is removed
// by its own uninstaller.
func TestUninstallOnWindows(t *testing.T) {
	ut := newUninstallTest(t, "-")
	ut.u.yes, ut.u.goos, ut.u.appDirs = true, "windows", nil
	if err := ut.u.run(); err != nil {
		t.Fatal(err)
	}
	if len(ut.later) != 1 || ut.later[0] != ut.u.exe || !exists(ut.u.exe) {
		t.Fatalf("left for later: %v", ut.later)
	}

	ut = newUninstallTest(t, "-")
	ut.u.yes, ut.u.goos, ut.u.appDirs = true, "windows", nil
	dir := filepath.Dir(ut.u.exe)
	writeTestFile(t, filepath.Join(dir, "unins000.exe"), []byte("inno"))
	if err := ut.u.run(); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "unins000.exe") + " /VERYSILENT /SUPPRESSMSGBOXES /NORESTART"
	if len(ut.detached) != 1 || ut.detached[0] != want || len(ut.later) != 0 {
		t.Fatalf("detached %v, later %v", ut.detached, ut.later)
	}
	if !strings.Contains(ut.out.String(), "Mirrin's own uninstaller is removing") {
		t.Fatalf("output:\n%s", ut.out)
	}
}

func TestUninstallMentionsMemoryKeptElsewhere(t *testing.T) {
	ut := newUninstallTest(t, "y\ndelete\nn\n")
	elsewhere := filepath.Join(t.TempDir(), "memory")
	writeTestFile(t, filepath.Join(elsewhere, "memory.db"), []byte("m"))
	ut.u.dataDir = elsewhere
	if err := ut.u.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), "kept its memory in "+elsewhere) || !exists(filepath.Join(elsewhere, "memory.db")) {
		t.Fatalf("output:\n%s", ut.out)
	}
}

func TestParseUninstallArgs(t *testing.T) {
	if err := uninstallCmd([]string{"--force"}); err == nil || !strings.Contains(err.Error(), uninstallUsage) {
		t.Fatalf("err = %v", err)
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{12: "12 bytes", 3 << 10: "3 KB", 3 << 20: "3.0 MB", 42 << 20: "42 MB", 5 << 30: "5.0 GB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestBundleID(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "Mirrin.app")
	writeApp(t, app, "com.mirrin.mavrk")
	u := &uninstaller{command: func(context.Context, string, ...string) (string, error) {
		return "", errors.New("unexpected command")
	}}
	if got := bundleID(u.command, app); got != "com.mirrin.mavrk" {
		t.Errorf("bundleID = %q", got)
	}
	// Only the app's own identifier counts, not one nested deeper.
	nested := filepath.Join(dir, "Nested.app")
	writeTestFile(t, filepath.Join(nested, "Contents", "Info.plist"), []byte(`<plist><dict><key>Helper</key><dict><key>CFBundleIdentifier</key><string>com.mirrin.mavrk</string></dict><key>CFBundleIdentifier</key><string>com.example.other</string></dict></plist>`))
	if got := bundleID(u.command, nested); got != "com.example.other" {
		t.Errorf("nested: %q", got)
	}
	if got := bundleID(u.command, filepath.Join(dir, "Missing.app")); got != "" {
		t.Errorf("missing: %q", got)
	}
	// A binary plist is read with plutil.
	binary := filepath.Join(dir, "Binary.app")
	writeTestFile(t, filepath.Join(binary, "Contents", "Info.plist"), []byte("bplist00\x01\x02"))
	var asked []string
	u.command = func(_ context.Context, name string, args ...string) (string, error) {
		asked = append(asked, name+" "+strings.Join(args, " "))
		return "com.openhuman.mavrk\n", nil
	}
	if got := bundleID(u.command, binary); got != "com.openhuman.mavrk" || len(asked) != 1 || !strings.HasPrefix(asked[0], "plutil -extract CFBundleIdentifier raw") {
		t.Errorf("binary: %q, asked %v", got, asked)
	}
}

// Deleting the twin says what outlives it: the encrypted backups (backup
// merged with release), which hold the keys too and bring the twin back
// with `mirrin restore`, instead of "This can't be undone".
func TestUninstallSaysTheEncryptedBackupsStay(t *testing.T) {
	ut := newUninstallTest(t, "y\ndelete\nn\n")
	ut.u.backups = "/Volumes/NAS/mirrin-backups"
	if err := ut.u.run(); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	out := ut.out.String()
	for _, w := range []string{"Your encrypted backups aren't touched.", "/Volumes/NAS/mirrin-backups stay", "keys and sign-ins", "`mirrin restore`"} {
		if !strings.Contains(out, w) {
			t.Errorf("want %q in:\n%s", w, out)
		}
	}
	if strings.Contains(out, "This can't be undone") {
		t.Errorf("says it can't be undone while backups can bring it back:\n%s", out)
	}
	// Without backups, it says what it always said.
	ut = newUninstallTest(t, "y\ndelete\nn\n")
	if err := ut.u.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), "This can't be undone") || strings.Contains(ut.out.String(), "encrypted backups") {
		t.Fatalf("without backups:\n%s", ut.out)
	}
}

// A shell startup file that's a symlink into a dotfiles repo stays a
// symlink, and the file it points to loses the line. Two installer lines in
// one file are both removed, and the copy kept is the file as it was.
func TestUninstallPathLinesInASymlinkedRc(t *testing.T) {
	ut := newUninstallTest(t, "y\ny\n")
	a, b := filepath.Join(ut.u.userHome, "bin-a"), filepath.Join(ut.u.userHome, "bin-b")
	orig := "# Added by the Mirrin installer\nexport PATH=\"" + a + ":$PATH\"\nexport EDITOR=vi\n# Added by the Mirrin installer\nexport PATH=\"" + b + ":$PATH\"\n"
	real := filepath.Join(ut.u.userHome, "dotfiles", "zshrc")
	writeTestFile(t, real, []byte(orig))
	zshrc := filepath.Join(ut.u.userHome, ".zshrc")
	if err := os.Symlink(real, zshrc); err != nil {
		t.Skip("no symlinks here:", err)
	}
	ut.u.tidyPath([]string{a, b})
	if fi, err := os.Lstat(zshrc); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".zshrc is no longer a symlink: %v\n%s", err, ut.out)
	}
	if got, _ := os.ReadFile(real); string(got) != "export EDITOR=vi\n" {
		t.Fatalf("the dotfiles copy is now %q\n%s", got, ut.out)
	}
	if got, err := os.ReadFile(zshrc + ".mirrin-backup"); err != nil || string(got) != orig {
		t.Fatalf("backup %q, %v", got, err)
	}
}
