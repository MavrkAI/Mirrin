package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const (
	oldTag = "v9.9.8"
	newTag = "v9.9.9"
)

// fakeRelease serves a GitHub-shaped release of newTag: /releases/latest
// redirects to its tag page, and /releases/download/<tag>/<file> serves
// files. SHA256SUMS lists every file unless sums is set ("-" leaves it out).
// codes makes a path answer with that status instead.
type fakeRelease struct {
	files map[string][]byte
	sums  string
	codes map[string]int
	mu    sync.Mutex
	got   []string // paths asked for
}

func (f *fakeRelease) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeRelease) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.got = append(f.got, r.URL.Path)
		f.mu.Unlock()
		if code := f.codes[r.URL.Path]; code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		switch {
		case r.URL.Path == "/releases/latest":
			http.Redirect(w, r, "/releases/tag/"+newTag, http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/releases/download/"+newTag+"/"):
			name := strings.TrimPrefix(r.URL.Path, "/releases/download/"+newTag+"/")
			if name == "SHA256SUMS" && f.sums != "-" {
				if f.sums != "" {
					io.WriteString(w, f.sums)
					return
				}
				for n, b := range f.files {
					fmt.Fprintf(w, "%s  %s\n", hexSum(b), n)
				}
				return
			}
			if b, ok := f.files[name]; ok {
				w.Write(b)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeRelease) asked(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.got {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func hexSum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fakeTwin is the background service and the running twin.
type fakeTwin struct {
	installed, running bool
	version            string // what the running twin reports
	restartTo          string // the version it runs after a restart ("" keeps it)
	restartErr         error
	restarts           int
}

func (f *fakeTwin) Installed() bool { return f.installed }
func (f *fakeTwin) Restart(io.Writer) error {
	f.restarts++
	if f.restartErr != nil {
		return f.restartErr
	}
	if f.restartTo != "" {
		f.version, f.running = f.restartTo, true
	}
	return nil
}
func (f *fakeTwin) RunningVersion() (string, bool) { return f.version, f.running }

// program is what a fake mirrin prints for `mirrin version`; the fake
// command runner prints a file's contents, so a file holding this "is" that
// version of mirrin.
func program(tag string) []byte { return []byte("mirrin " + tag + "\n") }

// fakeCommands stands in for the programs an update runs: `<bin> version`
// prints the file, ditto copies a folder, and cosign answers with cosignOut
// and fails when cosignErr is set.
type fakeCommands struct {
	cosignOut string
	cosignErr bool
	calls     []string
}

func (c *fakeCommands) run(ctx context.Context, name string, args ...string) (string, error) {
	c.calls = append(c.calls, filepath.Base(name)+" "+strings.Join(args, " "))
	switch {
	case filepath.Base(name) == "cosign":
		if c.cosignErr {
			return c.cosignOut, errors.New("exit status 1")
		}
		return c.cosignOut, nil
	case filepath.Base(name) == "ditto":
		return "", copyTree(args[0], args[1])
	case len(args) == 1 && args[0] == "version":
		b, err := os.ReadFile(name)
		return string(b), err
	}
	return "", fmt.Errorf("unexpected command %s %v", name, args)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o755)
	})
}

type updateTest struct {
	u    *updater
	out  *bytes.Buffer
	twin *fakeTwin
	cmds *fakeCommands
	rel  *fakeRelease
	cli  string // the installed mirrin
}

// newUpdateTest has mirrin oldTag installed in a temp folder and newTag
// published, for this machine's platform.
func newUpdateTest(t *testing.T) *updateTest {
	t.Helper()
	dir := t.TempDir()
	name := "mirrin"
	if runtime.GOOS == "windows" {
		name = "mirrin.exe"
	}
	cli := filepath.Join(dir, name)
	writeTestFile(t, cli, program(oldTag))
	ut := &updateTest{
		out: &bytes.Buffer{}, twin: &fakeTwin{}, cmds: &fakeCommands{}, cli: cli,
		rel: &fakeRelease{files: map[string][]byte{}},
	}
	ut.rel.files[fmt.Sprintf("mirrin-%s-%s", runtime.GOOS, runtime.GOARCH)+map[bool]string{true: ".exe"}[runtime.GOOS == "windows"]] = program(newTag)
	ut.u = &updater{
		out: ut.out, client: &http.Client{}, repo: "MavrkAI/Mirrin", current: oldTag,
		goos: runtime.GOOS, goarch: runtime.GOARCH, exe: cli, whatsapp: true,
		svc: ut.twin, command: ut.cmds.run, signedAdHoc: func(context.Context, string) bool { return false },
		attach: func(context.Context, string, string) error { return errors.New("no disk images here") },
		detach: func(string) {}, remove: os.Remove,
	}
	return ut
}

func writeTestFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

func (ut *updateTest) run(t *testing.T, opts updateOptions) error {
	t.Helper()
	ut.u.releases = ut.rel.serve(t).URL + "/releases"
	return ut.u.run(context.Background(), opts)
}

func (ut *updateTest) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(ut.cli)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// noLeftovers checks that an update left nothing staged beside the program.
func noLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if n := e.Name(); strings.Contains(n, ".new") || strings.Contains(n, ".old") || strings.Contains(n, "write-test") {
			t.Errorf("left %s behind", n)
		}
	}
}

func TestParseUpdateArgs(t *testing.T) {
	cases := []struct {
		args    []string
		want    updateOptions
		wantErr string
	}{
		{nil, updateOptions{}, ""},
		{[]string{"--check"}, updateOptions{check: true}, ""},
		{[]string{"--version", "0.3.0"}, updateOptions{version: "v0.3.0"}, ""},
		{[]string{"--version=v0.4.0-rc.1", "--check"}, updateOptions{check: true, version: "v0.4.0-rc.1"}, ""},
		{[]string{"--version"}, updateOptions{}, "usage"},
		{[]string{"--version", "latest/../../x"}, updateOptions{}, "isn't a release version"},
		{[]string{"--yes"}, updateOptions{}, "unknown option --yes"},
	}
	for _, c := range cases {
		got, err := parseUpdateArgs(c.args)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%v: err = %v, want %q", c.args, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%v = %+v, %v; want %+v", c.args, got, err, c.want)
		}
	}
}

func TestVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v0.3.0", "v0.3.0", 0},
		{"v0.3.1", "v0.3.0", 1},
		{"v0.10.0", "v0.9.9", 1},
		{"v1.0.0", "v0.99.99", 1},
		{"v0.4.0-rc.1", "v0.4.0", -1},
		{"v0.4.0-rc.2", "v0.4.0-rc.10", -1},
		{"v0.4.0-beta", "v0.4.0-alpha", 1},
		{"v0.4.0-rc.1", "v0.3.9", 1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := compareVersions(c.b, c.a); got != -c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
	for v, want := range map[string]bool{
		"v0.3.0": true, "v0.4.0-rc.1": true, "dev": false, "v0.3.0-4-gabc1234": false,
		"v0.3.0-dirty": false, "v0.3.0-4-gabc1234-dirty": false, "abc1234": false,
	} {
		if got := isRelease(v); got != want {
			t.Errorf("isRelease(%q) = %v", v, got)
		}
	}
}

func TestUpdateWhenUpToDate(t *testing.T) {
	ut := newUpdateTest(t)
	ut.u.current = newTag
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), "You have the latest Mirrin ("+newTag+")") {
		t.Fatalf("output:\n%s", ut.out)
	}
	if ut.rel.asked("/releases/download/") {
		t.Fatal("downloaded something with nothing to update")
	}
}

func TestUpdateCheckOnlyTells(t *testing.T) {
	ut := newUpdateTest(t)
	if err := ut.run(t, updateOptions{check: true}); err != nil {
		t.Fatal(err)
	}
	out := ut.out.String()
	for _, want := range []string{"Mirrin " + newTag + " is available (you have " + oldTag + ")", "/releases/tag/" + newTag, "Run `mirrin update`"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	if ut.rel.asked("/releases/download/") || ut.installed(t) != "mirrin "+oldTag || ut.twin.restarts != 0 {
		t.Fatal("--check changed something")
	}
}

func TestUpdateReplacesTheProgramAndRestartsTheService(t *testing.T) {
	ut := newUpdateTest(t)
	ut.twin.installed, ut.twin.running, ut.twin.version, ut.twin.restartTo = true, true, oldTag, newTag
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if got := ut.installed(t); got != "mirrin "+newTag {
		t.Fatalf("installed %q", got)
	}
	if ut.twin.restarts != 1 {
		t.Fatalf("restarted the service %d times", ut.twin.restarts)
	}
	out := ut.out.String()
	for _, want := range []string{"Updating Mirrin " + oldTag + " to " + newTag, "checksum matches", "Mirrin is now " + newTag, "The background twin now runs " + newTag} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "signature verified") {
		t.Error("claimed a signature check without cosign")
	}
	noLeftovers(t, filepath.Dir(ut.cli))
}

// Nothing is replaced unless every check passes.
func TestUpdateRefusesWhatItCannotVerify(t *testing.T) {
	asset := fmt.Sprintf("mirrin-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	cases := []struct {
		name    string
		setup   func(ut *updateTest)
		wantErr string
	}{
		{"tampered", func(ut *updateTest) {
			ut.rel.sums = hexSum([]byte("something else")) + "  " + asset + "\n"
		}, "doesn't match its published checksum"},
		{"no checksum list", func(ut *updateTest) { ut.rel.sums = "-" }, "has no checksum list"},
		{"no build for this machine", func(ut *updateTest) { ut.rel.files = map[string][]byte{"mirrin-plan9-mips": program(newTag)} }, "has no build for"},
		// A release from before the rename has only AntBot's programs, which
		// Mirrin never installs under its own name.
		{"a release from before the rename", func(ut *updateTest) {
			ut.rel.files = map[string][]byte{"antbot" + strings.TrimPrefix(asset, "mirrin"): []byte("antbot " + newTag + "\n")}
		}, "has no build for"},
		{"won't start", func(ut *updateTest) { ut.rel.files[asset] = []byte("garbage") }, "won't start on this machine"},
		{"is an older version", func(ut *updateTest) { ut.rel.files[asset] = program("v9.9.1") }, "won't start on this machine"},
		{"github is busy", func(ut *updateTest) { ut.rel.codes = map[string]int{"/releases/download/" + newTag + "/" + asset: 503} }, "answered HTTP 503"},
		{"no signature, with cosign", func(ut *updateTest) { ut.u.cosign = "/usr/local/bin/cosign" }, "has no Sigstore signature"},
		{"bad signature", func(ut *updateTest) {
			ut.u.cosign = "/usr/local/bin/cosign"
			ut.rel.files["SHA256SUMS.sigstore.json"] = []byte("{}")
			ut.cmds.cosignErr, ut.cmds.cosignOut = true, "Error: none of the expected identities matched\nmain.go:74: error during command execution: none of the expected identities matched"
		}, "cosign said: main.go:74: error during command execution: none of the expected identities matched"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ut := newUpdateTest(t)
			ut.twin.installed = true
			c.setup(ut)
			err := ut.run(t, updateOptions{})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want %q\n%s", err, c.wantErr, ut.out)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "nothing was changed") {
				t.Errorf("doesn't say nothing was changed: %v", err)
			}
			if got := ut.installed(t); got != "mirrin "+oldTag {
				t.Fatalf("the program was replaced: %q", got)
			}
			if ut.twin.restarts != 0 {
				t.Fatal("restarted the service after a failed update")
			}
			noLeftovers(t, filepath.Dir(ut.cli))
		})
	}
}

func TestUpdateChecksTheSignatureWithCosign(t *testing.T) {
	ut := newUpdateTest(t)
	ut.u.cosign = "/opt/bin/cosign"
	ut.rel.files["SHA256SUMS.sigstore.json"] = []byte("{}")
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if !strings.Contains(ut.out.String(), "signature verified with cosign") {
		t.Fatalf("output:\n%s", ut.out)
	}
	var call string
	for _, c := range ut.cmds.calls {
		if strings.HasPrefix(c, "cosign ") {
			call = c
		}
	}
	for _, want := range []string{"verify-blob", "--certificate-identity https://github.com/MavrkAI/Mirrin/.github/workflows/release.yml@refs/tags/" + newTag, "--certificate-oidc-issuer https://token.actions.githubusercontent.com"} {
		if !strings.Contains(call, want) {
			t.Errorf("cosign call %q lacks %q", call, want)
		}
	}
}

func TestUpdateLeavesHomebrewAndSourceBuildsAlone(t *testing.T) {
	ut := newUpdateTest(t)
	ut.u.exe = "/opt/homebrew/Cellar/mirrin/9.9.8/bin/mirrin"
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), "brew upgrade mirrin") || ut.rel.asked("/releases/download/") {
		t.Fatalf("Homebrew install:\n%s", ut.out)
	}

	ut = newUpdateTest(t)
	ut.u.current = "v9.9.8-3-gabc1234-dirty"
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatal(err)
	}
	out := ut.out.String()
	if !strings.Contains(out, "built from source") || !strings.Contains(out, "mirrin update --version "+newTag) || ut.rel.asked("/releases/download/") {
		t.Fatalf("source build:\n%s", out)
	}

	// Asked for a release by name, a source build switches to it.
	if err := ut.run(t, updateOptions{version: newTag}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if got := ut.installed(t); got != "mirrin "+newTag {
		t.Fatalf("installed %q", got)
	}

}

// A build without WhatsApp updates to the -nowhatsapp program, never to the
// one that includes it.
func TestUpdateKeepsABuildWithoutWhatsApp(t *testing.T) {
	exe := map[bool]string{true: ".exe"}[runtime.GOOS == "windows"]
	plain := fmt.Sprintf("mirrin-%s-%s", runtime.GOOS, runtime.GOARCH) + exe
	mit := fmt.Sprintf("mirrin-%s-%s-nowhatsapp", runtime.GOOS, runtime.GOARCH) + exe
	ut := newUpdateTest(t)
	ut.u.whatsapp = false
	if got := ut.u.assetName(); got != mit {
		t.Fatalf("assetName = %q, want %q", got, mit)
	}
	ut.rel.files[mit] = program(newTag)
	ut.rel.files[plain] = []byte("the build with WhatsApp")
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if got := ut.installed(t); got != "mirrin "+newTag {
		t.Fatalf("installed %q", got)
	}
	for _, p := range ut.rel.got {
		if p == "/releases/download/"+newTag+"/"+plain {
			t.Fatal("downloaded the build with WhatsApp")
		}
	}

	// A Mac app beside it (which includes WhatsApp) is left as it is, and it
	// says so, with how to point the service at this program.
	ut = newUpdateTest(t)
	ut.u.whatsapp = false
	apps := filepath.Join(t.TempDir(), "Applications")
	app := filepath.Join(apps, "Mirrin.app")
	writeTestFile(t, filepath.Join(app, "Contents", "MacOS", "mirrin"), program(oldTag))
	ut.u.appDirs = []string{apps}
	ut.rel.files[mit] = program(newTag)
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "mirrin")); string(b) != string(program(oldTag)) {
		t.Fatal("changed the app, which includes WhatsApp")
	}
	if !strings.Contains(ut.out.String(), app+" is left as it is") || !strings.Contains(ut.out.String(), "mirrin service uninstall && mirrin service install") {
		t.Fatalf("output:\n%s", ut.out)
	}

	// A release without the -nowhatsapp build is refused, not swapped for
	// the one with WhatsApp.
	ut = newUpdateTest(t)
	ut.u.whatsapp = false
	if err := ut.run(t, updateOptions{}); err == nil || !strings.Contains(err.Error(), mit) {
		t.Fatalf("err = %v\n%s", err, ut.out)
	}
	if got := ut.installed(t); got != "mirrin "+oldTag {
		t.Fatalf("installed %q", got)
	}
}

func TestUpdateSaysWhatWentWrongReachingGitHub(t *testing.T) {
	ut := newUpdateTest(t)
	ut.rel.codes = map[string]int{"/releases/latest": 429}
	if err := ut.run(t, updateOptions{}); err == nil || !strings.Contains(err.Error(), "answered HTTP 429") {
		t.Fatalf("rate limited: %v", err)
	}
	ut = newUpdateTest(t)
	ut.rel.codes = map[string]int{"/releases/latest": 404}
	if err := ut.run(t, updateOptions{}); err == nil || !strings.Contains(err.Error(), "couldn't find a published Mirrin release") {
		t.Fatalf("no releases: %v", err)
	}
	ut = newUpdateTest(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing answers
	ut.u.releases = srv.URL + "/releases"
	if err := ut.u.run(context.Background(), updateOptions{}); err == nil || !strings.Contains(err.Error(), "Check your internet connection") {
		t.Fatalf("offline: %v", err)
	}
}

func TestUpdateSaysWhenItCannotWriteThere(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder this account can't write to")
	}
	ut := newUpdateTest(t)
	dir := filepath.Dir(ut.cli)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	err := ut.run(t, updateOptions{})
	if err == nil || !strings.Contains(err.Error(), "can't write to "+dir) || !strings.Contains(err.Error(), "sudo mirrin update") {
		t.Fatalf("err = %v", err)
	}
	if ut.rel.asked("/releases/download/") {
		t.Fatal("downloaded before checking it could install")
	}
}

// On Windows a running program can be renamed but not replaced, so the old
// one is moved aside first. The swap works the same on any file system.
func TestUpdateSwapsAWindowsProgram(t *testing.T) {
	ut := newUpdateTest(t)
	ut.u.goos, ut.u.goarch = "windows", "arm64"
	exe := filepath.Join(filepath.Dir(ut.cli), "mirrin.exe")
	writeTestFile(t, exe, program(oldTag))
	ut.u.exe, ut.cli = exe, exe
	ut.rel.files = map[string][]byte{"mirrin-windows-arm64.exe": program(newTag)}
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if got := ut.installed(t); got != "mirrin "+newTag {
		t.Fatalf("installed %q", got)
	}
	noLeftovers(t, filepath.Dir(exe))
}

// A twin started by hand can still be running the mirrin an earlier update
// moved aside, which Windows can neither delete nor replace; the program
// being replaced then goes beside it under a new name.
func TestUpdateSwapsAWindowsProgramBesideOneStillRunning(t *testing.T) {
	ut := newUpdateTest(t)
	ut.u.goos, ut.u.goarch = "windows", "amd64"
	dir := filepath.Dir(ut.cli)
	exe := filepath.Join(dir, "mirrin.exe")
	writeTestFile(t, exe, program(oldTag))
	ut.u.exe, ut.cli = exe, exe
	running := filepath.Join(dir, "mirrin.old.exe")
	writeTestFile(t, running, program("v9.9.7"))
	stale := filepath.Join(dir, "mirrin.old-123.exe")
	writeTestFile(t, stale, program("v9.9.6"))
	ut.u.remove = func(p string) error {
		if p == running {
			return &os.PathError{Op: "remove", Path: p, Err: errors.New("Access is denied.")}
		}
		return os.Remove(p)
	}
	ut.rel.files = map[string][]byte{"mirrin-windows-amd64.exe": program(newTag)}
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if got := ut.installed(t); got != "mirrin "+newTag {
		t.Fatalf("installed %q", got)
	}
	if b, _ := os.ReadFile(running); string(b) != string(program("v9.9.7")) {
		t.Fatalf("replaced the mirrin that's still running: %q", b)
	}
	if exists(stale) {
		t.Error("left an earlier update's leftover behind")
	}
}

func TestInstallLine(t *testing.T) {
	for _, c := range []struct{ goos, repo, want string }{
		{"windows", "MavrkAI/Mirrin", "irm https://raw.githubusercontent.com/MavrkAI/Mirrin/main/install.ps1 | iex"},
		{"darwin", "MavrkAI/Mirrin", "curl -fsSL https://raw.githubusercontent.com/MavrkAI/Mirrin/main/install.sh | sh"},
		{"linux", "me/fork", "curl -fsSL https://raw.githubusercontent.com/me/fork/main/install.sh | sh"},
	} {
		if got := installLine(c.goos, c.repo); got != c.want {
			t.Errorf("installLine(%s, %s) = %q, want %q", c.goos, c.repo, got, c.want)
		}
	}

	// A source build on Windows, from a fork, is told the fork's PowerShell line.
	ut := newUpdateTest(t)
	ut.u.goos, ut.u.repo, ut.u.current = "windows", "me/fork", "dev"
	ut.rel.codes = map[string]int{"/releases/latest": 503}
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), "install a release with: irm https://raw.githubusercontent.com/me/fork/main/install.ps1 | iex") {
		t.Fatalf("output:\n%s", ut.out)
	}
}

// Going back to an older release says the twin is kept as it is, and how to
// keep a copy first.
func TestUpdateGoesBackWhenAsked(t *testing.T) {
	ut := newUpdateTest(t)
	ut.u.current = "v9.9.10"
	writeTestFile(t, ut.cli, program("v9.9.10"))
	if err := ut.run(t, updateOptions{version: newTag}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if got := ut.installed(t); got != "mirrin "+newTag {
		t.Fatalf("installed %q", got)
	}
	out := ut.out.String()
	for _, want := range []string{"Going back to " + newTag + " (you have v9.9.10)", "an older Mirrin may not understand everything a newer one saved", "mirrin identity export"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
}

// On a Mac the app is updated with the CLI: the background service runs the
// app's copy of mirrin when there is one.
func TestUpdateReplacesTheMacApp(t *testing.T) {
	ut := newUpdateTest(t)
	ut.u.goos = "darwin"
	apps := filepath.Join(t.TempDir(), "Applications")
	app := filepath.Join(apps, "Mirrin.app")
	writeTestFile(t, filepath.Join(app, "Contents", "MacOS", "mirrin"), program(oldTag))
	writeTestFile(t, filepath.Join(app, "Contents", "Info.plist"), []byte("old"))
	ut.u.appDirs = []string{filepath.Join(t.TempDir(), "none"), apps}
	ut.rel.files = map[string][]byte{"mirrin-darwin-" + runtime.GOARCH: program(newTag), "Mirrin-" + newTag + "-macos.dmg": program(newTag)}
	ut.u.goarch = runtime.GOARCH
	// The "disk image" holds the new mirrin; attaching it lays out an app.
	ut.u.attach = func(_ context.Context, dmg, mnt string) error {
		b, err := os.ReadFile(dmg)
		if err != nil {
			return err
		}
		writeTestFile(t, filepath.Join(mnt, "Mirrin.app", "Contents", "MacOS", "mirrin"), b)
		writeTestFile(t, filepath.Join(mnt, "Mirrin.app", "Contents", "Info.plist"), []byte("new"))
		return nil
	}
	ut.u.signedAdHoc = func(context.Context, string) bool { return true }
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if got := ut.installed(t); got != "mirrin "+newTag {
		t.Fatalf("CLI: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "mirrin")); string(b) != string(program(newTag)) {
		t.Fatalf("app: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "Contents", "Info.plist")); string(b) != "new" {
		t.Fatalf("the app was merged, not replaced: Info.plist %q", b)
	}
	noLeftovers(t, apps)
	if !strings.Contains(ut.out.String(), "may ask again for the microphone") {
		t.Errorf("no word about macOS permissions for an ad-hoc signed app:\n%s", ut.out)
	}

	// Run from inside the app, only the app is updated; a tampered disk image
	// leaves it as it was.
	ut2 := newUpdateTest(t)
	ut2.u.goos, ut2.u.attach = "darwin", ut.u.attach
	ut2.u.exe = filepath.Join(app, "Contents", "MacOS", "mirrin")
	writeTestFile(t, ut2.u.exe, program(oldTag))
	ut2.rel.files = map[string][]byte{"Mirrin-" + newTag + "-macos.dmg": program(newTag)}
	ut2.rel.sums = hexSum([]byte("not it")) + "  Mirrin-" + newTag + "-macos.dmg\n"
	if err := ut2.run(t, updateOptions{}); err == nil || !strings.Contains(err.Error(), "doesn't match its published checksum") {
		t.Fatalf("tampered disk image: %v", err)
	}
	if b, _ := os.ReadFile(ut2.u.exe); string(b) != string(program(oldTag)) {
		t.Fatalf("the app changed: %q", b)
	}
	if got := ut2.installed(t); got != "mirrin "+oldTag {
		t.Fatalf("the CLI changed: %q", got)
	}
}

// The service runs the app's copy of mirrin, so an app that couldn't be
// updated is reported as unfinished, and the next `mirrin update` finishes
// it even though the CLI is already current.
func TestUpdateFinishesAnAppLeftBehind(t *testing.T) {
	setup := func(t *testing.T) (*updateTest, string) {
		ut := newUpdateTest(t)
		ut.u.goos, ut.u.goarch = "darwin", "arm64"
		apps := filepath.Join(t.TempDir(), "Applications")
		app := filepath.Join(apps, "Mirrin.app")
		writeTestFile(t, filepath.Join(app, "Contents", "MacOS", "mirrin"), program(oldTag))
		ut.u.appDirs = []string{apps}
		ut.rel.files = map[string][]byte{"mirrin-darwin-arm64": program(newTag), "Mirrin-" + newTag + "-macos.dmg": program(newTag)}
		return ut, app
	}
	ut, app := setup(t)
	ut.twin.installed = true
	ut.u.attach = func(context.Context, string, string) error {
		return errors.New("hdiutil: attach failed - no mountable file systems")
	}
	err := ut.run(t, updateOptions{})
	if err == nil || !strings.Contains(err.Error(), "the mirrin command is now "+newTag+", but Mirrin.app wasn't updated") || !strings.Contains(err.Error(), "Run `mirrin update` again") {
		t.Fatalf("err = %v\n%s", err, ut.out)
	}
	if ut.twin.restarts != 0 {
		t.Fatal("restarted a service that still runs the old app")
	}
	if strings.Contains(strings.ToLower(err.Error()), "nothing was changed") {
		t.Errorf("says nothing was changed after the CLI was: %v", err)
	}

	// A disk image that doesn't match its checksum, after the CLI was
	// replaced, doesn't claim nothing changed either.
	ut, _ = setup(t)
	ut.rel.sums = hexSum(program(newTag)) + "  mirrin-darwin-arm64\n" + hexSum([]byte("not it")) + "  Mirrin-" + newTag + "-macos.dmg\n"
	err = ut.run(t, updateOptions{})
	if err == nil || !strings.Contains(err.Error(), "doesn't match its published checksum") || !strings.Contains(err.Error(), "the mirrin command is now "+newTag) {
		t.Fatalf("err = %v\n%s", err, ut.out)
	}
	if strings.Contains(strings.ToLower(err.Error()), "nothing was changed") {
		t.Errorf("says nothing was changed after the CLI was: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "mirrin")); string(b) != string(program(oldTag)) {
		t.Fatalf("a failed app update changed the app: %q", b)
	}

	ut, app = setup(t)
	ut.u.current = newTag // the CLI was updated last time
	writeTestFile(t, ut.cli, program(newTag))
	if err := ut.run(t, updateOptions{check: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), "but Mirrin.app is still "+oldTag) || ut.rel.asked("/releases/download/") {
		t.Fatalf("--check:\n%s", ut.out)
	}
	ut.u.attach = func(_ context.Context, dmg, mnt string) error {
		b, err := os.ReadFile(dmg)
		if err == nil {
			writeTestFile(t, filepath.Join(mnt, "Mirrin.app", "Contents", "MacOS", "mirrin"), b)
		}
		return err
	}
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "mirrin")); string(b) != string(program(newTag)) {
		t.Fatalf("app: %q\n%s", b, ut.out)
	}
	if ut.rel.asked("/releases/download/" + newTag + "/mirrin-darwin-arm64") {
		t.Error("downloaded the CLI again")
	}
}

func TestUpdateTellsHowToRestartATwinItCannot(t *testing.T) {
	// A twin started by hand keeps running the old program.
	ut := newUpdateTest(t)
	ut.twin.running, ut.twin.version = true, oldTag
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), "Your twin is still running "+oldTag+". Quit it") || ut.twin.restarts != 0 {
		t.Fatalf("output:\n%s", ut.out)
	}

	// A service that starts another copy of mirrin stays on the old version.
	ut = newUpdateTest(t)
	ut.twin.installed, ut.twin.running, ut.twin.version = true, true, oldTag
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), "mirrin service uninstall && mirrin service install") {
		t.Fatalf("output:\n%s", ut.out)
	}

	// A restart that fails says what to do.
	ut = newUpdateTest(t)
	ut.twin.installed, ut.twin.restartErr = true, errors.New("launchctl said no")
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ut.out.String(), "It didn't restart: launchctl said no. Try `mirrin service restart`") {
		t.Fatalf("output:\n%s", ut.out)
	}

	// Under sudo, the owner's service isn't the one root would find.
	ut = newUpdateTest(t)
	ut.u.sudo, ut.twin.installed = true, true
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ut.twin.restarts != 0 || !strings.Contains(ut.out.String(), "without sudo") {
		t.Fatalf("output:\n%s", ut.out)
	}
}

// The real thing: a downloaded program is made executable and run.
func TestUpdateRunsTheNewProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the program")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ut := newUpdateTest(t)
	ut.u.command = runCommand
	script := []byte("#!/bin/sh\necho \"mirrin " + newTag + "\"\n")
	for name := range ut.rel.files {
		ut.rel.files[name] = script
	}
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	out, err := exec.Command(ut.cli, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "mirrin "+newTag {
		t.Fatalf("the installed program says %q (%v)", out, err)
	}
	if st, _ := os.Stat(ut.cli); st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("not executable: %v", st.Mode())
	}
}

// The real thing on a Mac: a disk image made the way release-macos.sh makes
// it is opened with hdiutil, and the app is copied out with ditto.
func TestUpdateAppFromARealDiskImage(t *testing.T) {
	if runtime.GOOS != "darwin" || testing.Short() {
		t.Skip("needs hdiutil")
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("no hdiutil")
	}
	src := filepath.Join(t.TempDir(), "Mirrin.app")
	writeTestFile(t, filepath.Join(src, "Contents", "MacOS", "mirrin"), []byte("#!/bin/sh\necho \"mirrin "+newTag+"\"\n"))
	dmg := filepath.Join(t.TempDir(), "Mirrin-"+newTag+"-macos.dmg")
	if out, err := exec.Command("hdiutil", "create", "-volname", "Mirrin", "-srcfolder", src, "-ov", "-format", "UDZO", "-quiet", dmg).CombinedOutput(); err != nil {
		t.Skipf("hdiutil create: %v: %s", err, out)
	}
	image, err := os.ReadFile(dmg)
	if err != nil {
		t.Fatal(err)
	}

	ut := newUpdateTest(t)
	apps := filepath.Join(t.TempDir(), "Applications")
	app := filepath.Join(apps, "Mirrin.app")
	writeTestFile(t, filepath.Join(app, "Contents", "MacOS", "mirrin"), program(oldTag))
	ut.u.exe = filepath.Join(app, "Contents", "MacOS", "mirrin")
	ut.u.command, ut.u.attach, ut.u.detach = runCommand, attachDMG, detachDMG
	ut.rel.files = map[string][]byte{"Mirrin-" + newTag + "-macos.dmg": image}
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	out, err := exec.Command(ut.u.exe, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "mirrin "+newTag {
		t.Fatalf("the app's mirrin says %q (%v)", out, err)
	}
	noLeftovers(t, apps)
}

func TestInstallPaths(t *testing.T) {
	for p, want := range map[string]bool{
		"/opt/homebrew/Cellar/mirrin/0.3.0/bin/mirrin":              true,
		"/usr/local/Cellar/mirrin/0.3.0/bin/mirrin":                 true,
		"/home/linuxbrew/.linuxbrew/Cellar/mirrin/0.3.0/bin/mirrin": true,
		"/Users/me/.local/bin/mirrin":                               false,
	} {
		if got := homebrewPath(p); got != want {
			t.Errorf("homebrewPath(%s) = %v", p, got)
		}
	}
	if got := appBundle("/Applications/Mirrin.app/Contents/MacOS/mirrin"); got != filepath.FromSlash("/Applications/Mirrin.app") {
		t.Errorf("appBundle = %q", got)
	}
	if got := appBundle("/usr/local/bin/mirrin"); got != "" {
		t.Errorf("appBundle of a CLI = %q", got)
	}
}

// A repository renamed on GitHub answers its old releases/latest with a
// redirect to the new name's (MavrkAI/OldName here stands for any old name).
// The update follows it, downloads from the new name, and checks the
// signature against the repository that signs releases now.
func TestUpdateFollowsARenamedRepository(t *testing.T) {
	ut := newUpdateTest(t)
	ut.u.cosign = "/opt/bin/cosign"
	ut.u.repo = "MavrkAI/OldName"
	ut.rel.files["SHA256SUMS.sigstore.json"] = []byte("{}")
	mux := http.NewServeMux()
	mux.Handle("/MavrkAI/Mirrin/", http.StripPrefix("/MavrkAI/Mirrin", ut.rel.handler()))
	mux.HandleFunc("/MavrkAI/OldName/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/MavrkAI/Mirrin/releases/latest", http.StatusMovedPermanently)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ut.u.releases = srv.URL + "/MavrkAI/OldName/releases"
	if err := ut.u.run(context.Background(), updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if got := ut.installed(t); got != "mirrin "+newTag {
		t.Fatalf("installed %q\n%s", got, ut.out)
	}
	var call string
	for _, c := range ut.cmds.calls {
		if strings.HasPrefix(c, "cosign ") {
			call = c
		}
	}
	if !strings.Contains(call, "--certificate-identity https://github.com/MavrkAI/Mirrin/.github/workflows/release.yml@refs/tags/"+newTag) {
		t.Errorf("cosign call %q isn't for the new name", call)
	}
	if !strings.Contains(ut.out.String(), "What's new: "+srv.URL+"/MavrkAI/Mirrin/releases/tag/"+newTag) {
		t.Errorf("output:\n%s", ut.out)
	}

	// Releases that keep sending latest elsewhere don't send it round forever.
	loop := http.NewServeMux()
	loop.HandleFunc("/a/b/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/c/d/releases/latest", http.StatusMovedPermanently)
	})
	loop.HandleFunc("/c/d/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/a/b/releases/latest", http.StatusMovedPermanently)
	})
	srv = httptest.NewServer(loop)
	t.Cleanup(srv.Close)
	ut = newUpdateTest(t)
	ut.u.releases = srv.URL + "/a/b/releases"
	if _, err := ut.u.latest(context.Background()); err == nil || !strings.Contains(err.Error(), "couldn't find a published Mirrin release") {
		t.Fatalf("a redirect loop: %v", err)
	}
}

func TestMovedReleases(t *testing.T) {
	from, _ := url.Parse("https://github.com/MavrkAI/OldName/releases/latest")
	for loc, want := range map[string]string{
		"https://github.com/MavrkAI/Mirrin/releases/latest":  "https://github.com/MavrkAI/Mirrin/releases",
		"/MavrkAI/Mirrin/releases/latest/":                   "https://github.com/MavrkAI/Mirrin/releases",
		"https://GitHub.com/MavrkAI/Mirrin/releases/latest":  "https://GitHub.com/MavrkAI/Mirrin/releases",
		"https://github.com/MavrkAI/Mirrin/releases/tag/v1":  "", // the answer, not a move
		"https://github.com/MavrkAI/OldName/releases":        "", // no releases yet
		"https://example.com/MavrkAI/Mirrin/releases/latest": "", // another server
		"http://github.com/MavrkAI/Mirrin/releases/latest":   "", // not https any more
		"https://github.com/releases/latest":                 "",
		"https://github.com/MavrkAI/OldName/releases/latest": "", // itself
	} {
		got, ok := movedReleases(from, loc)
		if s := map[bool]string{true: fmt.Sprint(got)}[ok]; s != want {
			t.Errorf("%s: %q, want %q", loc, s, want)
		}
	}
}

// Settings named before the rename (ANTBOT_REPO, ANTBOT_DOWNLOAD_URL) still
// work; the MIRRIN_ names win.
func TestUpdateReadsTheOldSettings(t *testing.T) {
	for _, n := range []string{"REPO", "DOWNLOAD_URL"} {
		for _, p := range []string{"MIRRIN_", "ANTBOT_", "OPENHUMAN_"} {
			t.Setenv(p+n, "")
		}
	}
	t.Setenv("ANTBOT_REPO", "me/fork")
	t.Setenv("ANTBOT_DOWNLOAD_URL", "https://mirror.example/releases/")
	u, err := newUpdater(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if u.repo != "me/fork" || u.releases != "https://mirror.example/releases" {
		t.Fatalf("old settings: repo %q, releases %q", u.repo, u.releases)
	}
	t.Setenv("MIRRIN_REPO", "me/new")
	t.Setenv("ANTBOT_DOWNLOAD_URL", "")
	if u, err = newUpdater(io.Discard); err != nil || u.repo != "me/new" || u.releases != "https://github.com/me/new/releases" {
		t.Fatalf("new settings: repo %q, releases %q, %v", u.repo, u.releases, err)
	}
}

// AntBot.app, the app from before the rename, isn't Mirrin.app: an update
// leaves it alone (a service from then may still run it), and says what it
// is. Mirrin's own programs answer under any of their names.
func TestUpdateLeavesAnOldAntBotApp(t *testing.T) {
	ut := newUpdateTest(t)
	ut.u.goos = "darwin"
	apps := filepath.Join(t.TempDir(), "Applications")
	old := filepath.Join(apps, "AntBot.app")
	writeApp(t, old, "com.antbot.mavrk")
	theirs := filepath.Join(t.TempDir(), "Applications")
	writeApp(t, filepath.Join(theirs, "AntBot.app"), "com.example.antbot")
	ut.u.appDirs = []string{apps, theirs}
	if err := ut.run(t, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, ut.out)
	}
	if got := ut.installed(t); got != "mirrin "+newTag {
		t.Fatalf("CLI: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(old, "Contents", "MacOS", "antbot")); string(b) != string(program(oldTag)) {
		t.Fatalf("the old app changed: %q", b)
	}
	out := ut.out.String()
	if !strings.Contains(out, old+" is the old AntBot app, which Mirrin doesn't use") || strings.Contains(out, theirs) {
		t.Fatalf("output:\n%s", out)
	}

	for line, want := range map[string]string{"mirrin " + newTag: newTag, "antbot " + newTag: newTag, "openhuman " + newTag: newTag, "AntBot 2.0": ""} {
		bin := filepath.Join(t.TempDir(), "prog")
		writeTestFile(t, bin, []byte(line+"\n"))
		if got := ut.u.versionOf(context.Background(), bin); got != want {
			t.Errorf("%q: version %q, want %q", line, got, want)
		}
	}
}
