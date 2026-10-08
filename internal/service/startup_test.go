package service

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// fakeStartup is a startup whose tray is pretend.
type fakeStartup struct {
	running  bool
	launched [][]string // env of each launch
	stops    int
}

func newFakeStartup(t *testing.T, home string) (startup, *fakeStartup, *strings.Builder) {
	t.Helper()
	f := &fakeStartup{}
	var out strings.Builder
	s := startup{
		dir: filepath.Join(t.TempDir(), "Startup"), exe: `C:\Program Files\Mirrin 100%\mirrin.exe`, home: home,
		out: &out, wait: time.Second,
		launch: func(exe string, env []string) error {
			f.launched = append(f.launched, env)
			f.running = true
			return nil
		},
		stopTwin: func() error { f.stops++; f.running = false; return nil },
		running:  func() bool { return f.running },
	}
	return s, f, &out
}

// The Startup script sets MIRRIN_HOME and MIRRIN_SERVICE and starts the
// tray; a % in a path survives cmd.
func TestStartupScriptCarriesTheServiceEnvironment(t *testing.T) {
	script := startupScript(`C:\Program Files\Mirrin 100%\mirrin.exe`, `C:\Users\José\.mirrin`)
	env := startupEnv([]byte(script))
	if env["MIRRIN_HOME"] != `C:\Users\José\.mirrin` || env[config.ServiceEnv] != "1" || config.ServiceEnv != "MIRRIN_SERVICE" {
		t.Fatalf("env %v from:\n%s", env, script)
	}
	for _, want := range []string{"chcp 65001", `start "" /min "C:\Program Files\Mirrin 100%%\mirrin.exe" tray`, "\r\n"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
}

func TestStartupInstallWritesTheEntryAndStartsTheTray(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	s, f, out := newFakeStartup(t, home)
	s.secretEnvs = []string{"OPENAI_API_KEY"}

	if err := s.control("install"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(s.dir, startupName))
	if err != nil {
		t.Fatal(err)
	}
	if env := startupEnv(b); env["MIRRIN_HOME"] != home || env[config.ServiceEnv] != "1" {
		t.Fatalf("entry env %v:\n%s", env, b)
	}
	if !s.installed() {
		t.Fatal("not installed")
	}
	if len(f.launched) != 1 || !slices.Contains(f.launched[0], "MIRRIN_HOME="+home) || !slices.Contains(f.launched[0], config.ServiceEnv+"=1") {
		t.Fatalf("launched %v", f.launched)
	}
	for _, kv := range f.launched[0] {
		if strings.Contains(kv, "sk-openai") {
			t.Fatal("the tray was started with a key in its environment")
		}
	}
	if got, _ := config.ReadSecrets(); got["OPENAI_API_KEY"] != "sk-openai" {
		t.Fatal("the key wasn't kept for the background twin")
	}
	if !strings.Contains(out.String(), "running in the background") {
		t.Fatalf("output:\n%s", out)
	}

	// A reinstall quits the running tray and starts the new one.
	if err := s.control("install"); err != nil || f.stops != 1 || len(f.launched) != 2 {
		t.Fatalf("reinstall: %v, stops %d, launches %d", err, f.stops, len(f.launched))
	}

	// Another home's entry isn't this one's.
	other := s
	other.home = t.TempDir()
	if other.installed() {
		t.Fatal("another home's entry counted as installed")
	}

	if err := s.control("uninstall"); err != nil {
		t.Fatal(err)
	}
	if s.installed() || f.running {
		t.Fatalf("uninstall left the entry (%v) or the tray running (%v)", s.installed(), f.running)
	}
}

func TestStartupStartStopRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	s, f, out := newFakeStartup(t, home)
	if err := s.control("start"); err == nil || !strings.Contains(err.Error(), "mirrin service install") {
		t.Fatalf("start before install: %v", err)
	}
	if err := s.write(); err != nil {
		t.Fatal(err)
	}
	if err := s.control("start"); err != nil || len(f.launched) != 1 {
		t.Fatalf("start: %v %d", err, len(f.launched))
	}
	if err := s.control("start"); err != nil || len(f.launched) != 1 {
		t.Fatalf("start while running launched another: %v %d", err, len(f.launched))
	}
	if err := s.control("restart"); err != nil || f.stops != 1 || len(f.launched) != 2 {
		t.Fatalf("restart: %v", err)
	}
	if err := s.control("stop"); err != nil || f.running {
		t.Fatalf("stop: %v", err)
	}
	out.Reset()
	if err := s.control("status"); err != nil || !strings.Contains(out.String(), "installed, but not running") {
		t.Fatalf("status: %v %s", err, out)
	}
	s.stopTwin = func() error { return errors.New("access denied") }
	if err := s.control("restart"); err == nil {
		t.Fatal("restart went on after the old tray wouldn't quit")
	}
}

// The setup program writes the same sign-in entry as `mirrin service
// install`, and removes it again.
func TestSetupProgramWritesTheSameStartupEntry(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "windows", "mirrin.iss"))
	if err != nil {
		t.Fatal(err)
	}
	iss := string(b)
	for _, line := range strings.Split(strings.TrimSpace(startupScript("<exe>", "<home>")), "\r\n") {
		want := strings.NewReplacer("<exe>", "' + Exe + '", "<home>", "' + Home + '").Replace(line)
		if !strings.Contains(iss, "'"+want+"'") && !strings.Contains(iss, "'"+want) {
			t.Errorf("mirrin.iss's StartupScript lacks %q", want)
		}
	}
	if !strings.Contains(iss, `{userstartup}\`+startupName) {
		t.Error("mirrin.iss doesn't remove its sign-in entry")
	}
}

// From an administrator terminal, install sets up the sign-in entry but
// doesn't start the twin with administrator rights, and start refuses.
func TestStartupNeverStartsTheTrayElevated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	s, f, out := newFakeStartup(t, home)
	s.elevated = func() bool { return true }
	if err := s.control("install"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !s.installed() || len(f.launched) != 0 {
		t.Fatalf("installed %v, launched %d", s.installed(), len(f.launched))
	}
	if !strings.Contains(out.String(), "normal terminal") {
		t.Fatalf("output:\n%s", out)
	}
	for _, action := range []string{"start", "restart"} {
		if err := s.control(action); !errors.Is(err, errElevated) || len(f.launched) != 0 {
			t.Fatalf("%s: %v, launched %d", action, err, len(f.launched))
		}
	}
}

// Only this home's tray is this home's twin: not another profile's, and not
// a `mirrin run` in a terminal.
func TestOwnTraysAreThisHomesTrays(t *testing.T) {
	list := "10\t\"C:\\Mirrin\\mirrin.exe\" tray\r\n11\tmirrin.exe chat\r\n12\tC:\\Mirrin\\mirrin.exe run\r\n13\tmirrin.exe tray\r\n14\tmirrin.exe tray\r\nnot a line\r\n"
	// 10 and 12 hold this home; 14 is another profile's tray; 13 is this command.
	if got := ownTrays(list, 13, []int{10, 12, 13}); !slices.Equal(got, []int{10}) {
		t.Fatalf("ownTrays = %v", got)
	}
	if got := ownTrays(list, 13, nil); len(got) != 0 {
		t.Fatalf("with nothing holding this home: %v", got)
	}
}

// A service definition without MIRRIN_HOME runs the default home, which is
// still this home when MIRRIN_HOME names it explicitly.
func TestOwnUnitWithTheDefaultHomeNamed(t *testing.T) {
	uh, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	t.Setenv("MIRRIN_HOME", filepath.Join(uh, ".mirrin"))
	if !ownUnit(map[string]string{"PATH": "x"}) {
		t.Fatal("the default home, named, didn't own a unit without MIRRIN_HOME")
	}
	t.Setenv("MIRRIN_HOME", t.TempDir())
	if ownUnit(map[string]string{"PATH": "x"}) {
		t.Fatal("another home owned a unit without MIRRIN_HOME")
	}
}
