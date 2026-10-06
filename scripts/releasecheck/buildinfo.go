package main

import (
	"debug/buildinfo"
	"fmt"
	"runtime/debug"
	"strings"
)

const commandPath = "github.com/MavrkAI/Mirrin/cmd/mirrin"

func checkBuildFile(path, variant string, readInfo func(string) (*buildinfo.BuildInfo, error)) error {
	info, err := readInfo(path)
	if err != nil {
		return fmt.Errorf("cannot read Go build information: %w", err)
	}
	return checkBuildInfo(info, variant)
}

// The platform header alone cannot distinguish a GPL binary labelled MIT,
// or a CLI labelled as the relay. Check Go's embedded build record as well.
func checkBuildInfo(info *buildinfo.BuildInfo, variant string) error {
	if info == nil {
		return fmt.Errorf("missing Go build information")
	}
	wantPath := commandPath
	if variant == "relay" {
		wantPath += "-relay"
	}
	if info.Path != wantPath {
		return fmt.Errorf("built from %q, want %s", info.Path, wantPath)
	}
	hasSignal, hasWhatsApp := false, false
	for _, dep := range info.Deps {
		if dep == nil {
			continue
		}
		for _, path := range []string{dep.Path, replacementPath(dep.Replace)} {
			hasSignal = hasSignal || path == "go.mau.fi/libsignal"
			hasWhatsApp = hasWhatsApp || strings.HasPrefix(path, "go.mau.fi/")
		}
	}
	if variant == "nowhatsapp" {
		tagged := false
		for _, setting := range info.Settings {
			if setting.Key != "-tags" {
				continue
			}
			for _, tag := range strings.FieldsFunc(setting.Value, func(r rune) bool { return r == ',' || r == ' ' }) {
				tagged = tagged || tag == "nowhatsapp"
			}
		}
		if !tagged {
			return fmt.Errorf("MIT build is missing the nowhatsapp build tag")
		}
		if hasWhatsApp {
			return fmt.Errorf("MIT build links go.mau.fi/ code; rebuild with -tags nowhatsapp")
		}
	} else if variant == "" && !hasSignal {
		return fmt.Errorf("default build is missing go.mau.fi/libsignal (WhatsApp)")
	}
	return nil
}

func replacementPath(dep *debug.Module) string {
	if dep == nil {
		return ""
	}
	return dep.Path
}
