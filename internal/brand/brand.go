// Package brand is the product's name in the forms the code needs, and the
// names it had before: Mirrin was called AntBot, and before that openHuman.
// Homes, settings and background services made under an old name keep
// working, so code that reads an environment variable or looks for a home
// or a service by name asks here rather than spelling the names out.
//
// The old names live only in this package (and on lines marked
// rename:keep), so a later rename can't sweep them away by accident.
package brand

import (
	"os"
	"strings"
)

const (
	// Name is the command, the service label and the lower-case name.
	Name = "mirrin"
	// DisplayName is the name people read.
	DisplayName = "Mirrin"
	// HomeDirName is the home folder's name in the user's home (~/.mirrin).
	HomeDirName = ".mirrin"
)

// The names the product had before, newest first, and their home folders
// and display names in the same order. Never remove one: a machine can
// skip versions.
var (
	LegacyNames        = []string{"antbot", "openhuman"}   // rename:keep
	LegacyHomeDirs     = []string{".antbot", ".openhuman"} // rename:keep
	LegacyDisplayNames = []string{"AntBot", "openHuman"}   // rename:keep
)

// envPrefixes are the prefixes of the product's own environment variables,
// current first.
var envPrefixes = []string{"MIRRIN_", "ANTBOT_", "OPENHUMAN_"} // rename:keep

// Env is the value of the product's variable with this suffix: MIRRIN_<suffix>,
// else the same variable under an old name (ANTBOT_, then OPENHUMAN_).
func Env(suffix string) string { return Getenv(envPrefixes[0] + suffix) }

// Getenv is the first of EnvAliases(name) that is set, or "".
func Getenv(name string) string {
	for _, n := range EnvAliases(name) {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// EnvAliases are the names a variable of the product's goes by: name itself
// first, then the same variable under the other prefixes, current first.
// A variable that isn't the product's (OPENAI_API_KEY) has only its own name.
func EnvAliases(name string) []string {
	suffix, ok := envSuffix(name)
	if !ok {
		return []string{name}
	}
	out := []string{name}
	for _, p := range envPrefixes {
		if p+suffix != name {
			out = append(out, p+suffix)
		}
	}
	return out
}

// CurrentEnv is name under the current prefix: MIRRIN_X for ANTBOT_X or
// OPENHUMAN_X. Any other name comes back as it is.
func CurrentEnv(name string) string {
	if suffix, ok := envSuffix(name); ok {
		return envPrefixes[0] + suffix
	}
	return name
}

// envSuffix splits one of the product's variables into its prefix's rest.
func envSuffix(name string) (string, bool) {
	for _, p := range envPrefixes {
		if s, ok := strings.CutPrefix(name, p); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

// IsServiceLabel reports whether a launchd job label (launchd passes it on
// as XPC_SERVICE_NAME) is the product's background service, under its
// current name or an old one.
func IsServiceLabel(label string) bool {
	if label == Name {
		return true
	}
	for _, n := range LegacyNames {
		if label == n {
			return true
		}
	}
	return false
}

// LegacyDisplayName is the name people knew an old service, program or home
// by ("AntBot" for "antbot" or ".antbot"), or DisplayName for anything else.
func LegacyDisplayName(name string) string {
	for i := range LegacyNames {
		if name == LegacyNames[i] || name == LegacyHomeDirs[i] {
			return LegacyDisplayNames[i]
		}
	}
	return DisplayName
}
