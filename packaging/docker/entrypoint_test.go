// Package docker tests the hosted-twin image's scripts by running them with
// sh, without Docker.
package docker

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// TestMain lets the test binary stand in for wget where it isn't installed
// (macOS): run under the name wget, it does what healthcheck.sh asks of it.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "wget" {
		os.Exit(fakeWget(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeWget understands the flags healthcheck.sh and entrypoint.sh pass (-q
// -S -O file -T secs --header h URL) and exits like wget: 0 on success, 8 on
// an HTTP error. With -S it prints the status line on stderr, as wget does.
func fakeWget(args []string) int {
	var url string
	var show bool
	header := http.Header{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-q":
		case "-S":
			show = true
		case "-O", "-T":
			i++
		case "--header":
			i++
			if i < len(args) {
				if k, v, ok := strings.Cut(args[i], ":"); ok {
					header.Set(k, strings.TrimSpace(v))
				}
			}
		default:
			url = args[i]
		}
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 1
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 4
	}
	resp.Body.Close()
	if show {
		fmt.Fprintf(os.Stderr, "  HTTP/1.1 %s\n", resp.Status)
	}
	if resp.StatusCode >= 400 {
		return 8
	}
	return 0
}

func needSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
}

// entrypoint runs entrypoint.sh with a stand-in mirrin and returns its output.
func entrypoint(t *testing.T, home string, env ...string) (string, error) {
	t.Helper()
	return runEntrypoint(t, append([]string{"MIRRIN_HOME=" + home}, env...))
}

// runEntrypoint runs entrypoint.sh with a stand-in mirrin, env and no home
// variable of the test's own, and returns its output.
func runEntrypoint(t *testing.T, env []string) (string, error) {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "mirrin")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"started: $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "entrypoint.sh")
	cmd.Env = append(os.Environ(), "MIRRIN_HOME=", "MIRRIN_BIN="+fake,
		"TWIN_NAME=", "TWIN_PERSONA=", "OWNER_NAME=", "OWNER_HONORIFIC=", "OWNER_TIMEZONE=", "OWNER_ABOUT=",
		"LLM_PROVIDER=", "LLM_MODEL=", "TWIN_API_TOKEN=", "TWIN_GATEWAY=0", "TWIN_REPAIR_GATEWAY=", "TWIN_GATEWAY_TOKEN_FILE=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// imageEnv is what the image's ENV lines set, with its /data home moved to
// data (a test can't write /data).
func imageEnv(t *testing.T, data string) []string {
	t.Helper()
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == "ENV" {
			for _, kv := range f[1:] {
				env = append(env, strings.Replace(kv, "=/data", "="+data, 1))
			}
		}
	}
	return env
}

// A container keeps the home its deployment names with -e MIRRIN_HOME, and
// uses the image's /data otherwise. `docker exec mirrin`, which sees only
// the image's and the deployment's settings, finds the same home.
func TestImageKeepsTheDeploymentsHome(t *testing.T) {
	needSh(t)
	data, mine := t.TempDir(), t.TempDir()
	for _, c := range []struct {
		name   string
		deploy []string
		want   string
	}{
		{"the image's default", nil, data},
		{"-e MIRRIN_HOME", []string{"MIRRIN_HOME=" + mine}, mine},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, d := range []string{data, mine} {
				os.Remove(filepath.Join(d, "config.yaml"))
			}
			env := append(imageEnv(t, data), c.deploy...)
			if out, err := runEntrypoint(t, env); err != nil {
				t.Fatalf("entrypoint failed: %v\n%s", err, out)
			}
			for _, d := range []string{data, mine} {
				_, err := os.Stat(filepath.Join(d, "config.yaml"))
				if got := err == nil; got != (d == c.want) {
					t.Errorf("config in %s: %v, want the twin in %s", d, got, c.want)
				}
			}
			set := map[string]string{}
			for _, kv := range env {
				k, v, _ := strings.Cut(kv, "=")
				set[k] = v
			}
			if got := config.HomeEnv(func(k string) string { return set[k] }); got != c.want {
				t.Errorf("docker exec finds the twin in %q, want %q", got, c.want)
			}
		})
	}
}

func TestEntrypointWritesAConfigThatLoads(t *testing.T) {
	needSh(t)
	cases := []struct {
		name  string
		env   []string
		check func(*config.Config) string
	}{
		{"defaults", nil, func(c *config.Config) string {
			if c.Name != "Mirrin" || c.User.Name != "Owner" || c.User.Timezone != "UTC" || c.LLM.Provider != "openai" {
				return "defaults changed"
			}
			return ""
		}},
		// Crashed first boot: "mapping values are not allowed in this context".
		{"colon in the bio", []string{"OWNER_ABOUT=Engineer: loves tea"}, func(c *config.Config) string {
			return want(c.User.About, "Engineer: loves tea")
		}},
		// Was silently cut to "Founder".
		{"hash in the bio", []string{"OWNER_ABOUT=Founder #1"}, func(c *config.Config) string {
			return want(c.User.About, "Founder #1")
		}},
		{"quotes, backslash and a line break", []string{"OWNER_ABOUT=Says \"hi\" \\o/\nand 'bye'"}, func(c *config.Config) string {
			return want(c.User.About, "Says \"hi\" \\o/\nand 'bye'")
		}},
		{"names that look like YAML", []string{"TWIN_NAME=- [yes]", "OWNER_NAME=null", "OWNER_HONORIFIC=Dr. #2", "OWNER_TIMEZONE=America/New_York"},
			func(c *config.Config) string {
				return want(c.Name+"|"+c.User.Name+"|"+c.User.Honorific+"|"+c.User.Timezone, "- [yes]|null|Dr. #2|America/New_York")
			}},
		{"provider and model", []string{"LLM_PROVIDER=anthropic", "LLM_MODEL=claude-sonnet-5"}, func(c *config.Config) string {
			return want(c.LLM.Provider+"/"+c.LLM.Model, "anthropic/claude-sonnet-5")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			out, err := entrypoint(t, home, c.env...)
			if err != nil || !strings.Contains(out, "started: run") {
				t.Fatalf("entrypoint failed: %v\n%s", err, out)
			}
			t.Setenv("MIRRIN_HOME", home)
			// Loading saves a zone that is the machine's own as "Local"; on a
			// runner set to UTC, "UTC" read back as "Local". The image's zone
			// is what is checked here, not this machine's.
			t.Setenv("TZ", "Australia/Sydney")
			cfg, err := config.Load()
			if err != nil {
				b, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
				t.Fatalf("config doesn't load: %v\n%s", err, b)
			}
			if msg := c.check(cfg); msg != "" {
				t.Error(msg)
			}
			if cfg.DataDir != filepath.Join(home, "data") {
				t.Errorf("data_dir = %q", cfg.DataDir)
			}
		})
	}
}

func want(got, want string) string {
	if got != want {
		return "got " + quote(got) + ", want " + quote(want)
	}
	return ""
}

func quote(s string) string { return "\"" + strings.ReplaceAll(s, "\n", `\n`) + "\"" }

// wgetPath is a PATH with wget on it: the real one, or this test binary
// standing in for it.
func wgetPath(t *testing.T) string {
	t.Helper()
	path := os.Getenv("PATH")
	if _, err := exec.LookPath("wget"); err == nil {
		return path
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(self, filepath.Join(dir, "wget")); err != nil {
		t.Skip("no wget, and no symlink to stand in for it")
	}
	return dir + string(os.PathListSeparator) + path
}

// fakeTwin is the twin's API as the gateway pairing sees it: /healthz for
// anyone, /status for a key it knows.
type fakeTwin struct {
	srv  *httptest.Server
	port string
	mu   sync.Mutex
	keys map[string]bool
	down int // when set, /status answers this instead
}

func newFakeTwin(t *testing.T) *fakeTwin {
	f := &fakeTwin{keys: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte("ok\n"))
		case "/status":
			f.mu.Lock()
			ok := f.keys[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			down := f.down
			f.mu.Unlock()
			if down != 0 {
				http.Error(w, "busy", down)
				return
			}
			if !ok {
				http.Error(w, "not paired", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	_, f.port, _ = net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "http://"))
	return f
}

func (f *fakeTwin) set(key string, ok bool) { f.mu.Lock(); f.keys[key] = ok; f.mu.Unlock() }

// gatewayBoot runs entrypoint.sh with a stand-in mirrin whose pair prints a
// code and whose connect saves a key (key), as the real ones do; it returns
// the output and how many codes were made.
func gatewayBoot(t *testing.T, home string, twin *fakeTwin, key string, env ...string) (string, int) {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	script := `#!/bin/sh
case "$1" in
run)
  # like the daemon: runs until the gateway is sorted out
  i=0
  while [ $i -lt 100 ] && [ ! -f "$GW/status" ]; do i=$((i+1)); sleep 0.1; done
  echo "started: run" ;;
pair) echo pair >>"` + log + `"; printf 'On the other computer, run:\n\n  mirrin connect ab2.CODE\n\n' ;;
connect)
  [ "$2" = --name ] && [ "$3" = gateway ] && [ "$4" = ab2.CODE ] || { echo "bad connect: $*" >&2; exit 1; }
  printf 'address: http://127.0.0.1:7742\ntoken: ` + key + `\nname: Mirrin\n' >"$MIRRIN_HOME/remote.yaml" ;;
esac
`
	fake := filepath.Join(bin, "mirrin")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "entrypoint.sh")
	cmd.Env = append(os.Environ(), "PATH="+wgetPath(t), "MIRRIN_HOME="+home, "MIRRIN_BIN="+fake, "MIRRIN_PORT="+twin.port, "GW="+filepath.Join(home, "gateway"),
		"TWIN_NAME=", "TWIN_API_TOKEN=", "TWIN_GATEWAY=", "TWIN_REPAIR_GATEWAY=", "TWIN_GATEWAY_TOKEN_FILE=", "TWIN_START_WAIT=10")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(log)
	return string(out), strings.Count(string(b), "pair")
}

// The gateway gets a device key of its own from the entrypoint, with no
// TWIN_API_TOKEN, and the entrypoint never writes the twin's master key.
func TestEntrypointPairsTheGatewayOnItsOwnKey(t *testing.T) {
	needSh(t)
	twin := newFakeTwin(t)
	home := t.TempDir()
	out, pairs := gatewayBoot(t, home, twin, "dev_first")
	if !strings.Contains(out, "started: run") || pairs != 1 {
		t.Fatalf("pairs %d\n%s", pairs, out)
	}
	p := filepath.Join(home, "gateway", "token")
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "dev_first" {
		t.Fatalf("gateway key = %q, %v\n%s", b, err, out)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("gateway key mode %v, want 0600", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(home, "data", "api.token")); !os.IsNotExist(err) {
		t.Fatalf("the entrypoint wrote the master key: %v", err)
	}
	// The gateway's key never lands in the twin's own home: that would make
	// the twin a client of itself.
	if _, err := os.Stat(filepath.Join(home, "remote.yaml")); !os.IsNotExist(err) {
		t.Fatalf("the gateway's key went into the twin's home: %v", err)
	}

	// A key that still works is kept: no new pairing.
	twin.set("dev_first", true)
	if _, pairs := gatewayBoot(t, home, twin, "dev_second"); pairs != 0 {
		t.Fatalf("paired again with a working key (%d)", pairs)
	}
	if b, _ := os.ReadFile(p); string(b) != "dev_first" {
		t.Fatalf("gateway key = %q", b)
	}

	// TWIN_API_TOKEN, if still set, seeds nothing.
	out, _ = gatewayBoot(t, t.TempDir(), twin, "dev_x", "TWIN_API_TOKEN="+strings.Repeat("ab", 24))
	if !strings.Contains(out, "no longer used") {
		t.Fatalf("no word about TWIN_API_TOKEN:\n%s", out)
	}
}

// A revoked gateway key doesn't linger: it is removed, and the gateway isn't
// quietly paired again (that would undo the revocation) until asked.
func TestEntrypointRemovesARevokedGatewayKey(t *testing.T) {
	needSh(t)
	twin := newFakeTwin(t)
	home := t.TempDir()
	p := filepath.Join(home, "gateway", "token")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("dev_revoked"), 0o600); err != nil {
		t.Fatal(err)
	}
	twin.set("dev_revoked", false)
	out, pairs := gatewayBoot(t, home, twin, "dev_new")
	if _, err := os.Stat(p); !os.IsNotExist(err) || pairs != 0 || !strings.Contains(out, "revoked") {
		t.Fatalf("revoked key: %v, pairs %d\n%s", err, pairs, out)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "gateway", "status")); strings.TrimSpace(string(b)) != "revoked" {
		t.Fatalf("status %q", b)
	}
	if _, pairs := gatewayBoot(t, home, twin, "dev_new"); pairs != 0 {
		t.Fatal("a restart paired the revoked gateway again")
	}
	if _, pairs := gatewayBoot(t, home, twin, "dev_new", "TWIN_REPAIR_GATEWAY=1"); pairs != 1 {
		t.Fatal("TWIN_REPAIR_GATEWAY=1 didn't pair it")
	}
	if b, _ := os.ReadFile(p); string(b) != "dev_new" {
		t.Fatalf("gateway key = %q", b)
	}
	if _, err := os.Stat(filepath.Join(home, "gateway", "revoked")); !os.IsNotExist(err) {
		t.Fatal("still marked revoked")
	}
}

// A twin that answers /status with a 5xx or 429 hasn't refused the key:
// the gateway keeps it and isn't marked revoked.
func TestEntrypointKeepsTheGatewayKeyWhenTheTwinIsBusy(t *testing.T) {
	needSh(t)
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusTooManyRequests} {
		twin := newFakeTwin(t)
		twin.set("dev_ok", true)
		twin.down = code
		home := t.TempDir()
		p := filepath.Join(home, "gateway", "token")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("dev_ok"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, pairs := gatewayBoot(t, home, twin, "dev_new")
		if b, _ := os.ReadFile(p); string(b) != "dev_ok" || pairs != 0 {
			t.Fatalf("%d: gateway key = %q, pairs %d\n%s", code, b, pairs, out)
		}
		if _, err := os.Stat(filepath.Join(home, "gateway", "revoked")); !os.IsNotExist(err) {
			t.Fatalf("%d: marked revoked", code)
		}
		if b, _ := os.ReadFile(filepath.Join(home, "gateway", "status")); strings.TrimSpace(string(b)) != "kept" {
			t.Fatalf("%d: status %q", code, b)
		}
	}
}

// The healthcheck asks the public /healthz and holds no key.
func TestHealthcheckNeedsNoKey(t *testing.T) {
	needSh(t)
	twin := newFakeTwin(t)
	run := func(port string) error {
		cmd := exec.Command("sh", "healthcheck.sh")
		cmd.Env = append(os.Environ(), "PATH="+wgetPath(t), "MIRRIN_HOME="+t.TempDir(), "MIRRIN_PORT="+port)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("healthcheck: %s", out)
		}
		return err
	}
	if err := run(twin.port); err != nil {
		t.Errorf("unhealthy with a running twin and no key on disk: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closed, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	if err := run(closed); err == nil {
		t.Error("healthy with no twin running")
	}
	b, err := os.ReadFile("healthcheck.sh")
	if err != nil || strings.Contains(string(b), "api.token") || strings.Contains(string(b), "Authorization") {
		t.Fatal("the healthcheck still reads the master key")
	}
}

// The image's HEALTHCHECK has to go through healthcheck.sh: a bare
// `wget /health` has no token and is refused, so the container never turned
// healthy.
func TestDockerfileUsesTheHealthcheck(t *testing.T) {
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var check, copied bool
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "HEALTHCHECK":
			check = strings.Contains(line, "healthcheck.sh")
		case "COPY":
			copied = copied || strings.Contains(line, "packaging/docker/healthcheck.sh")
		}
	}
	if !check || !copied {
		t.Fatalf("HEALTHCHECK runs healthcheck.sh: %v; the image copies it: %v", check, copied)
	}
}
