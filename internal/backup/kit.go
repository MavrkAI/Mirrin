package backup

import (
	"fmt"
	"strings"
	"time"
)

// CheckWord is the word `mirrin backup init` asks for back, to be sure the
// words were written down before any backup depends on them.
const CheckWord = 7

// KitInfo is what a Recovery Kit says besides the words.
type KitInfo struct {
	Twin  string
	Where string // where backups go, in the owner's words
	Made  time.Time
}

// RecoveryKit is the printable page with the 12 words: plain text that
// fits a sheet of paper, numbered in two columns so the words can be copied
// by hand, with what they are for and how to use them. It is shown once and
// never saved by Mirrin.
func RecoveryKit(p Phrase, info KitInfo) string {
	w := p.Words()
	var b strings.Builder
	line := strings.Repeat("=", 60)
	fmt.Fprintln(&b, line)
	fmt.Fprintln(&b, "  Mirrin Recovery Kit")
	fmt.Fprintln(&b, line)
	twin := info.Twin
	if twin == "" {
		twin = "your twin"
	}
	fmt.Fprintf(&b, "  Twin:     %s\n", twin)
	if !info.Made.IsZero() {
		fmt.Fprintf(&b, "  Made:     %s\n", info.Made.Format("2 January 2006"))
	}
	if info.Where != "" {
		fmt.Fprintf(&b, "  Backups:  %s\n", info.Where)
	}
	fmt.Fprintf(&b, "  Kit ID:   %s  (the same ID shows in `mirrin backup status`)\n", KitID(p))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  Your 12 words")
	fmt.Fprintln(&b)
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&b, "     %2d. %-14s %2d. %s\n", i+1, w[i], i+7, w[i+6])
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  These words are the only key to your backups. Mirrin doesn't")
	fmt.Fprintln(&b, "  keep them, so nobody can give them back if they're lost: not")
	fmt.Fprintln(&b, "  us, not Apple, not whoever stores the backups. Anyone who has")
	fmt.Fprintln(&b, "  them can read your backups, keys and conversations included.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  Write them on paper and keep it somewhere safe and offline,")
	fmt.Fprintln(&b, "  with your passport. Not in a photo or a note on your phone.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  To bring your twin back, on this machine or a new one:")
	fmt.Fprintln(&b, "    install Mirrin, run `mirrin restore`, and type the 12 words.")
	fmt.Fprintln(&b, line)
	return b.String()
}

// KitID is a short, public tag for a phrase (from its namespace), so a kit
// can be matched to a twin without showing the words.
func KitID(p Phrase) string { return KitIDFor(p.Namespace()) }

// KitIDFor is the Kit ID for a namespace.
func KitIDFor(ns string) string {
	if len(ns) < 8 {
		return ns
	}
	return ns[:4] + "-" + ns[4:8]
}

// CheckTyped reports whether typed is word n of p (case and spaces aside;
// the first four letters are enough).
func CheckTyped(p Phrase, n int, typed string) bool {
	typed = strings.ToLower(strings.TrimSpace(typed))
	want := p.Word(n)
	if typed == "" || want == "" {
		return false
	}
	if typed == want {
		return true
	}
	i, ok := lookupWord(typed)
	return ok && wordlist[i] == want
}
