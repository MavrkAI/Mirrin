package main

import (
	"debug/buildinfo"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func validBuildInfo(variant string) *buildinfo.BuildInfo {
	info := &buildinfo.BuildInfo{Path: commandPath}
	switch variant {
	case "relay":
		info.Path += "-relay"
	case "nowhatsapp":
		info.Settings = []debug.BuildSetting{{Key: "-tags", Value: "nowhatsapp"}}
	default:
		info.Deps = []*debug.Module{{Path: "go.mau.fi/libsignal"}}
	}
	return info
}

func checkFixtures(dir string, require []string) ([]string, error) {
	return checkWithBuildInfo(dir, require, func(path string) (*buildinfo.BuildInfo, error) {
		variant := ""
		if strings.HasPrefix(filepath.Base(path), "mirrin-relay-") {
			variant = "relay"
		} else if strings.Contains(filepath.Base(path), "-nowhatsapp") {
			variant = "nowhatsapp"
		}
		return validBuildInfo(variant), nil
	})
}

func TestCheckBuildInfo(t *testing.T) {
	for _, c := range []struct {
		name, variant string
		change        func(*buildinfo.BuildInfo)
		want          string
	}{
		{"default", "", nil, ""}, {"MIT", "nowhatsapp", nil, ""}, {"relay", "relay", nil, ""},
		{"GPL labelled MIT", "nowhatsapp", func(i *buildinfo.BuildInfo) { i.Deps = []*debug.Module{{Path: "go.mau.fi/libsignal"}} }, "links go.mau.fi/"},
		{"MPL labelled MIT", "nowhatsapp", func(i *buildinfo.BuildInfo) { i.Deps = []*debug.Module{{Path: "go.mau.fi/util"}} }, "links go.mau.fi/"},
		{"replacement GPL", "nowhatsapp", func(i *buildinfo.BuildInfo) {
			i.Deps = []*debug.Module{{Path: "example.org/fork", Replace: &debug.Module{Path: "go.mau.fi/libsignal"}}}
		}, "links go.mau.fi/"},
		{"missing tag", "nowhatsapp", func(i *buildinfo.BuildInfo) { i.Settings = nil }, "missing the nowhatsapp"},
		{"tag substring", "nowhatsapp", func(i *buildinfo.BuildInfo) { i.Settings[0].Value = "notnowhatsapp" }, "missing the nowhatsapp"},
		{"multiple tags", "nowhatsapp", func(i *buildinfo.BuildInfo) { i.Settings[0].Value = "foo,nowhatsapp,bar" }, ""},
		{"CLI labelled relay", "relay", func(i *buildinfo.BuildInfo) { i.Path = commandPath }, "want " + commandPath + "-relay"},
		{"MIT labelled default", "", func(i *buildinfo.BuildInfo) { i.Deps = nil }, "missing go.mau.fi/libsignal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			info := validBuildInfo(c.variant)
			if c.change != nil {
				c.change(info)
			}
			err := checkBuildInfo(info, c.variant)
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

func TestCheckReadsRealBuildInfo(t *testing.T) {
	// The test executable has a real platform header and a Go build record,
	// but it must never pass as a Mirrin release binary.
	dir := t.TempDir()
	name := assetFor(runtime.GOOS, runtime.GOARCH, "nowhatsapp")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, hostBinary(t), 0700); err != nil {
		t.Fatal(err)
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path == commandPath {
		t.Fatal("test executable unexpectedly identifies as the CLI")
	}
	problems, err := check(dir, nil)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "built from") {
		t.Fatalf("test binary accepted as MIT: %v %v", problems, err)
	}
}

func TestCheckRejectsMissingBuildInfo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mirrin-relay-linux-amd64"), relayELF(62), 0600); err != nil {
		t.Fatal(err)
	}
	problems, err := check(dir, nil)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "cannot read Go build information") {
		t.Fatalf("header-only relay accepted: %v %v", problems, err)
	}
}
