package service

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// A service set up while the twin still lived in a home from before the
// rename (its move waited for the old twin to quit) names that home: for
// its logs, and as MIRRIN_HOME. Once the home has moved, systemd won't
// start a unit whose log folder is gone, and launchd can't open its log
// files, so such a definition is pointed at the home as it is now
// (rehomeUnit), and a new one isn't set up while the move still waits
// (homeInUse).

// rehomeUnit points the paths in the service definition at path that lead
// into a default home from before the rename (~/.antbot in user's home)
// that has moved away at home instead. A home that is still there is left
// alone. It reports whether the file changed.
func rehomeUnit(path, user, home string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	s := string(b)
	for _, d := range brand.LegacyHomeDirs {
		old := filepath.Join(user, d)
		if old == home {
			continue
		}
		if _, err := os.Lstat(old); !os.IsNotExist(err) {
			continue // not moved (or can't tell)
		}
		s = replacePath(s, old, home)
		if esc := html.EscapeString(old); esc != old { // as a plist writes it
			s = replacePath(s, esc, html.EscapeString(home))
		}
	}
	if s == string(b) {
		return false, nil
	}
	return true, writeUnitFile(path, s, st.Mode().Perm())
}

// replacePath replaces the path from with to wherever s names it, or a path
// inside it: not where it is only part of a longer name (~/.antbot-old,
// /Volumes/x/Users/me/.antbot).
func replacePath(s, from, to string) string {
	var out strings.Builder
	for {
		i := strings.Index(s, from)
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		end := i + len(from)
		whole := (i == 0 || !pathChar(s[i-1])) && (end == len(s) || !nameChar(s[end]))
		out.WriteString(s[:i])
		if whole {
			out.WriteString(to)
		} else {
			out.WriteString(from)
		}
		s = s[end:]
	}
}

// nameChar reports whether c can continue a file name.
func nameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_'
}

// pathChar reports whether c can come before a path's first / inside a
// longer path.
func pathChar(c byte) bool { return nameChar(c) || c == '/' || c == '\\' }

// homeInUse says why the background service can't be set up or started yet:
// the twin's home is still a default one from before the rename (~/.antbot
// in user's home) because a twin runs from it, and a service set up now
// would name that home, which won't be there once it moves. nil when there
// is nothing to wait for.
func homeInUse(home, user, action string, inUse func(dataDir string) bool) error {
	base := filepath.Base(home)
	if !slices.Contains(brand.LegacyHomeDirs, base) || filepath.Join(user, base) != filepath.Clean(home) || !inUse(config.DataDirIn(home)) {
		return nil
	}
	return fmt.Errorf("a twin is still running from %s (%s's folder); quit it, then run `mirrin service %s` again", home, brand.LegacyDisplayName(base), action)
}

// writeUnitFile replaces the service definition at path with s atomically,
// with mode.
func writeUnitFile(path, s string, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(s); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
