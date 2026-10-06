// Package scripts holds the tests for the release and install shell scripts.
// Each test runs the real script with sh against a local fake release, so
// nothing touches the network, GitHub or the contributor's home directory.
package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const testVersion = "v9.9.9"

// TestMain doubles as a stand-in mirrin.exe for the Windows installer test,
// which needs a real executable to answer `version`.
func TestMain(m *testing.M) {
	if os.Getenv("MIRRIN_FAKE_MIRRIN") == "1" && len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("mirrin " + testVersion)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func needSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the POSIX installer is for macOS and Linux")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
}

func repoFile(name string) string {
	p, _ := filepath.Abs(filepath.Join("..", name))
	return p
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fakeBinary is a stand-in mirrin that answers `version` like the real one.
func fakeBinary(name string) []byte {
	return []byte("#!/bin/sh\necho \"mirrin " + testVersion + " (" + name + ")\"\n")
}

// release serves a GitHub-shaped release: /latest redirects to the tag, and
// /download/<tag>/<file> serves files. SHA256SUMS is written from files
// unless sums overrides it ("-" leaves it out). codes makes a file, or
// "latest", answer with that HTTP status instead, as GitHub does when it is
// rate limiting or down.
func release(t *testing.T, files map[string][]byte, sums string, codes map[string]int) *httptest.Server {
	t.Helper()
	if sums == "" {
		var b strings.Builder
		for name, body := range files {
			fmt.Fprintf(&b, "%s  %s\n", sum(body), name)
		}
		sums = b.String()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if code := codes["latest"]; code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		http.Redirect(w, r, "/releases/tag/"+testVersion, http.StatusFound)
	})
	mux.HandleFunc("/releases/tag/", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/releases/download/"+testVersion+"/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/releases/download/"+testVersion+"/")
		if code := codes[name]; code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		if name == "SHA256SUMS" && sums != "-" {
			_, _ = w.Write([]byte(sums))
			return
		}
		if b, ok := files[name]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// guarded are the commands install.sh changes files with.
var guarded = []string{"mv", "mkdir", "rm", "chmod", "cp", "ln"}

// guard is a stand-in for one of those commands that refuses any absolute
// path outside the temp folder. A broken installer that ignores
// MIRRIN_BIN_DIR or XDG_BIN_HOME then fails its test instead of writing to
// the contributor's ~/.local/bin or /usr/local/bin.
func guard(t *testing.T, real string) string {
	t.Helper()
	tmp := filepath.Clean(os.TempDir())
	roots := []string{shellQuote(tmp) + "/*"}
	if r, err := filepath.EvalSymlinks(tmp); err == nil && r != tmp {
		roots = append(roots, shellQuote(r)+"/*")
	}
	return fmt.Sprintf(`for a in "$@"; do
  case "$a" in
    /*) case "$a" in %s) ;; *) echo "test guard: ${0##*/} refused $a, which is outside the temp folder" >&2; exit 1 ;; esac ;;
  esac
done
exec %s "$@"
`, strings.Join(roots, " | "), shellQuote(real))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// fakeTools puts uname and sysctl that report the given machine ahead of the
// real ones on PATH, with cosign when a body is given, and launchctl and
// systemctl that know no services unless given a body. The real service
// managers are never asked, so a contributor's own Mirrin service can't
// change what a test sees; nor is their own AntBot from before the rename
// (an antbot that isn't AntBot's stands in, unless given a body). The file
// commands are guarded (see guard).
func fakeTools(t *testing.T, in install) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range guarded {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("no %s", name)
		}
		write(name, guard(t, real))
	}
	write("uname", fmt.Sprintf("case \"$1\" in -m) echo %q ;; *) echo %q ;; esac\n", in.machine, in.kernel))
	if in.arm64 == "" {
		write("sysctl", "exit 1\n")
	} else {
		write("sysctl", "echo "+in.arm64+"\n")
	}
	if in.cosign != "" {
		write("cosign", in.cosign)
	}
	// A contributor's own gh must never be asked (it would reach GitHub):
	// by default a fake one that isn't signed in comes first on PATH.
	switch in.gh {
	case "":
		write("gh", "exit 1\n")
	case "-":
	default:
		write("gh", in.gh)
	}
	if in.xattr != "" {
		write("xattr", in.xattr)
	}
	if in.spctl != "" {
		write("spctl", in.spctl)
	}
	if in.hdiutil != "" {
		write("hdiutil", in.hdiutil)
	}
	if in.antbot == "" {
		write("antbot", "echo 'antbot: an unrelated program'\n")
	} else {
		write("antbot", in.antbot)
	}
	for name, body := range map[string]string{"launchctl": in.launchctl, "systemctl": in.systemctl} {
		if body == "" {
			body = "exit 1\n"
		}
		write(name, body)
	}
	return dir
}

type install struct {
	kernel, machine, arm64 string
	cosign                 string // body of a fake cosign; empty means none installed
	gh                     string // body of a fake gh; empty means one not signed in, "-" none (use with pick)
	launchctl, systemctl   string // bodies of fake service managers; empty means no services
	xattr, spctl, hdiutil  string // bodies of fake macOS tools; empty means the real ones, if any
	antbot                 string // body of the antbot on PATH; empty means one that isn't AntBot's
	files                  map[string][]byte
	sums                   string
	codes                  map[string]int // see release
	env                    []string
	onPath                 bool // the install folder is already on PATH

	// pick lets the installer choose the folder, as it does for users: no
	// MIRRIN_BIN_DIR, and XDG_BIN_HOME (its ~/.local/bin) is dest. PATH is
	// then the fake tools, path, and the system folders only, so nothing
	// outside the test's own folders can be chosen.
	pick bool
	path []string
}

// systemPath is the PATH for installs that pick their own folder. It must hold
// no mirrin, or the installer would rightly choose to upgrade it.
func systemPath(t *testing.T) string {
	t.Helper()
	dirs := []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "mirrin")); err == nil {
			t.Skipf("%s/mirrin exists; the installer would upgrade it", d)
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// run executes install.sh and returns its combined output and the install folder.
func (in install) run(t *testing.T) (string, string, error) {
	t.Helper()
	srv := release(t, in.files, in.sums, in.codes)
	tools := fakeTools(t, in)
	dest := filepath.Join(t.TempDir(), "bin")
	path := tools + string(os.PathListSeparator) + os.Getenv("PATH")
	if in.pick {
		path = strings.Join(append(append([]string{tools}, in.path...), systemPath(t)), string(os.PathListSeparator))
	}
	if in.onPath {
		path = dest + string(os.PathListSeparator) + path
	}
	cmd := exec.Command("sh", repoFile("install.sh"))
	cmd.Dir = t.TempDir()
	// These settings keep the installer from writing anywhere but dest, the
	// guarded file commands make sure of it, and a throwaway HOME catches
	// anything else (a relative path or a redirect): a test run once left a
	// fake mirrin in a contributor's real ~/.local/bin. No Chrome runs here,
	// so the keychain rule about HOME doesn't apply.
	cmd.Env = append(withoutOldSettings(os.Environ()),
		"HOME="+t.TempDir(),
		"PATH="+path,
		"SHELL=/bin/sh",
		"MIRRIN_DOWNLOAD_URL="+srv.URL+"/releases",
		"MIRRIN_BIN_DIR="+dest,
		"MIRRIN_NO_MODIFY_PATH=1",
		"MIRRIN_VERSION=",
	)
	if in.pick {
		cmd.Env = append(cmd.Env, "MIRRIN_BIN_DIR=", "XDG_BIN_HOME="+dest)
	}
	cmd.Env = append(cmd.Env, in.env...)
	out, err := cmd.CombinedOutput()
	return string(out), dest, err
}

// withoutOldSettings is env without the installers' settings from before
// the rename (ANTBOT_*), which they still read: a contributor's own must not
// change what a test does.
func withoutOldSettings(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(strings.ToUpper(kv), "ANTBOT_") {
			out = append(out, kv)
		}
	}
	return out
}

func TestInstallPicksTheRightBuild(t *testing.T) {
	needSh(t)
	files := map[string][]byte{}
	for _, a := range []string{"mirrin-linux-amd64", "mirrin-linux-arm64", "mirrin-darwin-amd64", "mirrin-darwin-arm64"} {
		files[a] = fakeBinary(a)
	}
	cases := []struct {
		name, kernel, machine, arm64 string
		want                         string
	}{
		{"Linux PC", "Linux", "x86_64", "", "mirrin-linux-amd64"},
		{"Linux ARM server or Pi", "Linux", "aarch64", "", "mirrin-linux-arm64"},
		{"Intel Mac", "Darwin", "x86_64", "0", "mirrin-darwin-amd64"},
		{"Apple silicon", "Darwin", "arm64", "1", "mirrin-darwin-arm64"},
		{"Apple silicon under Rosetta", "Darwin", "x86_64", "1", "mirrin-darwin-arm64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, dest, err := install{kernel: c.kernel, machine: c.machine, arm64: c.arm64, files: files, onPath: true,
				env: []string{"MIRRIN_NO_APP=1"}}.run(t)
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			got, err := os.ReadFile(filepath.Join(dest, "mirrin"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), "("+c.want+")") {
				t.Fatalf("installed the wrong build:\n%s", got)
			}
			for _, want := range []string{"checksum matches", "Mirrin " + testVersion + " is installed", "mirrin chat"} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if _, err := os.Stat(filepath.Join(dest, ".mirrin.new")); err == nil {
				t.Error("left the staging copy behind")
			}
		})
	}
}

func TestInstallRefusesWhatItCannotVerify(t *testing.T) {
	needSh(t)
	good := fakeBinary("mirrin-linux-amd64")
	// A port where nothing listens.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	offline := "http://" + l.Addr().String() + "/releases"
	_ = l.Close()
	withGood := map[string][]byte{"mirrin-linux-amd64": good}
	cases := []struct {
		name    string
		in      install
		want    string
		notWant string // a message that would send the user the wrong way
	}{
		{"tampered download", install{files: map[string][]byte{"mirrin-linux-amd64": good},
			sums: sum([]byte("something else")) + "  mirrin-linux-amd64\n"}, "doesn't match its published checksum", ""},
		{"not in the checksum list", install{files: map[string][]byte{"mirrin-linux-amd64": good},
			sums: sum(good) + "  mirrin-linux-arm64\n"}, "doesn't include mirrin-linux-amd64", ""},
		{"release without SHA256SUMS", install{files: map[string][]byte{"mirrin-linux-amd64": good}, sums: "-"},
			"has no checksum list", ""},
		{"no build for this machine", install{files: map[string][]byte{"mirrin-linux-arm64": good}},
			"has no build for linux/amd64", ""},
		{"unsupported CPU", install{machine: "armv7l", files: map[string][]byte{}}, "no Mirrin build for linux on armv7l", ""},
		{"Windows shell", install{kernel: "MINGW64_NT-10.0", files: map[string][]byte{}}, "install.ps1", ""},
		{"signature fails with cosign installed", install{files: map[string][]byte{"mirrin-linux-amd64": good,
			"SHA256SUMS.sigstore.json": []byte("{}")}, cosign: "exit 1\n"}, "signature", ""},
		// cosign's own reason reaches the user: a proxy, an old cosign, a bad signature.
		{"cosign says why", install{files: map[string][]byte{"mirrin-linux-amd64": good,
			"SHA256SUMS.sigstore.json": []byte("{}")}, cosign: "echo 'Error: none of the expected identities matched' >&2\nexit 1\n"},
			"cosign said: Error: none of the expected identities matched", ""},
		// Every release is signed, so a missing signature is refused, not skipped.
		{"signature missing with cosign installed", install{files: withGood, cosign: "exit 0\n"},
			"has no Sigstore signature", "signature verified"},
		{"GitHub rate limits the checksum list", install{files: withGood, codes: map[string]int{"SHA256SUMS": 403}},
			"answered HTTP 403 for SHA256SUMS", "has no checksum list"},
		{"GitHub rate limits the build", install{files: withGood, codes: map[string]int{"mirrin-linux-amd64": 403}},
			"answered HTTP 403 for mirrin-linux-amd64", "has no build"},
		{"GitHub rate limits the signature", install{files: withGood, cosign: "exit 0\n", codes: map[string]int{"SHA256SUMS.sigstore.json": 403}},
			"answered HTTP 403 for SHA256SUMS.sigstore.json", "has no Sigstore signature"},
		{"GitHub rate limits the latest-release page", install{files: withGood, codes: map[string]int{"latest": 429}},
			"answered HTTP 429 for the latest release", "couldn't find a published"},
		{"no release published yet", install{files: withGood, codes: map[string]int{"latest": 404}},
			"couldn't find a published Mirrin release", ""},
		{"nothing answers", install{files: withGood, env: []string{"MIRRIN_DOWNLOAD_URL=" + offline}},
			"couldn't reach " + strings.TrimPrefix(strings.TrimSuffix(offline, "/releases"), "http://") + " for the latest release", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			if in.kernel == "" {
				in.kernel = "Linux"
			}
			if in.machine == "" {
				in.machine = "x86_64"
			}
			out, dest, err := in.run(t)
			if err == nil {
				t.Fatalf("install succeeded; want a refusal:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("want %q in:\n%s", c.want, out)
			}
			if c.notWant != "" && strings.Contains(out, c.notWant) {
				t.Errorf("don't want %q in:\n%s", c.notWant, out)
			}
			if _, err := os.Stat(filepath.Join(dest, "mirrin")); err == nil {
				t.Error("installed a binary anyway")
			}
			if strings.Contains(out, "curl: (") {
				t.Errorf("a raw curl error reached the user:\n%s", out)
			}
		})
	}
}

func TestInstallKeepsTheOldCopyWhenTheNewOneWontRun(t *testing.T) {
	needSh(t)
	broken := []byte("#!/bin/sh\nexit 1\n")
	srvFiles := map[string][]byte{"mirrin-linux-amd64": broken}
	in := install{kernel: "Linux", machine: "x86_64", files: srvFiles}
	out, dest, err := in.runWithExisting(t, []byte("old"))
	if err == nil || !strings.Contains(out, "won't start on this machine") {
		t.Fatalf("want a refusal, got err=%v:\n%s", err, out)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "mirrin"))
	if string(got) != "old" {
		t.Fatalf("the working copy was replaced: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, ".mirrin.new")); err == nil {
		t.Error("left the staging copy behind")
	}
}

// runWithExisting is run with a mirrin already in the install folder.
func (in install) runWithExisting(t *testing.T, old []byte) (string, string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "mirrin"), old, 0o755); err != nil {
		t.Fatal(err)
	}
	in.env = append(in.env, "MIRRIN_BIN_DIR="+dest)
	out, _, err := in.run(t)
	return out, dest, err
}

// TestInstallTestsStayInTheTempFolder checks the guard itself: with the fake
// tools first on PATH, no file command reaches outside the temp folder.
func TestInstallTestsStayInTheTempFolder(t *testing.T) {
	needSh(t)
	tools := fakeTools(t, install{kernel: "Linux", machine: "x86_64"})
	inside := filepath.Join(t.TempDir(), "made")
	for _, c := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"mkdir", "-p", inside}, true},
		// / can't be written to anyway, so a broken guard can't do harm here.
		{[]string{"mkdir", "-p", "/mirrin-test-guard/x"}, false},
		{[]string{"mv", "-f", inside, "/mirrin-test-guard"}, false},
		{[]string{"chmod", "+x", "/mirrin-test-guard"}, false},
	} {
		cmd := exec.Command("sh", "-c", `"$@"`, "sh")
		cmd.Args = append(cmd.Args, c.args...)
		cmd.Env = append(os.Environ(), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if c.ok != (err == nil) {
			t.Errorf("%v: err=%v\n%s", c.args, err, out)
		}
		if !c.ok && !strings.Contains(string(out), "test guard: "+c.args[0]+" refused") {
			t.Errorf("%v wasn't stopped by the guard:\n%s", c.args, out)
		}
	}
	if _, err := os.Stat(inside); err != nil {
		t.Errorf("the guard stopped a write inside the temp folder: %v", err)
	}
}

// TestInstallUpgradesInPlace checks where an install goes when nobody says.
// An upgrade that put the new mirrin beside the old one left a service
// running the old program, since a service keeps the path it was installed
// from.
func TestInstallUpgradesInPlace(t *testing.T) {
	needSh(t)
	files := map[string][]byte{"mirrin-linux-amd64": fakeBinary("mirrin-linux-amd64")}
	old := []byte("#!/bin/sh\necho old\n")
	cases := []struct {
		name string
		// setup prepares the machine given the installer's ~/.local/bin and
		// another folder, and returns what goes on PATH.
		setup     func(t *testing.T, localBin, other string) []string
		wantIn    string // "local" or "other": where the new mirrin must be
		wantOut   []string
		untouched string // "other/mirrin" must still be this link
	}{
		{"first install", func(t *testing.T, localBin, other string) []string { return nil },
			"local", []string{"isn't on your PATH yet"}, ""},
		{"upgrade the mirrin on PATH", func(t *testing.T, localBin, other string) []string {
			writeFile(t, filepath.Join(other, "mirrin"), old)
			return []string{other}
		}, "other", []string{"it replaced the one that was there"}, ""},
		{"upgrade an earlier install that isn't on PATH", func(t *testing.T, localBin, other string) []string {
			writeFile(t, filepath.Join(localBin, "mirrin"), old)
			return []string{other}
		}, "local", []string{"it replaced the one that was there"}, ""},
		{"leave Homebrew's link alone", func(t *testing.T, localBin, other string) []string {
			cellar := filepath.Join(t.TempDir(), "Cellar", "mirrin")
			writeFile(t, cellar, old)
			if err := os.Symlink(cellar, filepath.Join(other, "mirrin")); err != nil {
				t.Fatal(err)
			}
			return []string{other}
		}, "local", []string{"another mirrin at", "comes first on your PATH"}, "link"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			other := t.TempDir()
			in := install{kernel: "Linux", machine: "x86_64", files: files, pick: true}
			// The installer's ~/.local/bin, made here so an earlier install can be put in it.
			localBin := filepath.Join(t.TempDir(), "bin")
			if err := os.MkdirAll(localBin, 0o755); err != nil {
				t.Fatal(err)
			}
			in.path = c.setup(t, localBin, other)
			in.env = []string{"XDG_BIN_HOME=" + localBin}
			out, _, err := in.run(t)
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			want := map[string]string{"local": localBin, "other": other}[c.wantIn]
			got, err := os.ReadFile(filepath.Join(want, "mirrin"))
			if err != nil || !strings.Contains(string(got), "mirrin-linux-amd64") {
				t.Fatalf("the new mirrin isn't in %s (%v):\n%s", c.wantIn, err, out)
			}
			if !strings.Contains(out, "is installed: "+filepath.Join(want, "mirrin")) {
				t.Errorf("output doesn't say where it went:\n%s", out)
			}
			for _, w := range c.wantOut {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			if c.untouched != "" {
				if fi, err := os.Lstat(filepath.Join(other, "mirrin")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Errorf("Homebrew's link was replaced")
				}
			}
		})
	}
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestInstallServiceNotes checks what an install says about services. An
// upgraded binary does nothing until the service restarts, and the openHuman
// service from before the rename relaunches the old program forever.
func TestInstallServiceNotes(t *testing.T) {
	needSh(t)
	uid := strconv.Itoa(os.Getuid())
	// The fake service managers know one service each; the installer's own
	// folder is in MIRRIN_BIN_DIR, which the fakes can read.
	launchd := func(label, program string) string {
		return fmt.Sprintf("[ \"$1 $2\" = \"print gui/%s/%s\" ] || exit 1\necho \"program = %s\"\n", uid, label, program)
	}
	systemd := func(unit, program string) string {
		return fmt.Sprintf("[ \"$1 $2 $3\" = \"--user cat %s\" ] || exit 1\necho \"ExecStart=%s run\"\n", unit, program)
	}
	mac := map[string][]byte{"mirrin-darwin-arm64": fakeBinary("mirrin-darwin-arm64")}
	linux := map[string][]byte{"mirrin-linux-amd64": fakeBinary("mirrin-linux-amd64")}
	cases := []struct {
		name    string
		in      install
		want    []string
		notWant []string
	}{
		{"no service yet", install{kernel: "Linux", machine: "x86_64", files: linux},
			[]string{"mirrin service install"}, []string{"mirrin service restart"}},
		{"Mac service runs this mirrin", install{kernel: "Darwin", machine: "arm64", arm64: "1", files: mac,
			launchctl: launchd("mirrin", "$MIRRIN_BIN_DIR/mirrin")},
			[]string{"restart it to use " + testVersion, "mirrin service restart"}, []string{"mirrin service install "}},
		{"Mac service runs Mirrin.app", install{kernel: "Darwin", machine: "arm64", arm64: "1", files: mac,
			launchctl: launchd("mirrin", "/Applications/Mirrin.app/Contents/MacOS/mirrin")},
			[]string{"mirrin service restart"}, nil},
		{"Linux service runs a mirrin from elsewhere", install{kernel: "Linux", machine: "x86_64", files: linux,
			systemctl: systemd("mirrin.service", "/opt/old/mirrin")},
			[]string{"runs a mirrin from another folder", "mirrin service uninstall && mirrin service install"}, nil},
		{"openHuman still runs on a Mac", install{kernel: "Darwin", machine: "arm64", arm64: "1", files: mac,
			launchctl: launchd("openhuman", "/Users/x/go/bin/openhuman")},
			[]string{"openHuman (Mirrin's first name)", "Library/LaunchAgents/openhuman.plist", "mirrin service install", "starts again at your next login"},
			[]string{"rm -f", "launchctl bootout"}}, // the plist may hold the only copy of its keys: mirrin moves them first
		{"openHuman still runs on Linux", install{kernel: "Linux", machine: "x86_64", files: linux,
			systemctl: systemd("openhuman.service", "/home/x/go/bin/openhuman")},
			[]string{"openHuman (Mirrin's first name)", ".config/systemd/user/openhuman.service", "mirrin service install"},
			[]string{"rm -f", "disable --now"}},
		{"AntBot still runs on a Mac", install{kernel: "Darwin", machine: "arm64", arm64: "1", files: mac,
			launchctl: launchd("antbot", "/Users/x/go/bin/antbot")},
			[]string{"AntBot (Mirrin's name before)", "Library/LaunchAgents/antbot.plist", "mirrin service install", "starts again at your next login"},
			[]string{"rm -f", "openHuman", "launchctl bootout"}}, // the plist may hold the only copy of its keys: mirrin moves them first
		{"AntBot still runs on Linux", install{kernel: "Linux", machine: "x86_64", files: linux,
			systemctl: systemd("antbot.service", "/home/x/go/bin/antbot")},
			[]string{"AntBot (Mirrin's name before)", ".config/systemd/user/antbot.service", "mirrin service install"},
			[]string{"rm -f", "openHuman", "disable --now"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			in.env = []string{"MIRRIN_NO_APP=1"}
			out, _, err := in.run(t)
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(out, w) {
					t.Errorf("output has %q:\n%s", w, out)
				}
			}
		})
	}
}

// Settings named before the rename (ANTBOT_*) still drive an install, and
// the MIRRIN_ name wins when both are set. Every setting install.sh
// documents reads its old name too.
func TestInstallHonoursTheOldSettings(t *testing.T) {
	needSh(t)
	files := map[string][]byte{"mirrin-linux-amd64-nowhatsapp": fakeBinary("mirrin-linux-amd64-nowhatsapp")}
	// releases/latest is down, so only the version asked for can install.
	down := map[string]int{"latest": http.StatusServiceUnavailable}
	old := filepath.Join(t.TempDir(), "old-bin")
	out, _, err := install{kernel: "Linux", machine: "x86_64", files: files, codes: down, env: []string{
		"MIRRIN_BIN_DIR=", "ANTBOT_BIN_DIR=" + old, "ANTBOT_BUILD=nowhatsapp", "ANTBOT_VERSION=" + testVersion}}.run(t)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(filepath.Join(old, "mirrin")); err != nil || !strings.Contains(string(got), "nowhatsapp") {
		t.Fatalf("not installed from the old settings (%v):\n%s", err, out)
	}

	both := filepath.Join(t.TempDir(), "new-bin")
	out, _, err = install{kernel: "Linux", machine: "x86_64", files: files, codes: down, env: []string{
		"MIRRIN_BIN_DIR=" + both, "ANTBOT_BIN_DIR=" + old + "-not", "MIRRIN_BUILD=nowhatsapp", "ANTBOT_BUILD=default",
		"MIRRIN_VERSION=" + testVersion, "ANTBOT_VERSION=v0.0.1"}}.run(t)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(both, "mirrin")); err != nil {
		t.Fatalf("MIRRIN_BIN_DIR lost to ANTBOT_BIN_DIR:\n%s", out)
	}

	src, err := os.ReadFile(repoFile("install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?m)^#   MIRRIN_([A-Z_]+)=`).FindAllStringSubmatch(string(src), -1) {
		n := m[1]
		if line := "MIRRIN_" + n + "=${MIRRIN_" + n + ":-${ANTBOT_" + n + ":-}}"; !strings.Contains(string(src), line) {
			t.Errorf("install.sh doesn't read ANTBOT_%s: no %q", n, line)
		}
	}
}

// An antbot from before the rename stays (a service from then may still run
// it) and is pointed out; another product's antbot isn't mentioned.
func TestInstallNotesAnOldAntbot(t *testing.T) {
	needSh(t)
	files := map[string][]byte{"mirrin-linux-amd64": fakeBinary("mirrin-linux-amd64")}
	for _, c := range []struct {
		name, answer string
		noted        bool
	}{
		{"AntBot's", "antbot v0.3.0-5-gabc1234", true},
		{"a source build", "antbot dev", true},
		{"another product's", "antbot 2.0 (ant colony simulator)", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, _, err := install{kernel: "Linux", machine: "x86_64", files: files,
				antbot: "[ \"$1\" = version ] && echo '" + c.answer + "'\n"}.run(t)
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			if noted := strings.Contains(out, "the old AntBot program is still at"); noted != c.noted {
				t.Fatalf("noted = %v, want %v:\n%s", noted, c.noted, out)
			}
			if strings.Contains(out, "rm ") {
				t.Fatalf("suggests deleting it now:\n%s", out)
			}
		})
	}
}

// TestShellProfile checks which startup file gets the PATH line. bash on a
// Mac reads only the first of .bash_profile, .bash_login and .profile, so
// creating .bash_profile would silently stop an existing .profile loading.
func TestShellProfile(t *testing.T) {
	needSh(t)
	src, err := os.ReadFile(repoFile("install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// The functions without the final `main "$@"`, so they can be called alone.
	body := strings.TrimRight(string(src), "\n")
	if !strings.HasSuffix(body, "\nmain \"$@\"") {
		t.Fatal(`install.sh no longer ends with main "$@"`)
	}
	lib := filepath.Join(t.TempDir(), "install-lib.sh")
	if err := os.WriteFile(lib, []byte(strings.TrimSuffix(body, `main "$@"`)), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, shell, os string
		exist           []string
		want            string
	}{
		{"zsh", "/bin/zsh", "darwin", nil, ".zshrc"},
		{"bash on Linux", "/bin/bash", "linux", []string{".profile"}, ".bashrc"},
		{"bash on a new Mac", "/bin/bash", "darwin", nil, ".bash_profile"},
		{"bash on a Mac with only .profile", "/bin/bash", "darwin", []string{".profile"}, ".profile"},
		{"bash on a Mac with .bash_login", "/bin/bash", "darwin", []string{".bash_login", ".profile"}, ".bash_login"},
		{"bash on a Mac with .bash_profile", "/bin/bash", "darwin", []string{".bash_profile", ".profile"}, ".bash_profile"},
		{"fish", "/opt/homebrew/bin/fish", "darwin", nil, ".config/fish/config.fish"},
		{"anything else", "/bin/ksh", "linux", nil, ".profile"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			for _, f := range c.exist {
				writeFile(t, filepath.Join(home, f), nil)
			}
			cmd := exec.Command("sh", "-c", `. "$1"; OS=$2; shell_profile "$3"`, "sh", lib, c.os, home)
			cmd.Env = append(os.Environ(), "SHELL="+c.shell, "ZDOTDIR=")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != filepath.Join(home, c.want) {
				t.Errorf("got %s, want ~/%s", got, c.want)
			}
		})
	}
}

func TestInstallTalksToTheUser(t *testing.T) {
	needSh(t)
	bin := fakeBinary("mirrin-darwin-arm64")
	files := map[string][]byte{"mirrin-darwin-arm64": bin, "SHA256SUMS.sigstore.json": []byte("{}")}
	out, _, err := install{kernel: "Darwin", machine: "arm64", arm64: "1", files: files,
		cosign: "exit 0\n"}.run(t)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	t.Logf("what the user sees:\n%s", out)
	for _, want := range []string{
		"installing Mirrin " + testVersion + " for darwin/arm64", // resolved through /releases/latest
		"signature verified with cosign",
		"this release has no Mirrin.app",
		"isn't on your PATH yet",
		"export PATH=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// GitHub's build attestation is an optional extra check: without gh, or
// with a gh that isn't signed in or can't verify, the install goes on with
// a note; a signed-in gh that verifies says so.
func TestInstallChecksTheAttestationWhenGhCan(t *testing.T) {
	needSh(t)
	for _, d := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if _, err := os.Stat(filepath.Join(d, "gh")); err == nil {
			t.Skipf("%s/gh exists; the test needs a PATH without gh", d)
		}
	}
	files := map[string][]byte{"mirrin-linux-amd64": fakeBinary("mirrin-linux-amd64")}
	cases := []struct {
		name, gh, want string
	}{
		{"no gh", "-", "with the GitHub CLI (gh) installed and signed in"},
		{"not signed in", "exit 1\n", "gh auth login"},
		{"verifies", `case "$1" in auth) exit 0 ;; attestation) [ "$2" = verify ] && [ -f "$3" ] && [ "$4" = -R ] && exit 0 ;; esac; exit 1` + "\n", "build attestation verified with gh"},
		{"doesn't verify", `[ "$1" = auth ] && exit 0; exit 1` + "\n", "gh couldn't verify GitHub's build attestation for mirrin-linux-amd64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, dest, err := install{kernel: "Linux", machine: "x86_64", files: files, gh: c.gh, pick: true}.run(t)
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("output lacks %q:\n%s", c.want, out)
			}
			if _, err := os.Stat(filepath.Join(dest, "mirrin")); err != nil {
				t.Fatalf("not installed: %v\n%s", err, out)
			}
		})
	}
}

// fakeHdiutil "mounts" a disk image holding Mirrin.app at the mountpoint it
// is given; the file commands stay guarded.
const fakeHdiutil = `case "$1" in
attach) while [ $# -gt 0 ]; do [ "$1" = -mountpoint ] && mnt=$2; shift; done
  mkdir -p "$mnt/Mirrin.app/Contents/MacOS" && echo app >"$mnt/Mirrin.app/Contents/MacOS/mirrin" ;;
esac
`

// A notarized Mirrin.app keeps macOS's quarantine flag, so Gatekeeper opens
// it normally and goes on checking it; the flag is only cleared from an app
// Gatekeeper wouldn't open, whose checksum matched. Gatekeeper's assessment
// rejects every bare command-line program, notarized or not, so the CLI's
// flag is cleared either way without calling it unnotarized.
func TestInstallClearsQuarantineOnlyWhenNotNotarized(t *testing.T) {
	needSh(t)
	dmg := "Mirrin-" + testVersion + "-macos.dmg"
	files := map[string][]byte{"mirrin-darwin-arm64": fakeBinary("mirrin-darwin-arm64"), dmg: []byte("disk image")}
	for _, notarized := range []bool{true, false} {
		apps := filepath.Join(t.TempDir(), "Applications")
		log := filepath.Join(t.TempDir(), "xattr.log")
		// Like the real spctl, it never accepts a command-line program.
		spctl := "exit 3\n"
		if notarized {
			spctl = "case \"$*\" in *.app) exit 0 ;; esac\nexit 3\n"
		}
		out, dest, err := install{kernel: "Darwin", machine: "arm64", arm64: "1", files: files, onPath: true,
			env:     []string{"MIRRIN_APPS_DIR=" + apps},
			hdiutil: fakeHdiutil,
			xattr:   fmt.Sprintf("case \"$1\" in -p) exit 0 ;; *) echo \"$*\" >>%s ;; esac\n", shellQuote(log)),
			spctl:   spctl}.run(t)
		if err != nil {
			t.Fatalf("install failed: %v\n%s", err, out)
		}
		b, _ := os.ReadFile(log)
		cli, app := filepath.Join(dest, "mirrin"), filepath.Join(apps, "Mirrin.app")
		if !strings.Contains(string(b), "-dr com.apple.quarantine "+cli) ||
			!strings.Contains(out, "clearing the macOS quarantine flag on "+cli+"; its checksum matched") ||
			strings.Contains(out, cli+" is notarized") || strings.Contains(out, cli+", which isn't notarized") {
			t.Errorf("notarized=%v: the CLI:\n%s", notarized, out)
		}
		appCleared := strings.Contains(string(b), "-dr com.apple.quarantine "+app)
		switch {
		case notarized && (appCleared || !strings.Contains(out, app+" is notarized by Apple, so it keeps its quarantine flag")):
			t.Errorf("notarized app: cleared=%v\n%s", appCleared, out)
		case !notarized && (!appCleared || !strings.Contains(out, "clearing the macOS quarantine flag on "+app+", which isn't notarized; its checksum matched")):
			t.Errorf("app that isn't notarized: cleared=%v\n%s", appCleared, out)
		}
	}
}

// On a Mac the installer adds Mirrin.app from the release's disk image, and
// says when the app isn't notarized, since macOS then asks for its
// permissions again after each update.
func TestInstallAddsTheMacApp(t *testing.T) {
	needSh(t)
	dmg := "Mirrin-" + testVersion + "-macos.dmg"
	files := map[string][]byte{"mirrin-darwin-arm64": fakeBinary("mirrin-darwin-arm64"), dmg: []byte("disk image")}
	for _, notarized := range []bool{true, false} {
		apps := filepath.Join(t.TempDir(), "Applications")
		spctl := "exit 3\n"
		if notarized {
			spctl = "exit 0\n"
		}
		out, _, err := install{kernel: "Darwin", machine: "arm64", arm64: "1", files: files, onPath: true,
			hdiutil: fakeHdiutil, spctl: spctl, xattr: "exit 1\n", env: []string{"MIRRIN_APPS_DIR=" + apps}}.run(t)
		if err != nil {
			t.Fatalf("install failed: %v\n%s", err, out)
		}
		if b, err := os.ReadFile(filepath.Join(apps, "Mirrin.app", "Contents", "MacOS", "mirrin")); err != nil || string(b) != "app\n" {
			t.Fatalf("Mirrin.app wasn't installed in %s (%v):\n%s", apps, err, out)
		}
		if !strings.Contains(out, "installed Mirrin.app in "+apps) {
			t.Errorf("output:\n%s", out)
		}
		if warned := strings.Contains(out, "isn't notarized by Apple yet"); warned == notarized {
			t.Errorf("notarized=%v, warned=%v:\n%s", notarized, warned, out)
		}
	}
}

func TestInstallPowerShell(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 is for Windows")
	}
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("no Windows PowerShell")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	asset := "mirrin-windows-" + runtime.GOARCH + ".exe"
	mitAsset := "mirrin-windows-" + runtime.GOARCH + "-nowhatsapp.exe"
	cases := []struct {
		name  string
		files map[string][]byte
		sums  string
		codes map[string]int
		iex   bool   // run as the README's one-liner does, not as a file
		want  string // in the output; the install must fail unless it is "is installed"
	}{
		{"MIT installs", map[string][]byte{mitAsset: bin}, "", nil, false, "Mirrin " + testVersion + " is installed"},
		{"MIT tampered", map[string][]byte{mitAsset: bin}, sum([]byte("other")) + "  " + mitAsset + "\n", nil, false, "doesn't match its published checksum"},
		{"MIT missing", map[string][]byte{asset: bin}, "", nil, false, "has no MIT (without WhatsApp) build for windows/" + runtime.GOARCH},
		{"installs", map[string][]byte{asset: bin}, "", nil, false, "Mirrin " + testVersion + " is installed"},
		{"tampered download", map[string][]byte{asset: bin}, sum([]byte("other")) + "  " + asset + "\n", nil, false, "doesn't match its published checksum"},
		{"no build for this PC", map[string][]byte{}, sum(bin) + "  mirrin-linux-amd64\n", nil, false, "has no build for windows/" + runtime.GOARCH},
		{"GitHub rate limits the build", map[string][]byte{asset: bin}, "", map[string]int{asset: 403}, false, "answered HTTP 403 for " + asset},
		// CI runs the one-liner with powershell -Command; a failed install
		// must fail the step, though a pasted one leaves the window open.
		{"one-liner from a command line", map[string][]byte{}, sum(bin) + "  mirrin-linux-amd64\n", nil, true, "has no build for windows/" + runtime.GOARCH},
		{"one-liner installs", map[string][]byte{asset: bin}, "", nil, true, "Mirrin " + testVersion + " is installed"},
		{"installs with the settings' old names", map[string][]byte{asset: bin}, "", nil, false, "Mirrin " + testVersion + " is installed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := release(t, c.files, c.sums, c.codes)
			dest := filepath.Join(t.TempDir(), "bin")
			cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", repoFile("install.ps1"))
			if c.iex {
				cmd = exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
					"Get-Content -Raw '"+strings.ReplaceAll(repoFile("install.ps1"), "'", "''")+"' | Invoke-Expression")
			}
			// The settings, under the names from before the rename (ANTBOT_)
			// for the case that says so.
			prefix := "MIRRIN_"
			if strings.Contains(c.name, "old names") {
				prefix = "ANTBOT_"
			}
			// Leave the normal install path unset, independent of the host.
			var cleanEnv []string
			for _, entry := range withoutOldSettings(os.Environ()) {
				key, _, _ := strings.Cut(entry, "=")
				if !strings.HasPrefix(strings.ToUpper(key), "MIRRIN_") {
					cleanEnv = append(cleanEnv, entry)
				}
			}
			cmd.Env = append(cleanEnv,
				prefix+"DOWNLOAD_URL="+srv.URL+"/releases",
				prefix+"BIN_DIR="+dest,
				prefix+"NO_MODIFY_PATH=1",
				"MIRRIN_FAKE_MIRRIN=1",
			)
			if strings.HasPrefix(c.name, "MIT") {
				cmd.Env = append(cmd.Env, "MIRRIN_BUILD=nowhatsapp")
			}
			out, err := cmd.CombinedOutput()
			installed := strings.HasSuffix(c.want, "is installed")
			if installed != (err == nil) || !strings.Contains(string(out), c.want) {
				t.Fatalf("err=%v, want %q in:\n%s", err, c.want, out)
			}
			_, statErr := os.Stat(filepath.Join(dest, "mirrin.exe"))
			if installed != (statErr == nil) {
				t.Errorf("mirrin.exe present = %v, want %v", statErr == nil, installed)
			}
		})
	}
}

func TestReleaseBrew(t *testing.T) {
	needSh(t)
	assets := []string{"mirrin-darwin-arm64", "mirrin-darwin-amd64", "mirrin-linux-arm64", "mirrin-linux-amd64"}
	var full strings.Builder
	for _, a := range assets {
		fmt.Fprintf(&full, "%s  %s\n", sum([]byte(a)), a)
	}
	cases := []struct {
		name string
		sums string
		want []string // in stdout; nil means the script must fail
	}{
		{"every asset", full.String(), []string{`version "9.9.9"`, sum([]byte("mirrin-darwin-amd64")), "download/v9.9.9/mirrin-linux-arm64"}},
		// The old script hashed the 404 page's empty body instead of stopping.
		{"Intel Mac build missing", strings.ReplaceAll(full.String(), sum([]byte("mirrin-darwin-amd64"))+"  mirrin-darwin-amd64\n", ""), nil},
		{"empty-file hash", strings.ReplaceAll(full.String(), sum([]byte("mirrin-linux-amd64")), sum(nil)), nil},
		// SUMS relative to where the script is run, as a maintainer types it.
		{"relative SUMS", full.String(), []string{sum([]byte("mirrin-linux-arm64"))}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			f := filepath.Join(dir, "SHA256SUMS")
			if err := os.WriteFile(f, []byte(c.sums), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", repoFile("scripts/release-brew.sh"))
			cmd.Env = append(os.Environ(), "VERSION="+testVersion, "SUMS="+f)
			if c.name == "relative SUMS" {
				cmd.Dir = dir
				cmd.Env = append(cmd.Env, "SUMS=SHA256SUMS")
			}
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if c.want == nil {
				if err == nil || stdout.Len() > 0 {
					t.Fatalf("want a failure and no formula, got err=%v:\n%s", err, stdout.String())
				}
				if !strings.Contains(stderr.String(), "no usable checksum") {
					t.Errorf("unhelpful error: %s", stderr.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
			for _, w := range c.want {
				if !strings.Contains(stdout.String(), w) {
					t.Errorf("formula lacks %q", w)
				}
			}
			if strings.Contains(stdout.String(), "SHA_") || strings.Contains(stdout.String(), `"VERSION"`) {
				t.Errorf("placeholders left in the formula:\n%s", stdout.String())
			}
		})
	}
}

func TestReleaseNotes(t *testing.T) {
	needSh(t)
	changelog := filepath.Join(t.TempDir(), "CHANGELOG.md")
	body := "# Changelog\n\n## Unreleased\n\n## 9.9.9 — 2026-10-01\n\n### Fixed\n- The thing.\n\n## 9.9.8 — 2026-09-01\n- Older.\n"
	if err := os.WriteFile(changelog, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	attested := "1"
	notes := func(tag string) string {
		cmd := exec.Command("sh", repoFile("scripts/release-notes.sh"), tag)
		cmd.Env = append(os.Environ(), "CHANGELOG="+changelog, "ATTESTED="+attested)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return string(out)
	}
	got := notes(testVersion)
	if !strings.Contains(got, "- The thing.") || strings.Contains(got, "Older") {
		t.Errorf("wrong section:\n%s", got)
	}
	if !strings.Contains(got, "release.yml@refs/tags/"+testVersion) || !strings.Contains(got, "install.ps1") {
		t.Errorf("no install and verify instructions:\n%s", got)
	}
	for _, want := range []string{"as a whole under GPL-3.0", "MIT builds without WhatsApp", "MIRRIN_BUILD=nowhatsapp", "$env:MIRRIN_BUILD", "mirrin-relay-linux-arm64", "gh attestation verify", "Mirrin-" + testVersion + "-sbom.spdx.json", "THIRD_PARTY_NOTICES.txt", "Mirrin-" + testVersion + "-source.tar.gz", "blob/" + testVersion + "/docs/licensing.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("notes lack %q:\n%s", want, got)
		}
	}
	if got := notes("v1.0.0"); !strings.Contains(got, "See CHANGELOG.md") {
		t.Errorf("no fallback for a version without a section:\n%s", got)
	}
	// A private repository gets no attestations, so the notes don't promise any.
	attested = ""
	if got := notes(testVersion); strings.Contains(got, "attestation") || !strings.Contains(got, "sbom.spdx.json") {
		t.Errorf("notes without attestations:\n%s", got)
	}
}

// TestIssueTemplateLabelsExist checks that every label an issue template
// applies is in .github/labels.yml, which the labels workflow creates on
// GitHub. The protocol-request template once applied a label that didn't
// exist, so protocol requests arrived unlabelled.
func TestIssueTemplateLabelsExist(t *testing.T) {
	b, err := os.ReadFile(repoFile(".github/labels.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var labels []struct{ Name, Color, Description string }
	if err := yaml.Unmarshal(b, &labels); err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	hex := regexp.MustCompile(`^[0-9a-f]{6}$`)
	for _, l := range labels {
		if l.Name == "" || !hex.MatchString(l.Color) || l.Description == "" {
			t.Errorf("label %+v needs a name, a six-digit lowercase hex color and a description", l)
		}
		defined[l.Name] = true
	}

	templates, err := filepath.Glob(repoFile(".github/ISSUE_TEMPLATE/*"))
	if err != nil || len(templates) == 0 {
		t.Fatalf("no issue templates found: %v", err)
	}
	for _, path := range templates {
		name := filepath.Base(path)
		if name == "config.yml" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if strings.HasSuffix(name, ".md") { // YAML front matter between --- lines
			parts := strings.SplitN(src, "---", 3)
			if len(parts) < 3 {
				t.Errorf("%s: no front matter", name)
				continue
			}
			src = parts[1]
		}
		var meta struct {
			Labels yaml.Node `yaml:"labels"`
		}
		if err := yaml.Unmarshal([]byte(src), &meta); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var used []string
		switch meta.Labels.Kind {
		case yaml.ScalarNode: // labels: a, b
			for _, l := range strings.Split(meta.Labels.Value, ",") {
				used = append(used, strings.TrimSpace(l))
			}
		case yaml.SequenceNode:
			for _, n := range meta.Labels.Content {
				used = append(used, n.Value)
			}
		}
		for _, l := range used {
			if l != "" && !defined[l] {
				t.Errorf("%s applies the label %q, which isn't in .github/labels.yml", name, l)
			}
		}
	}
}

// TestPackTemplateLintWorkflow runs the pack template's lint step the way
// GitHub would, with a freshly built mirrin where the install step puts it.
// The old step wrote a one-line ~/.mirrin/config.yaml that failed validation
// (WhatsApp on with no owner), so every new pack's first CI run was red.
func TestPackTemplateLintWorkflow(t *testing.T) {
	needSh(t)
	if testing.Short() {
		t.Skip("builds mirrin")
	}
	b, err := os.ReadFile(repoFile("examples/pack-template/.github/workflows/lint.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Run   string            `yaml:"run"`
				Shell string            `yaml:"shell"`
				Env   map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	var lint, binDir string
	for _, s := range wf.Jobs["lint"].Steps {
		switch {
		case strings.Contains(s.Run, "install.sh"):
			binDir = s.Env["MIRRIN_BIN_DIR"]
			// GitHub's default shell has no pipefail: a failed download
			// would pipe nothing into sh, pass, and leave the next step
			// saying "mirrin: command not found".
			if strings.Contains(s.Run, "|") && s.Shell != "bash" {
				t.Errorf("the install step pipes into sh without shell: bash, so a failed download passes:\n%s", s.Run)
			}
		case strings.Contains(s.Run, "protocols lint"):
			lint = s.Run
		}
	}
	if lint == "" {
		t.Fatal("no lint step in lint.yml")
	}
	if binDir != "${{ runner.temp }}/bin" {
		t.Fatalf("the install step puts mirrin in %q; the lint step looks in $RUNNER_TEMP/bin", binDir)
	}
	// Checked before anything runs: the step must never reach a real twin.
	for _, bad := range []string{"~/", "$HOME", "${HOME}", "go run"} {
		if strings.Contains(lint, bad) {
			t.Fatalf("the lint step uses %q; it has to work with the released binary on a throwaway MIRRIN_HOME:\n%s", bad, lint)
		}
	}

	runner := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(runner, "bin", "mirrin"), "./cmd/mirrin")
	build.Dir = repoFile("")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mirrin: %v\n%s", err, out)
	}
	step := func(pack string) (string, error) {
		cmd := exec.Command("sh", "-e", "-c", lint)
		cmd.Dir = pack
		// Only what a runner has: no API keys, no MIRRIN_HOME from outside.
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "RUNNER_TEMP=" + runner, "TMPDIR=" + os.TempDir()}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	pack := t.TempDir()
	if err := os.CopyFS(pack, os.DirFS(repoFile("examples/pack-template"))); err != nil {
		t.Fatal(err)
	}
	if out, err := step(pack); err != nil {
		t.Fatalf("the template's lint step fails on the template itself: %v\n%s", err, out)
	}
	// It does catch a broken protocol.
	broken := "name: Broken\nschedule: \"every tuesday-ish\"\n"
	if err := os.WriteFile(filepath.Join(pack, "protocols", "broken.yaml"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := step(pack); err == nil {
		t.Fatalf("lint passed a protocol with no prompt and a bad schedule:\n%s", out)
	}
}
