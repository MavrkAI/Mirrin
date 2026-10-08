package brand

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// frozen are values that never change with the product's name. They are
// bytes in encrypted backups, in signatures over them, or names of tokens
// and installed apps on users' machines, so changing one breaks existing
// backups, recovery or paired phones while every other test stays green (a
// replacement across the tree changes a constant and its test together).
// The backup ones still carry the name the product had when backups began.
//
// Each entry names the file that defines the value and, for a Go constant,
// the identifier that holds it. Go literals are written as they appear in
// the source, escapes included.
var frozen = []struct{ file, ident, literal, why string }{
	{"internal/backup/keys.go", "kdfSalt", "antbot-backup-v1", "every existing backup's keys derive from it"},
	{"internal/backup/handover.go", "handoverContext", `antbot-handover-v1\n`, "handover files are signed over it"},
	{"internal/backup/target_folder.go", "legacyICloudFolder", "AntBot Backups", "existing iCloud backups and printed Recovery Kits name it"},
	{"internal/entitle/recovery.go", "", `antbot-backup-namespace-v1\n`, "namespace bindings for existing backups are signed over it"},
	{"internal/entitle/recovery.go", "", `antbot-recover-v1\n`, "account recoveries are signed over it"},

	{"internal/devices/devices.go", "TokenPrefix", "abt1_", "paired devices hold tokens that start with it"},
	{"internal/api/pwa/manifest.tmpl.json", "", `"id":"/ui"`, "an installed Home Screen app is known by its id"},
	{"internal/api/pwa/manifest.tmpl.json", "", `"scope":"/"`, "an installed app keeps its scope"},
}

// TestKeptSpellingsAreFrozen pins every spelling in frozen where it is
// defined. If one fails, put it back.
func TestKeptSpellingsAreFrozen(t *testing.T) {
	root := repoRoot(t)
	for _, f := range frozen {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.file)))
		if err != nil {
			t.Errorf("%s: %v", f.file, err)
			continue
		}
		src := string(b)
		switch {
		case f.ident != "":
			re := regexp.MustCompile(`\b` + regexp.QuoteMeta(f.ident) + `\s*=\s*"` + regexp.QuoteMeta(f.literal) + `"`)
			if !re.MatchString(src) {
				t.Errorf("%s no longer has %s = %q. Put it back: %s.", f.file, f.ident, f.literal, f.why)
			}
		case strings.HasSuffix(f.file, ".go"):
			if !strings.Contains(src, `"`+f.literal+`"`) {
				t.Errorf("%s no longer has the literal %q. Put it back: %s.", f.file, f.literal, f.why)
			}
		default:
			if !strings.Contains(src, f.literal) {
				t.Errorf("%s no longer has %s. Put it back: %s.", f.file, f.literal, f.why)
			}
		}
	}
}
