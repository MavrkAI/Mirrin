package protocols

import (
	"path/filepath"
	"strings"
)

// fileURL is the file:// address of a local path: file:///tmp/x, and on
// Windows file:///C:/x ("file://" + a path is file://C:\x there, which
// isn't an address at all).
func fileURL(path string) string {
	return "file:///" + strings.TrimPrefix(filepath.ToSlash(path), "/")
}
