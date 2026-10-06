package daemon

import (
	"os"
	"path/filepath"
	"strings"
)

// screenshotFile reports whether p names a screenshot the daemon may show
// or send: a non-empty .png directly inside dataDir, where the browser
// writes them. The path is cleaned and its symlinks followed first, so
// "<dataDir>/../x.png", "<dataDir>-other/x.png" or a link pointing out of
// the directory are all refused.
func screenshotFile(dataDir, p string) (string, bool) {
	if dataDir == "" || !filepath.IsAbs(p) || !strings.EqualFold(filepath.Ext(p), ".png") {
		return "", false
	}
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", false
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return "", false
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil || filepath.Dir(real) != dir {
		return "", false
	}
	st, err := os.Stat(real)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return "", false
	}
	return filepath.Clean(p), true
}
