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
	legacy, retired := true, false
	s.legacy = func() bool { return legacy }
	s.retire = func() error { retired, legacy = true, false; return nil }
	writeFile(t, filepath.Join(s.dir, startupLink), "old shortcut")
	writeFile(t, filepath.Join(s.dir, legacyStartupName), antbotEntry(home))

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
	if !s.installed() || !retired {
		t.Fatalf("installed %v, retired the old service %v", s.installed(), retired)
	}
	for _, old := range []string{startupLink, legacyStartupName} {
		if _, err := os.Stat(filepath.Join(s.dir, old)); err == nil {
			t.Fatalf("left %s, which would start a second tray", old)
		}
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

// fakeRegistry is a service's Environment value.
type fakeRegistry struct {
	lines    []string
	writeErr error
	writes   int
}

func (r *fakeRegistry) Read() ([]string, error) {
	if r.lines == nil {
		return nil, errors.New("the system cannot find the file specified")
	}
	return r.lines, nil
}

func (r *fakeRegistry) Write(l []string) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	r.writes++
	r.lines = l
	return nil
}

func TestTidyRegistryMovesKeysOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	// The service an earlier version installed names its home the way AntBot did.
	r := &fakeRegistry{lines: []string{"ANTBOT_HOME=" + home, "PATH=C:\\Windows", "ANTHROPIC_API_KEY=sk-ant", "TELEGRAM_BOT_TOKEN=1:tg", "ANTBOT_EMAIL_PASSWORD=mail"}}
	var out strings.Builder
	tidyRegistryEnv(r, &out)
	got, _ := config.ReadSecrets()
	if got["ANTHROPIC_API_KEY"] != "sk-ant" || got["TELEGRAM_BOT_TOKEN"] != "1:tg" || got["MIRRIN_EMAIL_PASSWORD"] != "mail" {
		t.Fatalf("keys not kept: %v", got)
	}
	want := []string{"ANTBOT_HOME=" + home, "PATH=C:\\Windows", config.ServiceEnv + "=1"}
	if !slices.Equal(r.lines, want) {
		t.Fatalf("registry now %v, want %v", r.lines, want)
	}
	if !strings.Contains(out.String(), "ANTBOT_EMAIL_PASSWORD, ANTHROPIC_API_KEY, TELEGRAM_BOT_TOKEN") || strings.Contains(out.String(), "sk-ant") {
		t.Fatalf("output:\n%s", out.String())
	}

	// Tidy already: nothing written. AntBot's marker counts as one.
	tidyRegistryEnv(r, &out)
	if r.writes != 1 {
		t.Fatalf("rewrote a tidy registry: %d writes", r.writes)
	}
	marked := &fakeRegistry{lines: []string{"ANTBOT_HOME=" + home, "ANTBOT_SERVICE=1"}}
	tidyRegistryEnv(marked, &out)
	if marked.writes != 0 {
		t.Fatalf("rewrote a registry with AntBot's marker: %v", marked.lines)
	}

	// Another home's service, or none, is left alone.
	r = &fakeRegistry{lines: []string{"ANTBOT_HOME=" + t.TempDir(), "OPENAI_API_KEY=sk-other"}}
	tidyRegistryEnv(r, &out)
	if r.writes != 0 {
		t.Fatal("touched another home's service")
	}
	tidyRegistryEnv(&fakeRegistry{}, &out)

	// Without administrator rights the keys are still kept, and it says how.
	r = &fakeRegistry{lines: []string{"ANTBOT_HOME=" + home, "GEMINI_API_KEY=g-key"}, writeErr: errors.New("Access is denied.")}
	out.Reset()
	tidyRegistryEnv(r, &out)
	if got, _ := config.ReadSecrets(); got["GEMINI_API_KEY"] != "g-key" {
		t.Fatal("key not kept when the registry can't be written")
	}
	if !strings.Contains(out.String(), "sc.exe delete antbot") || strings.Contains(out.String(), "service install") {
		t.Fatalf("output:\n%s", out.String())
	}
	// The advice isn't repeated on every command.
	out.Reset()
	r.lines = []string{"ANTBOT_HOME=" + home, "GEMINI_API_KEY=g-key"}
	tidyRegistryEnv(r, &out)
	if out.Len() != 0 {
		t.Fatalf("said it again:\n%s", out.String())
	}
}

// The setup program writes the same sign-in entry as `mirrin service
// install`, and removes the ones earlier versions left.
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
	if !strings.Contains(iss, `{userstartup}\`+startupName) || !strings.Contains(iss, `\`+startupLink) || !strings.Contains(iss, `\`+legacyStartupName) {
		t.Error("mirrin.iss doesn't remove its sign-in entry, or leaves AntBot's entry or the old shortcut")
	}
}

// The setup program keeps AntBot's AppId, so it upgrades an AntBot install
// in place: it stops the old trays, takes away the old program and its
// shortcut, and turns AntBot's sign-in entry into Mirrin's.
func TestSetupProgramUpgradesAntBotInPlace(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "windows", "mirrin.iss"))
	if err != nil {
		t.Fatal(err)
	}
	iss := string(b)
	for _, want := range []string{
		"AppId={{7E1C6E4E-4C5C-4A5E-9C2B-ANTBOT01}",
		"'/F /IM antbot.exe'", "'/F /IM openhuman.exe'",
		`DeleteFile(ExpandConstant('{app}\antbot.exe'))`,
		`FileExists(ExpandConstant('{userstartup}\AntBot.cmd'))`,
		`'set "ANTBOT_HOME='`,
		"OPENHUMAN01}_is1",
	} {
		if !strings.Contains(iss, want) {
			t.Errorf("mirrin.iss lacks %s", want)
		}
	}
}

// An older install's Windows service (LocalSystem) counts as installed until
// it's gone: uninstall removes it, and install won't set up a second twin
// beside it when it can't.
func TestStartupRetiresTheOldWindowsService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	s, f, out := newFakeStartup(t, home)
	legacy, retires := true, 0
	denied := errors.New("Access is denied.")
	var retireErr error
	s.legacy = func() bool { return legacy }
	s.retire = func() error {
		retires++
		if retireErr != nil {
			return retireErr
		}
		legacy = false
		return nil
	}
	if s.installed() || !s.anyInstalled() {
		t.Fatal("the old service alone didn't count as installed")
	}

	// Uninstall with only the old service removes it.
	if err := s.control("uninstall"); err != nil || retires != 1 || legacy {
		t.Fatalf("uninstall: %v, retires %d, still there %v\n%s", err, retires, legacy, out)
	}
	if s.anyInstalled() {
		t.Fatal("still installed after uninstall")
	}

	// Without administrator rights, install stops and says how, rather than
	// adding a second twin that starts at sign-in.
	legacy, retireErr = true, denied
	out.Reset()
	err := s.control("install")
	if err == nil || !strings.Contains(err.Error(), "sc.exe delete antbot") || strings.Contains(err.Error(), "as administrator and run `mirrin service install`") {
		t.Fatalf("install beside the old service: %v", err)
	}
	if s.installed() || len(f.launched) != 0 {
		t.Fatalf("wrote the entry (%v) or started a tray (%d) beside the old service", s.installed(), len(f.launched))
	}
	// Uninstall still says the old service is there.
	if err := s.control("uninstall"); err == nil || !strings.Contains(err.Error(), "sc.exe delete") {
		t.Fatalf("uninstall kept quiet about the old service: %v", err)
	}
	// So do restart and status.
	if err := s.control("restart"); err == nil || len(f.launched) != 0 {
		t.Fatalf("restart with the old service: %v", err)
	}
	out.Reset()
	if err := s.control("status"); err != nil || !strings.Contains(out.String(), "sc.exe delete") || strings.Contains(out.String(), "not installed") {
		t.Fatalf("status: %v\n%s", err, out)
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
// a `mirrin run` in a terminal. AntBot's tray (antbot.exe) holding this
// home is one.
func TestOwnTraysAreThisHomesTrays(t *testing.T) {
	list := "10\t\"C:\\Mirrin\\mirrin.exe\" tray\r\n11\tmirrin.exe chat\r\n12\tC:\\Mirrin\\mirrin.exe run\r\n13\tmirrin.exe tray\r\n14\tmirrin.exe tray\r\n15\t\"C:\\AntBot\\antbot.exe\" tray\r\nnot a line\r\n"
	// 10, 12 and 15 hold this home; 14 is another profile's tray; 13 is this command.
	if got := ownTrays(list, 13, []int{10, 12, 13, 15}); !slices.Equal(got, []int{10, 15}) {
		t.Fatalf("ownTrays = %v", got)
	}
	if got := ownTrays(list, 13, nil); len(got) != 0 {
		t.Fatalf("with nothing holding this home: %v", got)
	}
}

// A service definition without MIRRIN_HOME runs the default home, which is
// still this home when MIRRIN_HOME (or AntBot's ANTBOT_HOME) names it
// explicitly.
func TestOwnUnitWithTheDefaultHomeNamed(t *testing.T) {
	uh, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	t.Setenv("ANTBOT_HOME", "")
	t.Setenv("OPENHUMAN_HOME", "")
	t.Setenv("MIRRIN_HOME", filepath.Join(uh, ".mirrin"))
	if !ownUnit(map[string]string{"PATH": "x"}) {
		t.Fatal("the default home, named, didn't own a unit without MIRRIN_HOME")
	}
	t.Setenv("MIRRIN_HOME", "")
	t.Setenv("ANTBOT_HOME", filepath.Join(uh, ".antbot"))
	if !ownUnit(map[string]string{"PATH": "x"}) {
		t.Fatal("AntBot's default home, named, didn't own a unit without MIRRIN_HOME")
	}
	t.Setenv("MIRRIN_HOME", t.TempDir())
	if ownUnit(map[string]string{"PATH": "x"}) {
		t.Fatal("another home owned a unit without MIRRIN_HOME")
	}
}

// antbotEntry is the Startup script AntBot wrote for home.
func antbotEntry(home string) string {
	return "@echo off\r\nchcp 65001 >nul\r\n" +
		`set "ANTBOT_HOME=` + home + "\"\r\n" +
		`set "ANTBOT_SERVICE=1"` + "\r\n" +
		`start "" /min "C:\Users\me\AppData\Local\Programs\AntBot\antbot.exe" tray` + "\r\n"
}

// AntBot's Startup entry counts as installed for its home, and becomes
// Mirrin's (one entry, starting this program); another home's is left.
func TestStartupReplacesAntBotsEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	s, _, out := newFakeStartup(t, home)
	old := filepath.Join(s.dir, legacyStartupName)
	writeFile(t, old, antbotEntry(home))
	if !s.installed() {
		t.Fatal("AntBot's entry for this home isn't counted as installed")
	}
	s.tidyLegacyEntry()
	if _, err := os.Stat(old); err == nil {
		t.Fatal("AntBot's entry is still there beside Mirrin's")
	}
	b, err := os.ReadFile(s.path())
	if err != nil {
		t.Fatalf("no Mirrin entry: %v\n%s", err, out)
	}
	if env := startupEnv(b); env["MIRRIN_HOME"] != home || !strings.Contains(string(b), strings.ReplaceAll(s.exe, "%", "%%")) {
		t.Fatalf("entry:\n%s", b)
	}
	if !strings.Contains(out.String(), "in place of AntBot") {
		t.Fatalf("output:\n%s", out)
	}
	// Once is enough.
	out.Reset()
	s.tidyLegacyEntry()
	if out.Len() != 0 {
		t.Fatalf("said it again:\n%s", out)
	}

	// Another home's entry isn't this one's to replace.
	other, _, _ := newFakeStartup(t, home)
	writeFile(t, filepath.Join(other.dir, legacyStartupName), antbotEntry(t.TempDir()))
	if other.installed() {
		t.Fatal("another home's AntBot entry counted as installed")
	}
	other.tidyLegacyEntry()
	if _, err := os.Stat(filepath.Join(other.dir, legacyStartupName)); err != nil {
		t.Fatal("another home's entry was removed")
	}
	if _, err := os.Stat(other.path()); err == nil {
		t.Fatal("wrote an entry for another home's")
	}
}
