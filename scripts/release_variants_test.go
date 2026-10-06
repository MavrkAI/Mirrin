package scripts

import (
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Exercise the real installer without sockets: curl reads a local fake release.
func TestInstallMITLocalRelease(t *testing.T) {
	needSh(t)
	for _, kernel := range []string{"Linux", "Darwin"} {
		for _, mode := range []string{"ok", "mixed-case", "app-service", "tampered", "missing", "invalid"} {
			t.Run(kernel+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				dest := filepath.Join(root, "bin")
				release := filepath.Join(root, "release")
				if err := os.Mkdir(release, 0700); err != nil {
					t.Fatal(err)
				}
				launchctl := ""
				if mode == "app-service" {
					launchctl = "echo '/Applications/Mirrin.app/Contents/MacOS/mirrin'"
				}
				tools := fakeTools(t, install{launchctl: launchctl, kernel: kernel, machine: "arm64", arm64: "1", xattr: "exit 0", spctl: "exit 0", hdiutil: "exit 99"})
				write := func(path string, b []byte) {
					t.Helper()
					if err := os.WriteFile(path, b, 0700); err != nil {
						t.Fatal(err)
					}
				}
				asset := "mirrin-" + strings.ToLower(kernel) + "-arm64-nowhatsapp"
				bin := fakeBinary(asset)
				write(filepath.Join(release, asset), bin)
				sums := sum(bin) + "  " + asset + "\n"
				if mode == "tampered" {
					write(filepath.Join(release, asset), []byte("tampered"))
				}
				if mode == "missing" {
					if err := os.Remove(filepath.Join(release, asset)); err != nil {
						t.Fatal(err)
					}
				}
				// If the MIT path tries to fetch the GPL app, curl fails and the log exposes it.
				sums += sum([]byte("dmg")) + "  Mirrin-" + testVersion + "-macos.dmg\n"
				write(filepath.Join(release, "SHA256SUMS"), []byte(sums))
				write(filepath.Join(tools, "curl"), []byte(`#!/bin/sh
while [ "$#" -gt 0 ]; do
 case "$1" in
  -o) out=$2; shift 2 ;;
  -w|--retry) shift 2 ;;
  -*) shift ;;
  *) url=$1; shift ;;
 esac
done
name=${url##*/}
printf '%s\n' "$name" >> "$REQUEST_LOG"
if [ ! -f "$FAKE_RELEASE/$name" ]; then printf 404; exit 22; fi
cp "$FAKE_RELEASE/$name" "$out" || exit 1
printf 200
`))
				build := "nowhatsapp"
				if mode == "mixed-case" {
					build = "NoWhatsApp"
				}
				success := mode == "ok" || mode == "mixed-case" || mode == "app-service"
				if mode == "invalid" {
					build = "typo"
				}
				cmd := exec.Command("sh", repoFile("install.sh"))
				cmd.Env = append(os.Environ(), "PATH="+tools+":"+dest+":"+systemPath(t), "MIRRIN_BUILD="+build,
					"MIRRIN_VERSION="+testVersion, "MIRRIN_DOWNLOAD_URL=https://release.invalid/releases", "MIRRIN_BIN_DIR="+dest,
					"MIRRIN_NO_MODIFY_PATH=1", "MIRRIN_NO_APP=", "SHELL=/bin/sh", "FAKE_RELEASE="+release, "REQUEST_LOG="+filepath.Join(root, "requests"))
				out, err := cmd.CombinedOutput()
				if (err == nil) != success {
					t.Fatalf("err=%v: %s", err, out)
				}
				if mode == "missing" && !strings.Contains(string(out), "no MIT (without WhatsApp) build") {
					t.Fatalf("missing MIT guidance: %s", out)
				}
				if mode == "app-service" && kernel == "Darwin" {
					if strings.Contains(string(out), "restart it to use") || !strings.Contains(string(out), "service still runs Mirrin.app with WhatsApp") || !strings.Contains(string(out), "service uninstall") {
						t.Fatalf("wrong service guidance: %s", out)
					}
				}
				installed, readErr := os.ReadFile(filepath.Join(dest, "mirrin"))
				if success && (readErr != nil || string(installed) != string(bin)) {
					t.Fatalf("wrong installed build: %v: %s", readErr, installed)
				}
				if !success && readErr == nil {
					t.Fatal("installed despite failed verification")
				}
				requests, _ := os.ReadFile(filepath.Join(root, "requests"))
				if strings.Contains(string(requests), ".dmg") {
					t.Fatalf("MIT install downloaded GPL app: %s", requests)
				}
				if mode == "invalid" && len(requests) != 0 {
					t.Fatalf("invalid selection contacted release: %s", requests)
				}
			})
		}
	}
}

func TestReleaseBuildCommands(t *testing.T) {
	needSh(t)
	// make -n expands the real recipes without compiling or signing anything.
	cmd := exec.Command("make", "-n", "dist-portable", "dist-darwin", "relay-release", "UNAME=Darwin", "VERSION=v1.2.3")
	cmd.Dir = repoFile(".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, want := range []string{"linux/amd64 linux/arm64 windows/amd64 windows/arm64", "-tags nowhatsapp -o dist/mirrin-$os-$arch-nowhatsapp$ext", "-tags nowhatsapp -o dist/mirrin-darwin-$arch-nowhatsapp", "-o dist/mirrin-relay-linux-$arch ./cmd/mirrin-relay || exit 1"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing build command %q: %s", want, out)
		}
	}
	if strings.Contains(string(out), "mirrin-relay-SHA256SUMS") {
		t.Fatal("relay writes a second checksum manifest")
	}
}

// The "Hey Maverick" wake-word model isn't cleared for commercial use. The
// public repository and the source archives are made with `git archive`,
// which leaves out what .gitattributes marks export-ignore
// (docs/maintainers-release.md); without the mark, both would carry it.
func TestGitArchiveLeavesOutTheWakeModel(t *testing.T) {
	cmd := exec.Command("git", "check-attr", "export-ignore", "--", "internal/channels/voice/assets/hey_maverick.onnx")
	cmd.Dir = repoFile(".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("not a git checkout (%v): %s", err, out)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(out)), ": export-ignore: set") {
		t.Fatalf("git archive would carry the wake-word model: %s", out)
	}
}

func TestWorkflowActionsPinned(t *testing.T) {
	files, err := filepath.Glob(repoFile(".github/workflows/*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("workflows: %v", err)
	}
	uses := regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*([^\s#]+)`)
	pinned := regexp.MustCompile(`@[0-9a-f]{40}$`)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range uses.FindAllStringSubmatch(string(b), -1) {
			if !strings.HasPrefix(m[1], "./") && !pinned.MatchString(m[1]) {
				t.Errorf("%s: unpinned action %s", file, m[1])
			}
		}
	}
}

func TestReleaseIncludesVariantsAndRelay(t *testing.T) {
	for file, wants := range map[string][]string{
		".github/workflows/release.yml": {"make dist-portable relay-release", "dist/mirrin-darwin-amd64-nowhatsapp", "dist/mirrin-darwin-arm64-nowhatsapp", "./scripts/releasecheck -sums dist"},
		"scripts/release-macos.sh":      {"for f in dist/mirrin-darwin-amd64 dist/mirrin-darwin-arm64 dist/mirrin-darwin-amd64-nowhatsapp dist/mirrin-darwin-arm64-nowhatsapp;", `cp dist/mirrin-darwin-amd64 dist/mirrin-darwin-arm64 dist/mirrin-darwin-amd64-nowhatsapp dist/mirrin-darwin-arm64-nowhatsapp "$TMP/submit/"`},
	} {
		b, err := os.ReadFile(repoFile(file))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s lacks %s", file, want)
			}
		}
	}
}

func TestReleaseBrewDefaultLicence(t *testing.T) {
	needSh(t)
	sums := filepath.Join(t.TempDir(), "SHA256SUMS")
	var lines strings.Builder
	for _, platform := range []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"} {
		// Put the MIT row first so a prefix match picks the wrong hash.
		lines.WriteString(sum([]byte(platform+"-nowhatsapp")) + "  mirrin-" + platform + "-nowhatsapp\n")
		lines.WriteString(sum([]byte(platform)) + "  mirrin-" + platform + "\n")
	}
	if err := os.WriteFile(sums, []byte(lines.String()), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", repoFile("scripts/release-brew.sh"))
	cmd.Env = append(os.Environ(), "VERSION="+testVersion, "SUMS="+sums)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, platform := range []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"} {
		if !strings.Contains(string(out), sum([]byte(platform))) || strings.Contains(string(out), sum([]byte(platform+"-nowhatsapp"))) {
			t.Fatalf("formula uses the wrong variant's hash: %s", out)
		}
	}
	if !strings.Contains(string(out), `license "GPL-3.0-only"`) {
		t.Fatalf("default binary labelled with wrong licence: %s", out)
	}
}

func TestMITSmokeStep(t *testing.T) {
	b, err := os.ReadFile(repoFile(".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name  string            `yaml:"name"`
				If    string            `yaml:"if"`
				Shell string            `yaml:"shell"`
				Env   map[string]string `yaml:"env"`
				Run   string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	for _, step := range wf.Jobs["smoke"].Steps {
		if step.Name != "Install and verify MIT build" {
			continue
		}
		if step.Env["MIRRIN_BUILD"] != "nowhatsapp" || step.Shell != "bash" || step.If != "matrix.name == 'Linux x64'" {
			t.Fatalf("wrong MIT smoke settings: %+v", step)
		}
		for _, want := range []string{"sh install.sh", "signature verified", "version | grep", "go version -m", "go.mau.fi/", "exit 1"} {
			if !strings.Contains(step.Run, want) {
				t.Errorf("MIT smoke step lacks %s", want)
			}
		}
		return
	}
	t.Fatal("no published MIT smoke step")
}

func TestRelayGuideUsesReleaseChecksums(t *testing.T) {
	b, err := os.ReadFile(repoFile("docs/relay-selfhost.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "mirrin-relay-SHA256SUMS") || !strings.Contains(string(b), "sha256sum --check --ignore-missing SHA256SUMS") {
		t.Fatal("relay guide does not use the published manifest")
	}
}
