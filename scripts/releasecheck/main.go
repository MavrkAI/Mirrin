// Command releasecheck is the last gate before a release goes out: every
// binary must be what its name says (a Linux build once shipped as
// mirrin-darwin-x86_64), every platform the installers ask for must be there,
// and SHA256SUMS covers what is published.
//
//	go run ./scripts/releasecheck [-require linux/amd64,...] [-sums] dist
package main

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Everything a full release ships, as install.sh, install.ps1 and the
// Homebrew formula ask for it, and the voice bundles voice setup downloads.
const allPlatforms = "linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64,windows/arm64," +
	"linux/amd64/nowhatsapp,linux/arm64/nowhatsapp,darwin/amd64/nowhatsapp,darwin/arm64/nowhatsapp," +
	"windows/amd64/nowhatsapp,windows/arm64/nowhatsapp,relay/linux/amd64,relay/linux/arm64," + voicePlatforms

// SumsFile is the checksum list the installers verify against.
const SumsFile = "SHA256SUMS"

// assetName matches published binaries: mirrin-<os>-<arch>[-nowhatsapp][.exe], in Go's
// names for both.
var assetName = regexp.MustCompile(`^mirrin-(linux|darwin|windows)-(amd64|arm64|universal)(-nowhatsapp)?(\.exe)?$`)

var relayName = regexp.MustCompile(`^mirrin-relay-linux-(amd64|arm64)$`)

// packageName matches the other files a release carries: the macOS disk image,
// the Windows setup program, the source with every dependency (what the GPL
// asks to go with a binary that links GPL code) and the SBOM.
var packageName = regexp.MustCompile(`^Mirrin-v?[0-9][0-9A-Za-z.+-]*-(macos\.dmg|windows-setup\.exe|source\.tar\.gz|sbom\.spdx\.json)$`)

// NoticesFile lists the third-party code in the binaries, with its licences.
const NoticesFile = "THIRD_PARTY_NOTICES.txt"

func main() {
	require := flag.String("require", allPlatforms, "os/arch, os/arch/nowhatsapp, relay/linux/arch or voice/os/arch builds that must be present, comma or space separated (empty for none)")
	sums := flag.Bool("sums", false, "write "+SumsFile+" for every file in the directory")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: releasecheck [-require os/arch,...] [-sums] <dir>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	dir := flag.Arg(0)
	want := strings.FieldsFunc(*require, func(r rune) bool { return r == ',' || r == ' ' })
	problems, err := check(dir, want)
	if err != nil {
		fmt.Fprintln(os.Stderr, "releasecheck:", err)
		os.Exit(1)
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "  "+p)
		}
		fmt.Fprintf(os.Stderr, "releasecheck: %d problem(s) in %s. Fix them before anything is published.\n", len(problems), dir)
		os.Exit(1)
	}
	fmt.Printf("releasecheck: every binary in %s matches its name\n", dir)
	if *sums {
		n, err := writeSums(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "releasecheck:", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s (%d files)\n", filepath.Join(dir, SumsFile), n)
	}
}

// check returns one line per binary whose contents don't match its name, per
// file that isn't part of a release (a leftover would be summed and published),
// and per required os/arch that has no binary.
func check(dir string, require []string) ([]string, error) {
	return checkWithBuildInfo(dir, require, buildinfo.ReadFile)
}

func checkWithBuildInfo(dir string, require []string, readInfo func(string) (*buildinfo.BuildInfo, error)) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var problems []string
	have := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		// Folders (Mirrin.app) and dotfiles are never published.
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasPrefix(name, "mirrin-") {
			if !packageName.MatchString(name) && name != SumsFile && name != NoticesFile && !strings.HasPrefix(name, SumsFile+".") {
				problems = append(problems, name+": not a release file (left from an older build?); remove it or run make clean")
			}
			continue
		}
		if m := voiceName.FindStringSubmatch(name); m != nil {
			want := m[1] + "/" + m[2]
			if err := checkVoice(filepath.Join(dir, name), want); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			} else {
				have["voice/"+want] = true
			}
			continue
		}
		if m := relayName.FindStringSubmatch(name); m != nil {
			want := "linux/" + m[1]
			got, err := platform(filepath.Join(dir, name))
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			} else if got != want {
				problems = append(problems, fmt.Sprintf("%s: is a %s binary, not %s", name, got, want))
			} else if err := checkBuildFile(filepath.Join(dir, name), "relay", readInfo); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			} else {
				have["relay/"+want] = true
			}
			continue
		}
		m := assetName.FindStringSubmatch(name)
		if m == nil {
			problems = append(problems, name+": not a release name (mirrin-<linux|darwin|windows>-<amd64|arm64>[-nowhatsapp], plus .exe on Windows; or mirrin-relay-linux-<amd64|arm64>)")
			continue
		}
		goos, goarch, exe := m[1], m[2], m[4] != ""
		want := goos + "/" + goarch
		if goarch == "universal" {
			want = goos + "/amd64+arm64"
		}
		if (goos == "windows") != exe {
			problems = append(problems, name+": only Windows builds end in .exe, and they always do")
			continue
		}
		got, err := platform(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if got != want {
			problems = append(problems, fmt.Sprintf("%s: is a %s binary, not %s", name, got, want))
			continue
		}
		variant := strings.TrimPrefix(m[3], "-")
		if err := checkBuildFile(filepath.Join(dir, name), variant, readInfo); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if m[3] != "" {
			want += "/nowhatsapp"
		}
		have[want] = true
	}
	for _, r := range require {
		if !have[r] {
			parts := strings.Split(r, "/")
			goos, goarch, variant := parts[0], "", ""
			if len(parts) > 1 {
				goarch = parts[1]
			}
			if len(parts) > 2 {
				variant = parts[2]
			}
			if goos == "relay" || goos == "voice" {
				goos, goarch, variant = goarch, variant, goos
			}
			name := assetFor(goos, goarch, variant)
			problems = append(problems, fmt.Sprintf("missing %s: no %s build", name, r))
		}
	}
	return problems, nil
}

// assetFor is the published file name for a build.
func assetFor(goos, goarch, variant string) string {
	prefix, suffix := "mirrin-", ""
	switch {
	case variant == "voice":
		return "mirrin-voice-" + goos + "-" + goarch + ".tar.gz"
	case variant == "relay":
		prefix = "mirrin-relay-"
	case variant != "":
		suffix = "-" + variant
	}
	name := prefix + goos + "-" + goarch + suffix
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// platform reads an executable's header and names its os/arch the way Go
// does. A macOS universal binary reports darwin/amd64+arm64.
func platform(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return "", errors.New("not an executable (too short)")
	}
	switch {
	case bytes.Equal(magic[:], []byte("\x7fELF")):
		ef, err := elf.NewFile(f)
		if err != nil {
			return "", err
		}
		return "linux/" + elfArch(ef.Machine), nil
	case magic[0] == 'M' && magic[1] == 'Z':
		pf, err := pe.NewFile(f)
		if err != nil {
			return "", err
		}
		switch pf.Machine {
		case pe.IMAGE_FILE_MACHINE_AMD64:
			return "windows/amd64", nil
		case pe.IMAGE_FILE_MACHINE_ARM64:
			return "windows/arm64", nil
		}
		return fmt.Sprintf("windows/machine-%#x", pf.Machine), nil
	}
	if ff, err := macho.NewFatFile(f); err == nil {
		var arches []string
		for _, a := range ff.Arches {
			arches = append(arches, machoArch(a.Cpu))
		}
		sort.Strings(arches)
		return "darwin/" + strings.Join(arches, "+"), nil
	}
	mf, err := macho.NewFile(f)
	if err != nil {
		return "", errors.New("not an executable (no ELF, Mach-O or PE header)")
	}
	return "darwin/" + machoArch(mf.Cpu), nil
}

func elfArch(m elf.Machine) string {
	switch m {
	case elf.EM_X86_64:
		return "amd64"
	case elf.EM_AARCH64:
		return "arm64"
	}
	return strings.ToLower(strings.TrimPrefix(m.String(), "EM_"))
}

func machoArch(c macho.Cpu) string {
	switch c {
	case macho.CpuAmd64:
		return "amd64"
	case macho.CpuArm64:
		return "arm64"
	}
	return strings.ToLower(strings.TrimPrefix(c.String(), "Cpu"))
}

// writeSums writes SHA256SUMS in the format `sha256sum -c`, `shasum -a 256 -c`
// and the installers read. Signatures of the list itself are left out.
func writeSums(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var b strings.Builder
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == SumsFile || strings.HasPrefix(name, SumsFile+".") || strings.HasPrefix(name, ".") {
			continue
		}
		sum, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("nothing to sum in %s", dir)
	}
	return n, os.WriteFile(filepath.Join(dir, SumsFile), []byte(b.String()), 0o644)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
