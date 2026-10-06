package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A service set up while the twin's move waited names the old home for its
// logs and as MIRRIN_HOME. Once the home has moved, those paths lead
// nowhere (systemd won't start the unit), so they are pointed at the home
// as it is now; while the old home is still there, nothing changes.
func TestRehomeUnitAfterTheMove(t *testing.T) {
	user := t.TempDir()
	old, home := filepath.Join(user, ".antbot"), filepath.Join(user, ".mirrin")
	if err := os.MkdirAll(filepath.Join(home, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>MIRRIN_HOME</key>
		<string>` + old + `</string>
		<key>MIRRIN_SERVICE</key>
		<string>1</string>
		<key>OTHER</key>
		<string>/srv` + old + `</string>
	</dict>
	<key>Label</key>
	<string>mirrin</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Applications/Mirrin.app/Contents/MacOS/mirrin</string>
		<string>tray</string>
	</array>
	<key>StandardErrorPath</key>
	<string>` + old + `/logs/mirrin.err.log</string>
	<key>StandardOutPath</key>
	<string>` + old + `/logs/mirrin.out.log</string>
	<key>WorkingDirectory</key>
	<string>` + old + `-old/x</string>
</dict>
</plist>
`
	unit := "[Service]\nExecStart=/usr/local/bin/mirrin run\nStandardOutput=append:" + old + "/logs/mirrin.out\nStandardError=append:" + old + "/logs/mirrin.err\nRestart=always\nEnvironment=MIRRIN_HOME=" + old + "\nEnvironment=OTHER=/srv" + old + "\nWorkingDirectory=" + old + "-old/x\n"
	for _, c := range []struct{ name, body string }{{"mirrin.plist", plist}, {"mirrin.service", unit}} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), c.name)
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}

			// The move still waits: the old home is there.
			if err := os.MkdirAll(old, 0o700); err != nil {
				t.Fatal(err)
			}
			if changed, err := rehomeUnit(path, user, old); err != nil || changed {
				t.Fatalf("rewritten while the twin is still in the old home: %v %v", changed, err)
			}
			if changed, err := rehomeUnit(path, user, home); err != nil || changed {
				t.Fatalf("rewritten before the old home moved: %v %v", changed, err)
			}

			if err := os.Remove(old); err != nil {
				t.Fatal(err)
			}
			changed, err := rehomeUnit(path, user, home)
			if err != nil || !changed {
				t.Fatalf("not rewritten after the move: %v %v", changed, err)
			}
			b, _ := os.ReadFile(path)
			got := string(b)
			for _, want := range []string{home + "/logs/mirrin.", old + "-old/x", "/srv" + old} {
				if !strings.Contains(got, want) {
					t.Errorf("lacks %q:\n%s", want, got)
				}
			}
			if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(got, old+"-old", ""), "/srv"+old, ""), old) {
				t.Errorf("still names the old home:\n%s", got)
			}
			if env := unitEnv(path); env["MIRRIN_HOME"] != home {
				t.Errorf("MIRRIN_HOME = %q", env["MIRRIN_HOME"])
			}
			if st, _ := os.Stat(path); st.Mode().Perm() != 0o644 {
				t.Errorf("mode changed to %v", st.Mode().Perm())
			}
			if changed, err := rehomeUnit(path, user, home); err != nil || changed {
				t.Fatalf("a second run changed something: %v %v", changed, err)
			}
		})
	}
}

// `mirrin service install` doesn't set up a service in a home from before
// the rename while a twin still runs from it: the service would keep that
// home's paths once it moves.
func TestInstallWaitsForTheOldTwin(t *testing.T) {
	user := t.TempDir()
	old := filepath.Join(user, ".antbot")
	busy := func(string) bool { return true }
	free := func(string) bool { return false }
	err := homeInUse(old, user, "install", busy)
	if err == nil || !strings.Contains(err.Error(), "still running from "+old) || !strings.Contains(err.Error(), "`mirrin service install` again") {
		t.Fatalf("got %v", err)
	}
	for _, c := range []struct {
		home   string
		inUse  func(string) bool
		reason string
	}{
		{old, free, "the old twin quit"},
		{filepath.Join(user, ".mirrin"), busy, "the home has moved"},
		{filepath.Join(t.TempDir(), "twin"), busy, "a home of its own"},
	} {
		if err := homeInUse(c.home, user, "install", c.inUse); err != nil {
			t.Errorf("%s: %v", c.reason, err)
		}
	}
}
