package tailscale

import (
	"errors"
	"os/exec"

	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStatus(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, name := range []string{"macos", "linux"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			b, e := os.ReadFile("testdata/status-" + name + ".json")
			if e != nil {
				t.Fatal(e)
			}
			os.WriteFile(filepath.Join(dir, "status.json"), b, 0600)
			path, _ := filepath.Abs("../tlsmgr/testdata/fake-tailscale.sh")
			s, e := (&CLI{Path: path, Dir: dir}).Status(context.Background())
			if e != nil || len(s.TailscaleIPs) == 0 || strings.HasSuffix(s.Self.DNSName, ".") {
				t.Fatalf("%+v %v", s, e)
			}
		})
	}
}
func TestHealthFixes(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	_, err := (&CLI{Path: filepath.Join(t.TempDir(), "missing")}).Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), AdminURL) || !strings.Contains(err.Error(), "install") {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"BackendState":"Running","Self":{"DNSName":"x.ts.net."}}`), 0600)
	path, _ := filepath.Abs("../tlsmgr/testdata/fake-tailscale.sh")
	_, err = (&CLI{Path: path, Dir: dir}).Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), AdminURL) || !strings.Contains(err.Error(), "enable HTTPS") {
		t.Fatal(err)
	}
}

func TestMacCLIPathAndFriendlyErrors(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	file := filepath.Join(t.TempDir(), "Tailscale")
	os.WriteFile(file, nil, 0700)
	info, _ := os.Stat(file)
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	got, err := cliPath("", "darwin", missing, func(path string) (os.FileInfo, error) {
		if path != "/Applications/Tailscale.app/Contents/MacOS/Tailscale" {
			t.Fatal(path)
		}
		return info, nil
	})
	if err != nil || got != "/Applications/Tailscale.app/Contents/MacOS/Tailscale" {
		t.Fatal(got, err)
	}
	if got, err = cliPath("custom", "darwin", missing, os.Stat); err != nil || got != "custom" {
		t.Fatal(got, err)
	}
	_, err = (&CLI{Path: filepath.Join(t.TempDir(), "absent")}).Status(context.Background())
	if err == nil || strings.Contains(err.Error(), "fork/exec") || strings.Contains(err.Error(), "no such file") {
		t.Fatal(err)
	}
	var p *Problem
	if !errors.As(err, &p) || p.Cause == nil {
		t.Fatal("diagnostic not retained", err)
	}
}
