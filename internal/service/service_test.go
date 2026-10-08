package service

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kardianos/service"

	"github.com/MavrkAI/Mirrin/internal/config"
)

type fakeUnit struct {
	installed, running bool
	calls              []string
}

func (f *fakeUnit) Status() (service.Status, error) {
	switch {
	case !f.installed:
		return service.StatusUnknown, service.ErrNotInstalled
	case f.running:
		return service.StatusRunning, nil
	}
	return service.StatusStopped, nil
}

func (f *fakeUnit) Install() error {
	f.calls = append(f.calls, "install")
	if f.installed {
		return errors.New("Init already exists")
	}
	f.installed = true
	return nil
}

func (f *fakeUnit) Uninstall() error {
	f.calls = append(f.calls, "uninstall")
	f.installed = false
	return nil
}

func (f *fakeUnit) Start() error { f.calls = append(f.calls, "start"); f.running = true; return nil }
func (f *fakeUnit) Stop() error  { f.calls = append(f.calls, "stop"); f.running = false; return nil }

func TestEnvCarriesNoSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret")
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:secret")
	env := Env()
	for k, v := range env {
		if strings.Contains(v, "secret") {
			t.Fatalf("%s=%s would be written into the unit file", k, v)
		}
	}
	if env["MIRRIN_HOME"] != home || env[config.ServiceEnv] != "1" {
		t.Fatalf("MIRRIN_HOME = %q, want %q so the service finds this home on every OS (env %v)", env["MIRRIN_HOME"], home, env)
	}
}

func TestInstallKeepsSecretsStartsAndWaits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	writeFile(t, config.SecretsPathIn(home), "MIRRIN_S3_ACCESS_KEY_ID=\"kept\"\n")

	// An older install wrote keys into its plist; the shell's value wins over it.
	plist := filepath.Join(t.TempDir(), "mirrin.plist")
	writeFile(t, plist, `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
	<key>Disabled</key><false/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key><string>/Users/me</string>
		<key>ANTHROPIC_API_KEY</key><string>sk-ant-&amp;old</string>
		<key>OPENAI_API_KEY</key><string>sk-openai-stale</string>
		<key>MIRRIN_EMAIL_PASSWORD</key><string>app-pass</string>
	</dict>
	<key>Label</key><string>mirrin</string>
</dict></plist>`)
	// A systemd unit with keys too. A key the secrets file already has isn't
	// overwritten.
	unitFile := filepath.Join(t.TempDir(), "mirrin.service")
	writeFile(t, unitFile, "[Service]\nExecStart=/usr/bin/mirrin run\nEnvironment=MIRRIN_S3_ACCESS_KEY_ID=stale\nEnvironment=MIRRIN_S3_SECRET_ACCESS_KEY=s3-secret\n")

	svc := &fakeUnit{installed: true, running: true} // reinstall over an existing one
	started := false
	var out strings.Builder
	in := installer{
		svc: svc, out: &out, wait: time.Second,
		start:      func() error { started = true; return svc.Start() },
		up:         func() bool { return started },
		secretEnvs: []string{"OPENAI_API_KEY", "DISCORD_BOT_TOKEN"},
		unitFiles:  []string{plist, unitFile},
	}
	if err := in.run(); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	if !started || !svc.installed {
		t.Fatalf("service not installed and started: %v", svc.calls)
	}
	if got := strings.Join(svc.calls, ","); got != "stop,uninstall,install,start" {
		t.Fatalf("existing service not replaced cleanly: %s", got)
	}
	st, err := os.Stat(config.SecretsPath())
	if err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file: %v %v", st, err)
	}
	got, _ := config.ReadSecrets()
	want := map[string]string{
		"OPENAI_API_KEY": "sk-openai", "ANTHROPIC_API_KEY": "sk-ant-&old",
		"MIRRIN_EMAIL_PASSWORD": "app-pass", "MIRRIN_S3_SECRET_ACCESS_KEY": "s3-secret",
		"MIRRIN_S3_ACCESS_KEY_ID": "kept",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("secret %s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["HOME"]; ok {
		t.Error("HOME is not a secret")
	}
	if strings.Contains(out.String(), "sk-") {
		t.Fatalf("a secret value was printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "running in the background") {
		t.Fatalf("no success line:\n%s", out.String())
	}
}

func TestInstallReportsWhenItDoesNotComeUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	writeFile(t, filepath.Join(home, "logs", "mirrin.err.log"), "time=x level=INFO msg=starting\nerror: OpenAI needs an API key.\n")
	svc := &fakeUnit{}
	var out strings.Builder
	in := installer{svc: svc, out: &out, wait: 50 * time.Millisecond,
		start: svc.Start, up: func() bool { return false }}
	err := in.run()
	if err == nil {
		t.Fatal("install claimed success although nothing answered")
	}
	if !strings.Contains(out.String(), "Last error: OpenAI needs an API key.") {
		t.Fatalf("the service's last error wasn't shown:\n%s", out.String())
	}
}

func TestLastErrorIsRecentAndReadable(t *testing.T) {
	cases := []struct{ name, log, want string }{
		{"a model error in plain words",
			"time=1 level=INFO msg=\"Mirrin online\"\ntime=2 level=ERROR msg=handle chat=cli:t err=\"anthropic 401: {\\\"type\\\":\\\"error\\\",\\\"error\\\":{\\\"type\\\":\\\"authentication_error\\\"}}\"\n",
			"Anthropic didn't accept the API key. Check it at https://console.anthropic.com/settings/keys, then run `mirrin init` to update it."},
		{"errors before the last good start are old news",
			"time=1 level=ERROR msg=\"channel stopped\" err=\"telegram 401: Unauthorized\"\ntime=2 level=INFO msg=\"Mirrin online\" model=x\ntime=3 level=INFO msg=ok\n", ""},
		{"a channel error keeps its context",
			"time=1 level=INFO msg=\"Mirrin online\"\ntime=2 level=ERROR msg=\"channel stopped\" channel=telegram err=\"telegram 401: Unauthorized\"\n",
			"channel stopped: telegram 401: Unauthorized"},
		{"a crash before logging", "panic: boom\n", "panic: boom"},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "mirrin.err.log")
		writeFile(t, p, c.log)
		if got := lastErrorLine(p); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}

	// Only the end of a log that has grown for months is read.
	p := filepath.Join(t.TempDir(), "mirrin.err.log")
	old := strings.Repeat("time=0 level=ERROR msg=ancient err=\"from long ago\"\n", 10000)
	writeFile(t, p, old+"time=9 level=ERROR msg=recent err=\"disk full\"\n")
	if got := lastErrorLine(p); got != "recent: disk full" {
		t.Fatalf("got %q", got)
	}
}

func TestStateOf(t *testing.T) {
	cases := []struct {
		st                 service.Status
		err                error
		installed, running bool
	}{
		{service.StatusRunning, nil, true, true},
		{service.StatusStopped, nil, true, false},
		{service.StatusUnknown, service.ErrNotInstalled, false, false},
		{service.StatusUnknown, errors.New("service in failed state"), true, false}, // systemd
		{service.StatusUnknown, errors.New("Access is denied."), true, false},       // Windows, not an admin
	}
	for _, c := range cases {
		if i, r := stateOf(c.st, c.err); i != c.installed || r != c.running {
			t.Errorf("stateOf(%v, %v) = %v %v, want %v %v", c.st, c.err, i, r, c.installed, c.running)
		}
	}
}

func TestTidyUnitMovesKeysOutOfAnOldUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	reloads := 0
	defer func(f func()) { reloadUnits = f }(reloadUnits)
	reloadUnits = func() { reloads++ }
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>mirrin</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>/Users/me</string>
		<key>MIRRIN_HOME</key>
		<string>` + home + `</string>
		<key>ANTHROPIC_API_KEY</key>
		<string>sk-ant-&amp;x</string>
		<key>TELEGRAM_BOT_TOKEN</key><string>1:tg</string>
	</dict>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>
`
	unit := "[Unit]\nDescription=Mirrin\n\n[Service]\nExecStart=/usr/bin/mirrin run\nEnvironment=HOME=/home/me\nEnvironment=MIRRIN_HOME=" + home + "\nEnvironment=MIRRIN_EMAIL_PASSWORD=app pass\nRestart=always\n\n[Install]\nWantedBy=default.target\n"
	for _, c := range []struct{ name, body string }{{"mirrin.plist", plist}, {"mirrin.service", unit}} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), c.name)
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			tidyUnit(path, &out)
			env := unitEnv(path)
			for k := range env {
				if looksSecret(k) {
					t.Errorf("%s is still in the unit file", k)
				}
			}
			if env["HOME"] == "" || env["MIRRIN_HOME"] != home || env[config.ServiceEnv] != "1" {
				t.Errorf("env after = %v", env)
			}
			if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o644 {
				t.Errorf("mode changed to %v", st.Mode().Perm())
			}
			if !strings.Contains(out.String(), "Moved") || strings.Contains(out.String(), "sk-ant") {
				t.Errorf("output: %q", out.String())
			}
			saved, _ := config.ReadSecrets()
			if saved["ANTHROPIC_API_KEY"] == "" && saved["MIRRIN_EMAIL_PASSWORD"] == "" {
				t.Errorf("the keys weren't kept: %v", saved)
			}

			// Once is enough.
			before, _ := os.ReadFile(path)
			out.Reset()
			tidyUnit(path, &out)
			if after, _ := os.ReadFile(path); string(after) != string(before) || out.Len() > 0 {
				t.Errorf("a second run changed something: %q", out.String())
			}
		})
	}
	saved, _ := config.ReadSecrets()
	if saved["ANTHROPIC_API_KEY"] != "sk-ant-&x" || saved["TELEGRAM_BOT_TOKEN"] != "1:tg" || saved["MIRRIN_EMAIL_PASSWORD"] != "app pass" {
		t.Fatalf("secrets = %v", saved)
	}
	if reloads != 2 {
		t.Fatalf("systemd reloaded %d times, want once per changed unit", reloads)
	}

	// A command run with another home leaves the owner's service alone.
	other := filepath.Join(t.TempDir(), "mirrin.plist")
	writeFile(t, other, strings.Replace(plist, home, "/Users/someone-else/twin", 1))
	before, _ := os.ReadFile(other)
	tidyUnit(other, io.Discard)
	if after, _ := os.ReadFile(other); string(after) != string(before) {
		t.Fatal("a test's home rewrote the owner's service")
	}

	// A unit with the marker and no keys isn't rewritten on every start.
	marked := filepath.Join(t.TempDir(), "mirrin.service")
	writeFile(t, marked, "[Service]\nExecStart=/usr/bin/mirrin run\nEnvironment=MIRRIN_HOME="+home+"\nEnvironment=MIRRIN_SERVICE=1\n")
	before, _ = os.ReadFile(marked)
	tidyUnit(marked, io.Discard)
	if after, _ := os.ReadFile(marked); string(after) != string(before) {
		t.Fatalf("a unit with the marker was rewritten:\n%s", after)
	}
}

func TestOwnUnit(t *testing.T) {
	home := t.TempDir()
	uh, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	cases := []struct {
		name     string
		procHome string // this process's MIRRIN_HOME
		unit     map[string]string
		want     bool
	}{
		{"same home", home, map[string]string{"MIRRIN_HOME": home}, true},
		{"another profile's service", home, map[string]string{"MIRRIN_HOME": "/Users/someone-else/twin"}, false},
		{"an older unit, default home", "", map[string]string{"HOME": "/Users/me"}, true},
		{"an older unit, but this command uses another home", home, map[string]string{"HOME": "/Users/me"}, false},
		// A unit that spells out the default home is the default home's.
		{"a unit for the default home", "", map[string]string{"MIRRIN_HOME": filepath.Join(uh, ".mirrin")}, true},
		{"a unit for the default home, but this command uses another home", home, map[string]string{"MIRRIN_HOME": filepath.Join(uh, ".mirrin")}, false},
	}
	for _, c := range cases {
		t.Setenv("MIRRIN_HOME", c.procHome)
		if got := ownUnit(c.unit); got != c.want {
			t.Errorf("%s: ownUnit = %v", c.name, got)
		}
	}
}

func TestUnitEnvReadsSystemd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mirrin.service")
	writeFile(t, p, "[Service]\nExecStart=/usr/bin/mirrin run\nEnvironment=HOME=/home/me\nEnvironment=\"GEMINI_API_KEY=abc=def\"\n")
	env := unitEnv(p)
	if env["HOME"] != "/home/me" || env["GEMINI_API_KEY"] != "abc=def" {
		t.Fatalf("env = %v", env)
	}
}

func TestUnitPath(t *testing.T) {
	cases := []struct{ goos, want string }{
		{"darwin", "/h/Library/LaunchAgents/mirrin.plist"},
		{"linux", "/h/.config/systemd/user/mirrin.service"},
		{"windows", ""},
	}
	for _, c := range cases {
		if got := filepath.ToSlash(unitPath(c.goos, "/h", "mirrin")); got != c.want {
			t.Errorf("%s: %q, want %q", c.goos, got, c.want)
		}
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}
