package service

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// legacyPlist is a LaunchAgent AntBot installed: it runs an older program,
// names its home the way AntBot did, and carries keys from before secrets.env.
func legacyPlist(program, home string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key><string>antbot</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + program + `</string>
		<string>tray</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>ANTBOT_HOME</key><string>` + home + `</string>
		<key>ANTBOT_SERVICE</key><string>1</string>
		<key>ANTBOT_EMAIL_PASSWORD</key><string>app pass</string>
		<key>OPENAI_API_KEY</key><string>sk-openai</string>
	</dict>
	<key>KeepAlive</key><true/>
</dict>
</plist>
`
}

// fakeRetirer is a retirer over unit files in a temporary folder.
type fakeRetirer struct {
	units  map[string]*fakeUnit
	opened []string
	checks int // looks at whether the old twin still holds its home
	busy   int // looks before it lets go
}

func newFakeRetirer(t *testing.T, dir string, out *strings.Builder) (retirer, *fakeRetirer) {
	t.Helper()
	f := &fakeRetirer{units: map[string]*fakeUnit{}}
	r := retirer{
		out: out, self: "/Applications/Mirrin.app/Contents/MacOS/mirrin", wait: 5 * time.Second,
		path: func(name string) string { return filepath.Join(dir, name+".plist") },
		open: func(name string) (unit, error) {
			f.opened = append(f.opened, name)
			u := f.units[name]
			if u == nil {
				return nil, errors.New("no such service")
			}
			return u, nil
		},
		inUse: func(string) bool { f.checks++; return f.checks <= f.busy },
	}
	return r, f
}

// AntBot's service runs an older program: its keys are saved into the home
// first (under the new names), then it is stopped and removed, never taken
// over, and Mirrin waits for its twin to let go of the home.
func TestRetireLegacyBootsOutAnOlderProgram(t *testing.T) {
	home := filepath.Join(t.TempDir(), "twin")
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("ANTBOT_HOME", "")
	t.Setenv("OPENHUMAN_HOME", "")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "antbot.plist"), legacyPlist("/Users/me/go/bin/antbot", home))
	// openHuman's, for another home, stays.
	writeFile(t, filepath.Join(dir, "openhuman.plist"), strings.Replace(legacyPlist("/usr/local/bin/openhuman", "/Users/someone-else/twin"), "ANTBOT_HOME", "OPENHUMAN_HOME", 1))

	var out strings.Builder
	r, f := newFakeRetirer(t, dir, &out)
	f.units["antbot"] = &fakeUnit{installed: true, running: true}
	f.busy = 2
	r.run()

	if got := strings.Join(f.units["antbot"].calls, ","); got != "stop,uninstall" {
		t.Fatalf("AntBot's service: %s (opened %v)\n%s", got, f.opened, out.String())
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened %v: another home's service was touched, or one was taken over", f.opened)
	}
	if f.checks != 3 {
		t.Fatalf("looked %d times for the old twin to quit, want until it let go (3)", f.checks)
	}
	got, err := config.ReadSecretsIn(home)
	if err != nil {
		t.Fatal(err)
	}
	if got["MIRRIN_EMAIL_PASSWORD"] != "app pass" || got["OPENAI_API_KEY"] != "sk-openai" {
		t.Fatalf("keys not kept: %v", got)
	}
	if _, ok := got["ANTBOT_SERVICE"]; ok {
		t.Fatal("the marker isn't a key")
	}
	if !strings.Contains(out.String(), "Stopped the old AntBot background service") || strings.Contains(out.String(), "app pass") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// A service running this very program, or this process, is left for
// `mirrin service install`; so is one whose keys can't be saved.
func TestRetireLegacyLeavesWhatItShould(t *testing.T) {
	home := filepath.Join(t.TempDir(), "twin")
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("ANTBOT_HOME", "")
	t.Setenv("OPENHUMAN_HOME", "")

	for _, c := range []struct {
		name  string
		setup func(r *retirer, dir string)
	}{
		{"it runs this program", func(r *retirer, dir string) {
			writeFile(t, filepath.Join(dir, "antbot.plist"), legacyPlist(r.self, home))
		}},
		{"it runs this process", func(r *retirer, dir string) {
			writeFile(t, filepath.Join(dir, "antbot.plist"), legacyPlist("/Users/me/go/bin/antbot", home))
			r.label = "antbot"
		}},
		{"another home's", func(r *retirer, dir string) {
			writeFile(t, filepath.Join(dir, "antbot.plist"), legacyPlist("/Users/me/go/bin/antbot", "/Users/someone-else/twin"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			var out strings.Builder
			r, f := newFakeRetirer(t, dir, &out)
			f.units["antbot"] = &fakeUnit{installed: true, running: true}
			c.setup(&r, dir)
			r.run()
			if len(f.opened) != 0 || len(f.units["antbot"].calls) != 0 || out.Len() != 0 {
				t.Fatalf("touched it: opened %v, calls %v\n%s", f.opened, f.units["antbot"].calls, out.String())
			}
		})
	}

	// Keys that can't be saved keep the service, which holds them.
	blocked := filepath.Join(t.TempDir(), "a-file")
	writeFile(t, blocked, "not a folder")
	home = filepath.Join(blocked, "twin")
	t.Setenv("MIRRIN_HOME", home)
	dir := t.TempDir()
	var out strings.Builder
	r, f := newFakeRetirer(t, dir, &out)
	f.units["antbot"] = &fakeUnit{installed: true, running: true}
	writeFile(t, filepath.Join(dir, "antbot.plist"), legacyPlist("/Users/me/go/bin/antbot", home))
	r.run()
	if len(f.units["antbot"].calls) != 0 || !strings.Contains(out.String(), "stays until its keys are saved") {
		t.Fatalf("calls %v\n%s", f.units["antbot"].calls, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "antbot.plist")); err != nil {
		t.Fatal("the definition holding the keys went")
	}
}

// When the old service can't be removed, it says how to do it by hand.
func TestRetireLegacySaysHowWhenItCant(t *testing.T) {
	home := filepath.Join(t.TempDir(), "twin")
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("ANTBOT_HOME", "")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "antbot.plist"), legacyPlist("/Users/me/go/bin/antbot", home))
	var out strings.Builder
	r, f := newFakeRetirer(t, dir, &out)
	r.run() // f has no unit: opening it fails
	if len(f.opened) != 1 || !strings.Contains(out.String(), "still installed") || !strings.Contains(out.String(), "antbot") {
		t.Fatalf("opened %v\n%s", f.opened, out.String())
	}
	// Stopping it isn't enough: its definition would start it again at the
	// next login, so the advice deletes that too. (Only Linux and macOS have
	// these services; on Windows the home's path isn't one they'd write.)
	if runtime.GOOS == "windows" {
		return
	}
	if got := removeCommand("linux", "antbot"); !strings.HasPrefix(got, "systemctl --user disable --now antbot.service; rm '") || !strings.HasSuffix(got, "/.config/systemd/user/antbot.service'") {
		t.Fatal(got)
	}
	if got := removeCommand("darwin", "antbot"); !strings.HasPrefix(got, "launchctl bootout gui/") || !strings.Contains(got, "/antbot; rm '") || !strings.HasSuffix(got, "/Library/LaunchAgents/antbot.plist'") {
		t.Fatal(got)
	}
}

// While this home's AntBot service would start an older program at the next
// login, the home must stay where that program expects it (LegacyHold):
// moved, it would start an empty twin in its place. A service with nothing
// left to start, one running this program, this process or another home's
// holds nothing.
func TestLegacyHoldWhileTheOldServiceCanStart(t *testing.T) {
	home := filepath.Join(t.TempDir(), "twin")
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("ANTBOT_HOME", "")
	t.Setenv("OPENHUMAN_HOME", "")
	older := filepath.Join(t.TempDir(), "antbot")
	writeFile(t, older, "#!/bin/sh\n")

	for _, c := range []struct {
		name, program, home, label string
		want                       bool
	}{
		{"an older program", older, home, "", true},
		{"its program is gone", filepath.Join(t.TempDir(), "antbot"), home, "", false},
		{"this program", "", home, "", false},
		{"this process", older, home, "antbot", false},
		{"another home's", older, "/Users/someone-else/twin", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			r, _ := newFakeRetirer(t, dir, &strings.Builder{})
			if c.program == "" {
				c.program = r.self
			}
			r.label = c.label
			writeFile(t, filepath.Join(dir, "antbot.plist"), legacyPlist(c.program, c.home))
			got := r.hold()
			if (got != "") != c.want || c.want && !strings.Contains(got, "AntBot") {
				t.Fatalf("hold() = %q, want a hold: %v", got, c.want)
			}
		})
	}
	r, _ := newFakeRetirer(t, t.TempDir(), &strings.Builder{})
	if got := r.hold(); got != "" {
		t.Fatalf("no old service: %q", got)
	}
}

// On Windows the old service is AntBot's entry in the Startup folder.
func TestLegacyEntryHold(t *testing.T) {
	home := filepath.Join(t.TempDir(), "twin")
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("ANTBOT_HOME", "")
	t.Setenv("OPENHUMAN_HOME", "")
	older := filepath.Join(t.TempDir(), "antbot.exe")
	writeFile(t, older, "")
	self := filepath.Join(t.TempDir(), "mirrin.exe")
	script := func(exe, h string) string {
		return "@echo off\r\nset \"ANTBOT_HOME=" + h + "\"\r\nset \"ANTBOT_SERVICE=1\"\r\nstart \"\" /min \"" + exe + "\" tray\r\n"
	}
	for _, c := range []struct {
		name, body string
		want       bool
	}{
		{"an older program", script(older, home), true},
		{"its program is gone", script(filepath.Join(t.TempDir(), "antbot.exe"), home), false},
		{"this program", script(self, home), false},
		{"another home's", script(older, filepath.Join(t.TempDir(), "other")), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "AntBot.cmd"), c.body)
			if got := legacyEntryHold(dir, self); (got != "") != c.want {
				t.Fatalf("legacyEntryHold = %q, want a hold: %v", got, c.want)
			}
		})
	}
	if got := legacyEntryHold(t.TempDir(), self); got != "" {
		t.Fatalf("no entry: %q", got)
	}
	if got := startupProgram([]byte(startupScript(`C:\Program Files\Mirrin\mirrin.exe`, home))); got != `C:\Program Files\Mirrin\mirrin.exe` {
		t.Fatalf("startupProgram = %q", got)
	}
}

func TestSameProgram(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "mirrin")
	writeFile(t, exe, "")
	link := filepath.Join(dir, "antbot")
	if err := os.Symlink(exe, link); err != nil {
		t.Skip(err)
	}
	if !sameProgram(exe, link) || !sameProgram(exe, exe) {
		t.Fatal("a link to this program is this program")
	}
	if sameProgram(exe, filepath.Join(dir, "other")) || sameProgram("", exe) || sameProgram(exe, "") {
		t.Fatal("another program, or none, is this one")
	}
}

// The service's own log under an old name counts until this one has one.
func TestLastErrorReadsTheOldServiceLog(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "antbot.err.log"), "error: the old one failed\n")
	if got := lastError(dir); got != "the old one failed" {
		t.Fatalf("lastError = %q", got)
	}
	writeFile(t, filepath.Join(dir, "mirrin.err.log"), "time=1 level=INFO msg=\"Mirrin online\"\n")
	if got := lastError(dir); got != "" {
		t.Fatalf("lastError = %q, want this service's (none since it came up)", got)
	}
}

// AntBot's systemd unit set ANTBOT_DESKTOP_SERVICE, and a supervisor from
// before an update passes ANTBOT_LOGS_SUPERVISED: both still count.
func TestOldServiceVariablesStillCount(t *testing.T) {
	t.Setenv(desktopServiceEnv, "")
	t.Setenv("ANTBOT_DESKTOP_SERVICE", "1")
	if !wantsDesktop() {
		t.Fatal("ANTBOT_DESKTOP_SERVICE=1 doesn't supervise the desktop")
	}
	t.Setenv("ANTBOT_DESKTOP_SERVICE", "0")
	if wantsDesktop() {
		t.Fatal("ANTBOT_DESKTOP_SERVICE=0 supervises the desktop")
	}
	for _, env := range []map[string]string{{"MIRRIN_SERVICE": "1"}, {"ANTBOT_SERVICE": "1"}} {
		if !hasServiceMarker(env) {
			t.Errorf("%v has no marker", env)
		}
	}
	if hasServiceMarker(map[string]string{"HOME": "/Users/me"}) {
		t.Error("a unit without either marker has one")
	}
}

// The service runs Mirrin.app when it is installed, else this program;
// never AntBot.app, which only ever holds older code.
func TestServiceProgramNeverPicksAntBotsApp(t *testing.T) {
	self := "/Users/me/go/bin/mirrin"
	have := map[string]bool{"/Applications/AntBot.app/Contents/MacOS/antbot": true}
	exists := func(p string) bool { return have[p] }
	if got := serviceProgram("darwin", self, exists); got != self {
		t.Fatalf("with only AntBot.app: %q", got)
	}
	have[macApp] = true
	if got := serviceProgram("darwin", self, exists); got != macApp {
		t.Fatalf("with Mirrin.app: %q", got)
	}
	if got := serviceProgram("linux", self, exists); got != self {
		t.Fatalf("on Linux: %q", got)
	}
}
