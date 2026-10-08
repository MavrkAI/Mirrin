package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/brand"
)

// homeVar names the twin's home when it is set.
const homeVar = "MIRRIN_HOME"

// userHomeDir is the user's own home folder. Tests point it at a temporary
// one, so they never look at the contributor's real homes.
var userHomeDir = os.UserHomeDir

// Home returns the Mirrin home directory: the folder MIRRIN_HOME names,
// else ~/.mirrin.
func Home() string {
	if h := HomeEnv(os.Getenv); h != "" {
		return h
	}
	user, err := userHomeDir()
	if err != nil || user == "" {
		return brand.HomeDirName
	}
	return filepath.Join(user, brand.HomeDirName)
}

// HomeEnv is the home a set of environment variables names: MIRRIN_HOME,
// unless it names the default home (~/.mirrin), which counts as not set.
// "" means the default home.
func HomeEnv(getenv func(string) string) string {
	if h := getenv(homeVar); h != "" && !IsDefaultHome(h) {
		return h
	}
	return ""
}

// IsDefaultHome reports whether dir is a home's default place, ~/.mirrin.
func IsDefaultHome(dir string) bool {
	user, err := userHomeDir()
	if err != nil || user == "" {
		return false
	}
	return samePath(dir, filepath.Join(user, brand.HomeDirName))
}

func samePath(a, b string) bool {
	return pathsEqual(filepath.Clean(a), filepath.Clean(b))
}

// How paths compare here: on Windows and macOS (whose disks ignore case
// unless set up otherwise) case doesn't count, and on Windows / and \ are
// alike. Tests set them anywhere.
var (
	foldCase    = runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	foldSlashes = runtime.GOOS == "windows"
)

// pathsEqual reports whether two paths, as written, name the same place.
func pathsEqual(a, b string) bool {
	if foldSlashes {
		a, b = strings.ReplaceAll(a, `\`, "/"), strings.ReplaceAll(b, `\`, "/")
	}
	if foldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}
