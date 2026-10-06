package agent

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var reImageMarker = regexp.MustCompile(`\[\[image:([^\]]+)\]\]`)

// reShot matches the names the browser gives its captures.
var reShot = regexp.MustCompile(`^(browser|screenshot)-[0-9-]+\.(png|jpe?g)$`)

// maxImage is the largest picture sent to the model.
const maxImage = 4 << 20

// liftImage pulls a "[[image:/path]]" marker out of a tool result and loads
// the picture so the model can look at it. The path stays in the text so it
// can be forwarded to the user. Only a screenshot the twin took itself is
// read: a file directly in dataDir, named like the browser's captures, that
// really is a PNG or JPEG. Any other marker (a web page, an email or an MCP
// server can print one) is left as text and nothing is read.
func liftImage(text, dataDir string) (string, []byte, string) {
	for _, m := range reImageMarker.FindAllStringSubmatch(text, -1) {
		path, ok := ownScreenshot(strings.TrimSpace(m[1]), dataDir)
		if !ok {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxImage+1))
		f.Close()
		if err != nil || len(data) == 0 || len(data) > maxImage {
			continue
		}
		mt := http.DetectContentType(data)
		if mt != "image/png" && mt != "image/jpeg" {
			continue
		}
		return strings.Replace(text, m[0], "screenshot: "+path, 1), data, mt
	}
	return text, nil, ""
}

// ownScreenshot cleans path and reports whether it names one of the twin's
// own screenshots in dataDir.
func ownScreenshot(path, dataDir string) (string, bool) {
	if dataDir == "" || !filepath.IsAbs(path) {
		return "", false
	}
	path = filepath.Clean(path)
	if filepath.Dir(path) != filepath.Clean(dataDir) || !reShot.MatchString(filepath.Base(path)) {
		return "", false
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return path, true
}
