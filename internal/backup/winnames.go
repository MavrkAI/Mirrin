package backup

import (
	"path"
	"runtime"
	"strconv"
	"strings"
)

// AwkwardChars are the characters a file name can't have to come back
// under the same name on every computer. ':' and '\' can't go in a
// snapshot at all (restorable); a name with any of the others restores,
// but on Windows under a changed name (windowsSafe).
const AwkwardChars = `: \ ? * < > | "`

// windowsReserved are the characters Windows refuses in a name that macOS
// and Linux allow, besides ':' and '\'.
const windowsReserved = `?*<>|"`

// windowsNames says whether a restore must give files names Windows takes
// (a variable so tests can restore as Windows would).
var windowsNames = runtime.GOOS == "windows"

// windowsSafe is an archive path with every character Windows refuses
// replaced by '_'.
func windowsSafe(name string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(windowsReserved, r) {
			return '_'
		}
		return r
	}, name)
}

// renamer gives each file a restore writes the name it can have here, and
// remembers the ones it changed for the report.
type renamer struct {
	on      bool
	taken   map[string]bool
	renamed []string
}

func newRenamer(on bool, paths []string) *renamer {
	r := &renamer{on: on, taken: map[string]bool{}}
	for _, p := range paths {
		if windowsSafe(p) == p {
			r.taken[strings.ToLower(p)] = true // Windows names ignore case
		}
	}
	return r
}

// name is where the file at archive path p goes.
func (r *renamer) name(p string) string {
	if r == nil || !r.on {
		return p
	}
	safe := windowsSafe(p)
	if safe == p {
		return p
	}
	dir, base := path.Split(safe)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	got := safe
	for i := 2; r.taken[strings.ToLower(got)]; i++ {
		got = dir + stem + " (" + strconv.Itoa(i) + ")" + ext
	}
	r.taken[strings.ToLower(got)] = true
	r.renamed = append(r.renamed, printable(p)+" is now "+got)
	return got
}
