package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/homelock"
)

// tempUserHome points the user's home at a temporary folder, clears every
// home variable and lets Home() move a home from before the rename, until
// the test ends. Nothing outside the temporary folder is looked at.
func tempUserHome(t *testing.T) string {
	t.Helper()
	user := t.TempDir()
	prev := userHomeDir
	userHomeDir = func() (string, error) { return user, nil }
	for _, k := range homeVars {
		t.Setenv(k, "")
	}
	migrateInTests = true
	resetHome()
	t.Cleanup(func() {
		userHomeDir = prev
		migrateInTests = false
		resetHome()
	})
	return user
}

// resetHome forgets the home this process settled on.
func resetHome() {
	homeMemo.Lock()
	homeMemo.key, homeMemo.dir = "", ""
	homeMemo.Unlock()
}

// makeTwin puts a small twin in dir: settings, a key and its data folder.
func makeTwin(t *testing.T, dir, config string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"config.yaml": config, "secrets.env": "ANTBOT_EMAIL_PASSWORD=\"app pass\"\n", filepath.Join("data", "memory.db"): "memory"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Under `go test` a legacy home stays where it is: a test that forgets its
// own MIRRIN_HOME would otherwise move the contributor's real twin. Only a
// test that opts in moves one.
func TestMigrateHomeIsOffUnderTests(t *testing.T) {
	root := t.TempDir()
	old, dir := filepath.Join(root, ".antbot"), filepath.Join(root, ".mirrin")
	makeTwin(t, old, "name: Ava\n")

	if got := migrateHome([]string{old}, dir); got != dir {
		t.Fatalf("migrateHome under go test = %q, want the new home", got)
	}
	if _, err := os.Stat(filepath.Join(old, "data")); err != nil {
		t.Fatalf("the legacy home moved under go test: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the new home appeared under go test: %v", err)
	}

	migrateInTests = true
	t.Cleanup(func() { migrateInTests = false })
	if got := migrateHome([]string{old}, dir); got != dir {
		t.Fatalf("migrateHome = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "memory.db")); err != nil {
		t.Fatalf("an opted-in test didn't move the legacy home: %v", err)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("the legacy home (or a link to the new one) is still there: %v", err)
	}
}

// AntBot's home moves to ~/.mirrin on first use, and the paths in its
// config.yaml follow it: absolute, ~/ and ~\ ones. Paths that only look
// alike, and the names of variables, stay as they are.
func TestHomeMovesAntbot(t *testing.T) {
	user := tempUserHome(t)
	for _, k := range []string{"MIRRIN_EMAIL_PASSWORD", "ANTBOT_EMAIL_PASSWORD", "OPENHUMAN_EMAIL_PASSWORD"} {
		t.Setenv(k, "")
	}
	old := filepath.Join(user, ".antbot")
	makeTwin(t, old, strings.Join([]string{
		"# Mirrin settings.",
		"name: Ava",
		"data_dir: " + filepath.Join(old, "data"),
		"channels:",
		"  voice:",
		"    whisper_model: ~/.antbot/models/ggml-base.en.bin",
		"    kokoro_dir: ~\\.antbot\\tts",
		"skills:",
		"  email:",
		"    password_env: ANTBOT_EMAIL_PASSWORD",
		"  system:",
		"    allowed_dirs:",
		"      - ~/.antbot-old/x",
		"      - /Volumes/NAS/antbot-backups",
		"mcp:",
		"  servers:",
		"    - name: notes",
		"      env:",
		"        TOKEN: $ANTBOT_FOO",
		"",
	}, "\n"))

	dir := filepath.Join(user, ".mirrin")
	if got := Home(); got != dir {
		t.Fatalf("Home() = %q, want %q", got, dir)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("~/.antbot is still there (no compatibility link is made): %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "data", "memory.db")); got != "memory" {
		t.Fatalf("the data didn't move: %q", got)
	}
	cfg := readFile(t, filepath.Join(dir, "config.yaml"))
	for _, want := range []string{
		"data_dir: " + filepath.Join(dir, "data"),
		"whisper_model: ~/.mirrin/models/ggml-base.en.bin",
		"kokoro_dir: ~\\.mirrin\\tts",
		"~/.antbot-old/x",
		"/Volumes/NAS/antbot-backups",
		"password_env: ANTBOT_EMAIL_PASSWORD",
		"TOKEN: $ANTBOT_FOO",
		"# Mirrin settings.",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config.yaml lacks %q:\n%s", want, cfg)
		}
	}
	if st, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Fatalf("config.yaml mode: %v %v", st, err)
	}
	// The key saved under AntBot's name still answers to the new one.
	if got := Secret("MIRRIN_EMAIL_PASSWORD"); got != "app pass" {
		t.Fatalf("Secret(MIRRIN_EMAIL_PASSWORD) = %q", got)
	}
}

// A home from before AntBot moves straight to ~/.mirrin, and its
// OPENHUMAN_ variable names keep resolving without being rewritten.
func TestHomeMovesOpenhumanStraight(t *testing.T) {
	user := tempUserHome(t)
	for _, k := range []string{"MIRRIN_EMAIL_PASSWORD", "ANTBOT_EMAIL_PASSWORD", "OPENHUMAN_EMAIL_PASSWORD"} {
		t.Setenv(k, "")
	}
	old := filepath.Join(user, ".openhuman")
	makeTwin(t, old, "skills:\n  email:\n    password_env: OPENHUMAN_EMAIL_PASSWORD\n  calendar:\n    token_file: "+filepath.Join(old, "google-token.json")+"\n")
	if err := os.WriteFile(filepath.Join(old, "secrets.env"), []byte("MIRRIN_EMAIL_PASSWORD=\"moved\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(user, ".mirrin")
	if got := Home(); got != dir {
		t.Fatalf("Home() = %q", got)
	}
	cfg := readFile(t, filepath.Join(dir, "config.yaml"))
	if !strings.Contains(cfg, "token_file: "+filepath.Join(dir, "google-token.json")) || !strings.Contains(cfg, "password_env: OPENHUMAN_EMAIL_PASSWORD") {
		t.Fatalf("config.yaml:\n%s", cfg)
	}
	if got := Secret("OPENHUMAN_EMAIL_PASSWORD"); got != "moved" {
		t.Fatalf("Secret(OPENHUMAN_EMAIL_PASSWORD) = %q", got)
	}
}

// With both, AntBot's home is the one that moves; openHuman's stays.
func TestHomePrefersAntbotOverOpenhuman(t *testing.T) {
	user := tempUserHome(t)
	makeTwin(t, filepath.Join(user, ".openhuman"), "name: older\n")
	makeTwin(t, filepath.Join(user, ".antbot"), "name: newer\n")
	dir := filepath.Join(user, ".mirrin")
	if got := Home(); got != dir {
		t.Fatalf("Home() = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "config.yaml")); got != "name: newer\n" {
		t.Fatalf("moved the wrong home: %q", got)
	}
	if _, err := os.Stat(filepath.Join(user, ".openhuman", "config.yaml")); err != nil {
		t.Fatalf("openHuman's home was touched: %v", err)
	}
}

// Every service AntBot installed names ANTBOT_HOME=~/.antbot. That, and a
// MIRRIN_HOME naming the default, still mean the default home, which moves.
func TestHomeLegacyDefaultEnvStillMigrates(t *testing.T) {
	for _, c := range []struct{ name, value string }{
		{"ANTBOT_HOME", ".antbot"},
		{"MIRRIN_HOME", ".mirrin"},
		{"OPENHUMAN_HOME", ".openhuman"},
	} {
		t.Run(c.name, func(t *testing.T) {
			user := tempUserHome(t)
			makeTwin(t, filepath.Join(user, ".antbot"), "name: Ava\n")
			t.Setenv(c.name, filepath.Join(user, c.value)+string(filepath.Separator))
			dir := filepath.Join(user, ".mirrin")
			if got := Home(); got != dir {
				t.Fatalf("Home() = %q, want %q", got, dir)
			}
			if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
				t.Fatalf("not moved: %v", err)
			}
		})
	}
}

// MIRRIN_HOME names the home, then AntBot's ANTBOT_HOME, then openHuman's
// OPENHUMAN_HOME; with none, it is ~/.mirrin.
func TestHomePrecedence(t *testing.T) {
	user := tempUserHome(t)
	mirrin, antbot, openhuman := t.TempDir(), t.TempDir(), t.TempDir()
	for _, c := range []struct {
		name                      string
		mirrin, antbot, openhuman string
		want                      string
	}{
		{"all three", mirrin, antbot, openhuman, mirrin},
		{"the current name only", mirrin, "", "", mirrin},
		{"AntBot's over openHuman's", "", antbot, openhuman, antbot},
		{"AntBot's only", "", antbot, "", antbot},
		{"openHuman's only", "", "", openhuman, openhuman},
		{"none", "", "", "", filepath.Join(user, ".mirrin")},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MIRRIN_HOME", c.mirrin)
			t.Setenv("ANTBOT_HOME", c.antbot)
			t.Setenv("OPENHUMAN_HOME", c.openhuman)
			if got := Home(); got != c.want {
				t.Errorf("Home() = %q, want %q", got, c.want)
			}
			if got := CurrentHome(); got != c.want {
				t.Errorf("CurrentHome() = %q, want %q", got, c.want)
			}
		})
	}
}

// A home named anywhere else is used as it is, the current name first.
func TestHomeCustomEnvHonoured(t *testing.T) {
	user := tempUserHome(t)
	makeTwin(t, filepath.Join(user, ".antbot"), "name: Ava\n")
	custom := filepath.Join(t.TempDir(), "twin")
	t.Setenv("OPENHUMAN_HOME", filepath.Join(t.TempDir(), "older"))
	t.Setenv("ANTBOT_HOME", custom)
	if got := Home(); got != custom {
		t.Fatalf("Home() = %q, want ANTBOT_HOME's %q", got, custom)
	}
	// A MIRRIN_HOME naming the default, as the setup program writes it,
	// doesn't hide a home AntBot was told to use.
	t.Setenv("MIRRIN_HOME", filepath.Join(user, ".mirrin"))
	if got := Home(); got != custom {
		t.Fatalf("Home() = %q, want ANTBOT_HOME's %q", got, custom)
	}
	mine := filepath.Join(t.TempDir(), "mine")
	t.Setenv("MIRRIN_HOME", mine)
	if got := Home(); got != mine {
		t.Fatalf("Home() = %q, want MIRRIN_HOME's %q", got, mine)
	}
	if _, err := os.Stat(filepath.Join(user, ".antbot", "config.yaml")); err != nil {
		t.Fatal("a named home moved the default one")
	}
}

// While a twin runs from the old home (it holds the claim on its data
// folder), the old home stays and is used, for the rest of the process.
// An AntBot twin's claim is data/antbot.lock, the file it locks.
func TestHomeDeferredWhileLocked(t *testing.T) {
	user := tempUserHome(t)
	old := filepath.Join(user, ".antbot")
	makeTwin(t, old, "name: Ava\n")
	f, err := homelock.Lock(filepath.Join(old, "data", "antbot.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Home(); got != old {
		t.Fatalf("Home() = %q while the old twin runs, want %q", got, old)
	}
	if _, err := os.Stat(filepath.Join(user, ".mirrin")); !os.IsNotExist(err) {
		t.Fatalf("something moved under a running twin: %v", err)
	}
	f.Close()
	if got := Home(); got != old {
		t.Fatalf("Home() switched to %q halfway through a process", got)
	}
	if got := CurrentHome(); got != old {
		t.Fatalf("CurrentHome() = %q", got)
	}
	resetHome() // the next process
	if got := Home(); got != filepath.Join(user, ".mirrin") {
		t.Fatalf("Home() = %q once the old twin quit", got)
	}
}

// A data folder elsewhere (data_dir) holds the claim there.
func TestHomeDeferredWhileLockedElsewhere(t *testing.T) {
	user := tempUserHome(t)
	old := filepath.Join(user, ".antbot")
	data := filepath.Join(t.TempDir(), "data")
	makeTwin(t, old, "data_dir: "+data+"\n")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := homelock.Lock(homelock.Path(data))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := Home(); got != old {
		t.Fatalf("Home() = %q, want %q", got, old)
	}
}

// When the move fails, the old home is used rather than an empty new one.
func TestHomeRenameFailureKeepsOld(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder this user can't write to")
	}
	user := tempUserHome(t)
	old := filepath.Join(user, ".antbot")
	makeTwin(t, old, "name: Ava\n")
	if err := os.Chmod(user, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(user, 0o700) })
	if got := Home(); got != old {
		t.Fatalf("Home() = %q after a failed move, want %q", got, old)
	}
	if _, err := os.Stat(filepath.Join(old, "config.yaml")); err != nil {
		t.Fatal(err)
	}
}

// An empty ~/.mirrin (a log folder made early, say) doesn't stop the move;
// one holding a file does, and the old home is used.
func TestHomeEmptyMirrinDoesNotBlock(t *testing.T) {
	user := tempUserHome(t)
	makeTwin(t, filepath.Join(user, ".antbot"), "name: Ava\n")
	dir := filepath.Join(user, ".mirrin")
	if err := os.MkdirAll(filepath.Join(dir, "logs", "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := Home(); got != dir {
		t.Fatalf("Home() = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "config.yaml")); got != "name: Ava\n" {
		t.Fatalf("config.yaml = %q", got)
	}

	user = tempUserHome(t)
	old := filepath.Join(user, ".antbot")
	makeTwin(t, old, "name: Ava\n")
	dir = filepath.Join(user, ".mirrin")
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "stray.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Home(); got != old {
		t.Fatalf("Home() = %q with something in the way, want %q", got, old)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "stray.log")); err != nil {
		t.Fatal("the stray file was removed")
	}
}

// A ~/.mirrin that already holds a twin wins over a leftover ~/.antbot.
func TestHomeExistingMirrinWins(t *testing.T) {
	user := tempUserHome(t)
	makeTwin(t, filepath.Join(user, ".antbot"), "name: old\n")
	makeTwin(t, filepath.Join(user, ".mirrin"), "name: new\n")
	if got := Home(); got != filepath.Join(user, ".mirrin") {
		t.Fatalf("Home() = %q", got)
	}
	if got := readFile(t, filepath.Join(user, ".antbot", "config.yaml")); got != "name: old\n" {
		t.Fatalf("the leftover was touched: %q", got)
	}
}

// Home settles once per environment, and follows a changed one.
func TestHomeMemoized(t *testing.T) {
	user := tempUserHome(t)
	dir := filepath.Join(user, ".mirrin")
	if got := Home(); got != dir {
		t.Fatalf("Home() = %q", got)
	}
	// A twin appearing in the old place later doesn't move mid-process.
	makeTwin(t, filepath.Join(user, ".antbot"), "name: Ava\n")
	if got := Home(); got != dir {
		t.Fatalf("Home() = %q", got)
	}
	if _, err := os.Stat(filepath.Join(user, ".antbot", "config.yaml")); err != nil {
		t.Fatal("moved mid-process")
	}
	other := t.TempDir()
	t.Setenv("MIRRIN_HOME", other)
	if got := Home(); got != other {
		t.Fatalf("Home() = %q after MIRRIN_HOME changed", got)
	}
}

// A key is found under any of its names: in the environment first, then
// in the secrets file.
func TestSecretAliases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	for _, k := range []string{"MIRRIN_EMAIL_PASSWORD", "ANTBOT_EMAIL_PASSWORD", "OPENHUMAN_EMAIL_PASSWORD", "MIRRIN_WHISPER_URL", "ANTBOT_WHISPER_URL", "OPENHUMAN_WHISPER_URL"} {
		t.Setenv(k, "")
	}
	if err := SaveSecrets(map[string]string{"ANTBOT_EMAIL_PASSWORD": "saved", "OPENHUMAN_WHISPER_URL": "http://old"}); err != nil {
		t.Fatal(err)
	}
	if got := Secret("MIRRIN_EMAIL_PASSWORD"); got != "saved" {
		t.Fatalf("saved under AntBot's name: %q", got)
	}
	if got := Secret("MIRRIN_WHISPER_URL"); got != "http://old" {
		t.Fatalf("saved under openHuman's name: %q", got)
	}
	t.Setenv("ANTBOT_EMAIL_PASSWORD", "exported")
	if got := Secret("MIRRIN_EMAIL_PASSWORD"); got != "exported" {
		t.Fatalf("an exported key under the old name didn't win over the file: %q", got)
	}
	if name, v := Exported("MIRRIN_EMAIL_PASSWORD"); name != "ANTBOT_EMAIL_PASSWORD" || v != "exported" {
		t.Fatalf("Exported = %q %q", name, v)
	}
	if err := SaveSecrets(map[string]string{"MIRRIN_EMAIL_PASSWORD": "new"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTBOT_EMAIL_PASSWORD", "")
	if got := Secret("MIRRIN_EMAIL_PASSWORD"); got != "new" {
		t.Fatalf("the current name in the file comes first: %q", got)
	}
	if got := Secret("OPENAI_API_KEY_NOT_OURS"); got != "" {
		t.Fatalf("another program's variable: %q", got)
	}
}

// The default mailbox and bucket key names are the new ones; keys saved by
// AntBot under the old ones still work.
func TestS3AndEmailDefaultsReadLegacyKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	for _, k := range []string{"MIRRIN_EMAIL_PASSWORD", "ANTBOT_EMAIL_PASSWORD", "MIRRIN_S3_ACCESS_KEY_ID", "ANTBOT_S3_ACCESS_KEY_ID", "MIRRIN_S3_SECRET_ACCESS_KEY", "ANTBOT_S3_SECRET_ACCESS_KEY"} {
		t.Setenv(k, "")
	}
	if err := SaveSecrets(map[string]string{"ANTBOT_EMAIL_PASSWORD": "mail", "ANTBOT_S3_ACCESS_KEY_ID": "AKIA", "ANTBOT_S3_SECRET_ACCESS_KEY": "shh"}); err != nil {
		t.Fatal(err)
	}
	c := Default()
	if c.Skills.Email.PasswordEnv != "MIRRIN_EMAIL_PASSWORD" || c.EmailPassword() != "mail" {
		t.Fatalf("email: %q %q", c.Skills.Email.PasswordEnv, c.EmailPassword())
	}
	access, secret := BackupS3{}.KeyEnvs()
	if access != "MIRRIN_S3_ACCESS_KEY_ID" || Secret(access) != "AKIA" || Secret(secret) != "shh" {
		t.Fatalf("s3: %q %q %q", access, Secret(access), Secret(secret))
	}
}

func TestServiceMarkers(t *testing.T) {
	for _, k := range []string{"MIRRIN_SERVICE", "ANTBOT_SERVICE", "OPENHUMAN_SERVICE", "XPC_SERVICE_NAME"} {
		t.Setenv(k, "")
	}
	if UnderServiceManager() {
		t.Fatal("nothing set")
	}
	for _, c := range [][2]string{{"MIRRIN_SERVICE", "1"}, {"ANTBOT_SERVICE", "1"}, {"XPC_SERVICE_NAME", "mirrin"}, {"XPC_SERVICE_NAME", "antbot"}} {
		t.Setenv(c[0], c[1])
		if !UnderServiceManager() {
			t.Errorf("%s=%s isn't the service manager", c[0], c[1])
		}
		t.Setenv(c[0], "")
	}
	t.Setenv("XPC_SERVICE_NAME", "application.com.apple.Terminal.1")
	if UnderServiceManager() {
		t.Fatal("a terminal's label")
	}
	t.Setenv("ANTBOT_SERVICE", "1")
	if !ServiceMarked() {
		t.Fatal("AntBot's marker")
	}
}

func TestDataDirIn(t *testing.T) {
	home := t.TempDir()
	if got := DataDirIn(home); got != filepath.Join(home, "data") {
		t.Fatalf("no config: %q", got)
	}
	elsewhere := filepath.Join(t.TempDir(), "d")
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("data_dir: "+elsewhere+"\nllm: [not, a, map]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DataDirIn(home); got != elsewhere {
		t.Fatalf("data_dir: %q", got)
	}
}

// An empty lock file in ~/.mirrin (config.yaml.lock, left by an edit made
// while the twin was still in ~/.antbot) doesn't stop the move either.
func TestHomeEmptyLockDoesNotBlock(t *testing.T) {
	user := tempUserHome(t)
	makeTwin(t, filepath.Join(user, ".antbot"), "name: Ava\n")
	dir := filepath.Join(user, ".mirrin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Home(); got != dir {
		t.Fatalf("Home() = %q with an empty lock file in the way", got)
	}
	if got := readFile(t, filepath.Join(dir, "config.yaml")); got != "name: Ava\n" {
		t.Fatalf("config.yaml = %q", got)
	}
}

// While an old background service would still start from the old home
// (HoldHomeMove names it), the home stays; once it's gone, it moves.
func TestHomeHeldByAnOldService(t *testing.T) {
	user := tempUserHome(t)
	old := filepath.Join(user, ".antbot")
	makeTwin(t, old, "name: Ava\n")
	hold := "The old AntBot background service"
	HoldHomeMove = func() string { return hold }
	t.Cleanup(func() { HoldHomeMove = nil })
	if got := Home(); got != old {
		t.Fatalf("Home() = %q while the old service is installed, want %q", got, old)
	}
	if _, err := os.Stat(filepath.Join(user, ".mirrin")); !os.IsNotExist(err) {
		t.Fatalf("moved under the old service: %v", err)
	}
	hold = ""
	resetHome() // the next process
	if got := Home(); got != filepath.Join(user, ".mirrin") {
		t.Fatalf("Home() = %q once the old service was removed", got)
	}
}

// A path in config.yaml that would still lead into the old home after the
// move (one the prefixes don't catch) stops the move: the twin would
// otherwise start with an empty data folder there. Nothing changes.
func TestHomeMoveStopsWhenAPathWouldStayBehind(t *testing.T) {
	user := tempUserHome(t)
	old := filepath.Join(user, ".antbot")
	body := "name: Ava\ndata_dir: ~/./.antbot/data\nchannels:\n  voice:\n    whisper_model: ~/.antbot/models/x.bin\n"
	makeTwin(t, old, body)
	if got := Home(); got != old {
		t.Fatalf("Home() = %q, want the old home kept", got)
	}
	if got := readFile(t, filepath.Join(old, "config.yaml")); got != body {
		t.Fatalf("config.yaml changed:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(user, ".mirrin")); !os.IsNotExist(err) {
		t.Fatalf("the new home appeared: %v", err)
	}
}

// Where paths ignore case (Windows, macOS) and Windows takes / for \, a
// path written either way still follows the move.
func TestRebasePathsFoldsLikeTheDisk(t *testing.T) {
	defer func(c, s bool) { foldCase, foldSlashes = c, s }(foldCase, foldSlashes)
	foldCase, foldSlashes = true, true
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("data_dir: c:/users/ME/.AntBot/data\nother: C:\\Users\\me\\.antbot-old\\x\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if !RebasePaths(&doc, `C:\Users\me\.antbot`, `C:\Users\me\.mirrin`) {
		t.Fatal("nothing changed")
	}
	var c struct {
		DataDir string `yaml:"data_dir"`
		Other   string `yaml:"other"`
	}
	if err := doc.Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.DataDir != `C:\Users\me\.mirrin\data` || c.Other != `C:\Users\me\.antbot-old\x` {
		t.Fatalf("after: %+v", c)
	}

	foldCase, foldSlashes = false, false
	if _, ok := underPath("/home/me/.AntBot/data", "/home/me/.antbot"); ok {
		t.Fatal("case counts on a disk where it does")
	}
}

// A config.yaml that links to a copy outside the home (dotfiles) stays a
// link, and the copy's paths follow the move; one that links into the old
// home by its full path would dangle after it, so the home stays.
func TestHomeMoveWritesThroughALinkedConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symbolic links")
	}
	user := tempUserHome(t)
	old := filepath.Join(user, ".antbot")
	makeTwin(t, old, "")
	dotfiles := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(dotfiles, []byte("name: Ava\ndata_dir: ~/.antbot/data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(old, "config.yaml")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dotfiles, link); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(user, ".mirrin")
	if got := Home(); got != dir {
		t.Fatalf("Home() = %q", got)
	}
	if st, err := os.Lstat(filepath.Join(dir, "config.yaml")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config.yaml isn't a link any more: %v %v", st, err)
	}
	if got := readFile(t, dotfiles); !strings.Contains(got, "data_dir: ~/.mirrin/data") {
		t.Fatalf("the linked copy kept the old paths:\n%s", got)
	}

	user = tempUserHome(t)
	old = filepath.Join(user, ".antbot")
	makeTwin(t, old, "")
	real := filepath.Join(old, "settings.yaml")
	if err := os.WriteFile(real, []byte("name: Ava\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(old, "config.yaml")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got := Home(); got != old {
		t.Fatalf("Home() = %q with config.yaml linked into the old home by its full path", got)
	}
}
