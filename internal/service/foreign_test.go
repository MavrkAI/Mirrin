package service

import (
	"path/filepath"
	"testing"
)

// testPlist is a LaunchAgent for name, which runs program for home
// (named by homeVar).
func testPlist(name, program, homeVar, home string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + name + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + program + `</string>
		<string>run</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>` + homeVar + `</key><string>` + home + `</string>
	</dict>
	<key>KeepAlive</key><true/>
</dict>
</plist>
`
}

// A service installed for another home (the owner's, seen from a command
// run with MIRRIN_HOME set) is reported with the program it keeps running;
// this home's own service isn't.
func TestForeignUnit(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "mirrin.plist")
	writeFile(t, current, testPlist("mirrin", "/Users/me/.local/bin/mirrin", "MIRRIN_HOME", "/Users/me/twins/work"))
	currentUnit := filepath.Join(dir, "mirrin.service")
	writeFile(t, currentUnit, "[Service]\nExecStart=/home/me/.local/bin/mirrin run\nEnvironment=MIRRIN_HOME=/home/me/twins/work\n")
	spaced := filepath.Join(dir, "spaced.service")
	writeFile(t, spaced, "[Service]\nExecStart=/home/me/my\\x20apps/mirrin run\nEnvironment=MIRRIN_HOME=/home/me/twins/work\n")
	older := filepath.Join(dir, "older.service")
	writeFile(t, older, "[Service]\nExecStart=/usr/local/bin/mirrin run\nEnvironment=HOME=/home/me\n")

	t.Setenv("MIRRIN_HOME", t.TempDir())
	for path, want := range map[string]string{
		current: "/Users/me/.local/bin/mirrin", currentUnit: "/home/me/.local/bin/mirrin",
		spaced: "/home/me/my apps/mirrin", older: "/usr/local/bin/mirrin",
	} {
		got, program, ok := foreignUnitAt(path)
		if !ok || got != path || program != want {
			t.Errorf("foreignUnitAt(%s) = %q, %q, %v; want the program %q", filepath.Base(path), got, program, ok, want)
		}
	}
	if _, _, ok := foreignUnitAt(filepath.Join(dir, "none.plist")); ok {
		t.Error("a missing definition is foreign")
	}
	if _, _, ok := foreignUnitAt(""); ok {
		t.Error("no definition (Windows) is foreign")
	}

	// Run for the home the service was installed for, it's this home's own.
	t.Setenv("MIRRIN_HOME", "/Users/me/twins/work")
	if _, _, ok := foreignUnitAt(current); ok {
		t.Error("this home's own service is foreign")
	}
	t.Setenv("MIRRIN_HOME", "")
	if _, _, ok := foreignUnitAt(older); ok {
		t.Error("an older unit is foreign to the default home")
	}
}

func TestUnitProgram(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]string{
		"[Service]\nExecStart=\"/opt/My Apps/mirrin\" run\n": "/opt/My Apps/mirrin",
		"[Service]\nExecStart=-/usr/bin/mirrin run\n":        "/usr/bin/mirrin",
		"[Service]\nRestart=always\n":                        "",
	} {
		p := filepath.Join(dir, "mirrin.service")
		writeFile(t, p, body)
		if got := unitProgram(p); got != want {
			t.Errorf("unitProgram(%q) = %q, want %q", body, got, want)
		}
	}
	for name, program := range map[string]string{
		"mirrin":     "/Applications/Mirrin.app/Contents/MacOS/mirrin",
		"mirrin-cli": "/usr/local/bin/mirrin",
	} {
		p := filepath.Join(dir, name+".plist")
		writeFile(t, p, `<plist><dict><key>Label</key><string>`+name+`</string><key>Program</key><string>`+program+`</string></dict></plist>`)
		if got := unitProgram(p); got != program {
			t.Errorf("Program key: %q, want %q", got, program)
		}
	}
}
