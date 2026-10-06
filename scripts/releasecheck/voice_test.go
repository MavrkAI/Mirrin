package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeBundle writes a voice bundle holding files (name → contents, mode 0755).
func writeBundle(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for name, b := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []interface{ Close() error }{tw, zw, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A full release needs the voice bundle for each Mac and Linux machine.
func TestReleaseRequiresTheVoiceBundles(t *testing.T) {
	problems, err := checkFixtures(t.TempDir(), strings.Split(allPlatforms, ","))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(problems, "\n")
	for _, want := range []string{"mirrin-voice-darwin-arm64.tar.gz", "mirrin-voice-darwin-amd64.tar.gz", "mirrin-voice-linux-amd64.tar.gz", "mirrin-voice-linux-arm64.tar.gz"} {
		if !strings.Contains(got, "missing "+want) {
			t.Errorf("want missing %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "mirrin-voice-windows") {
		t.Errorf("Windows has no voice bundle:\n%s", got)
	}
}

func TestVoiceBundleContents(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("voice bundles are for macOS and Linux")
	}
	bin := hostBinary(t)
	host := runtime.GOOS + "/" + runtime.GOARCH
	otherArch := map[string]string{"amd64": "arm64", "arm64": "amd64"}[runtime.GOARCH]
	name := "mirrin-voice-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	cases := []struct {
		name, asset string
		files       map[string][]byte
		want        string
	}{
		{"both programs", name, map[string][]byte{"mirrin-voice/whisper-server": bin, "mirrin-voice/sox": bin, "mirrin-voice/SOURCES.txt": []byte("x")}, ""},
		{"no sox", name, map[string][]byte{"mirrin-voice/whisper-server": bin}, "has no mirrin-voice/sox"},
		{"not a program", name, map[string][]byte{"mirrin-voice/whisper-server": bin, "mirrin-voice/sox": []byte("#!/bin/sh")}, "not an executable"},
		{"another machine's", "mirrin-voice-" + runtime.GOOS + "-" + otherArch + ".tar.gz", map[string][]byte{"mirrin-voice/whisper-server": bin, "mirrin-voice/sox": bin}, "program, not " + runtime.GOOS + "/" + otherArch},
		{"not a Windows thing", "mirrin-voice-windows-amd64.tar.gz", map[string][]byte{"mirrin-voice/sox": bin}, "not a release name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBundle(t, filepath.Join(dir, c.asset), c.files)
			problems, err := checkFixtures(dir, []string{"voice/" + host})
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(problems, "\n")
			if c.want == "" && got != "" {
				t.Fatalf("unexpected problems:\n%s", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("want %q in:\n%s", c.want, got)
			}
		})
	}
	// Not a tar.gz at all.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("<html>404</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, _ := checkFixtures(dir, nil); len(p) != 1 || !strings.Contains(p[0], "not a .tar.gz") {
		t.Fatalf("problems: %v", p)
	}
}
