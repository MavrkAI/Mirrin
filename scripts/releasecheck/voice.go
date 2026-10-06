package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// voiceName matches the voice bundles (packaging/voice/build.sh): whisper-server
// and sox for one-click voice setup, for macOS and Linux.
var voiceName = regexp.MustCompile(`^mirrin-voice-(linux|darwin)-(amd64|arm64)\.tar\.gz$`)

// voicePrograms are what every voice bundle must hold, in its mirrin-voice
// folder.
var voicePrograms = []string{"whisper-server", "sox"}

// voicePlatforms are the bundles a full release carries.
const voicePlatforms = "voice/linux/amd64,voice/linux/arm64,voice/darwin/amd64,voice/darwin/arm64"

// checkVoice checks that a voice bundle holds each program, built for want
// (os/arch).
func checkVoice(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a .tar.gz: %v", err)
	}
	tmp, err := os.MkdirTemp("", "releasecheck-voice-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	found := map[string]bool{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("not a .tar.gz: %v", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		prog, ok := strings.CutPrefix(name, "mirrin-voice/")
		if !ok || h.Typeflag != tar.TypeReg || !contains(voicePrograms, prog) {
			continue
		}
		if h.Mode&0o111 == 0 {
			return fmt.Errorf("%s isn't executable", name)
		}
		out := filepath.Join(tmp, prog)
		w, err := os.Create(out)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, tr)
		w.Close()
		if err != nil {
			return err
		}
		got, err := platform(out)
		if err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		if got != want {
			return fmt.Errorf("%s is a %s program, not %s", name, got, want)
		}
		found[prog] = true
	}
	for _, p := range voicePrograms {
		if !found[p] {
			return fmt.Errorf("has no mirrin-voice/%s", p)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
