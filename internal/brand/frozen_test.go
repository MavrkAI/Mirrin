package brand

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// frozen are the spellings of the old name that never change with the
// product's name. They are bytes on the wire, in signatures and in encrypted
// backups, or names of files and browser storage on users' machines, so
// renaming one breaks self-hosted relays, Cloud signatures, existing backups,
// paired phones or saved settings while every other test stays green (a
// replacement across the tree changes a constant and its test together).
// This file is in package brand, which every renaming pass leaves alone.
//
// Each entry names the file that defines the spelling and, for a Go
// constant, the identifier that holds it. Go literals are written as they
// appear in the source, escapes included.
var frozen = []struct{ file, ident, literal, why string }{
	{"internal/homelock/homelock.go", "Name", "antbot.lock", "an AntBot twin and a Mirrin one on the same data folder must lock the same file"},
	{"internal/backup/contents.go", "", "antbot.lock", "a backup never carries a twin's lock"},
	{"internal/protocols/packs.go", "ProvenanceFile", ".antbot-pack.json", "every pack clone and identity archive carries it"},
	{"internal/devices/devices.go", "TokenPrefix", "abt1_", "paired devices hold tokens that start with it"},

	{"internal/relay/wire/wire.go", "Subprotocol", "antbot.tunnel.v1", "self-hosted relays speak it"},
	{"internal/relay/wire/wire.go", "HelloContext", "antbot-relay-tunnel-v1", "relays check hellos signed over it"},
	{"internal/relay/wire/wire.go", "ExporterLabel", "EXPORTER-antbot-tunnel", "it binds a hello to its TLS session"},

	{"internal/backup/keys.go", "kdfSalt", "antbot-backup-v1", "every existing backup's keys derive from it"},
	{"internal/backup/handover.go", "handoverContext", `antbot-handover-v1\n`, "handover files are signed over it"},
	{"internal/entitle/recovery.go", "", `antbot-backup-namespace-v1\n`, "Cloud checks namespace bindings signed over it"},
	{"internal/entitle/recovery.go", "", `antbot-recover-v1\n`, "Cloud checks account recoveries signed over it"},
	{"internal/httpsig/httpsig.go", "Tag", "antbot-cloud-v1", "Cloud requests are signed under it"},
	{"internal/stepup/stepup.go", "", "antbot-approval-v1", "passkeys sign approvals over it"},
	{"internal/stepup/stepup.go", "", "antbot-enrol-v1", "passkeys sign enrolments over it"},
	{"internal/cloud/cloudtest/fake.go", "", "antbot dev key ", "the dev keys pinned in internal/entitle/keys_dev.go derive from it"},
	{"cloud/internal/keys/keys.go", "", "antbot dev key ", "the dev keys pinned in internal/entitle/keys_dev.go derive from it"},
	{"internal/entitle/entitle.go", "Audience", "antbot", "every entitlement token carries it"},

	{"internal/stepup/stepup.go", "Header", "AntBot-Stepup", "Home Screen apps cached before the rename send it"},
	{"internal/api/pwa/pwa.js", "", "'AntBot-Stepup'", "the cached app sends it"},
	{"internal/api/auth.go", "noticeHeader", "AntBot-Notice", "older clients read it"},
	{"internal/identity/settings.go", "homeToken", "$ANTBOT_HOME", "older builds import archives that carry it"},

	{"internal/api/ui.html", "", "'antbot.name'", "browsers keep the twin's name under it"},
	{"internal/api/ui.html", "", `"antbot.theme"`, "browsers keep the chosen theme under it"},
	{"internal/api/ui.html", "", "'antbot.noted.seen'", "browsers keep what was seen under it"},
	{"internal/api/ui.html", "", "'antbot.left.seen'", "browsers keep what was seen under it"},
	{"internal/api/ui.html", "", "'antbot.character'", "browsers keep the chosen character under it"},
	{"internal/api/pwa/pwa.js", "", "'antbot-install-ticket'", "an app being installed keeps its ticket under it"},
	{"internal/api/pwa/pwa.js", "", "'antbot-last-seen'", "the app keeps its last visit under it"},
	{"internal/api/pwa/manifest.tmpl.json", "", `"id":"/ui"`, "an installed Home Screen app is known by its id"},
	{"internal/api/pwa/manifest.tmpl.json", "", `"scope":"/"`, "an installed app keeps its scope"},

	{"packaging/windows/mirrin.iss", "", "AppId={{7E1C6E4E-4C5C-4A5E-9C2B-ANTBOT01}", "the setup upgrades an AntBot install in place"},
}

// TestKeptSpellingsAreFrozen pins every spelling in frozen where it is
// defined. If one fails, put the old spelling back: the name it carries is
// the old product's on purpose.
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

// Every storage key the pages use stays one of the kept ones or a new
// name: renaming a key in place forgets what browsers saved under it.
func TestStorageKeysKeepTheirNames(t *testing.T) {
	root := repoRoot(t)
	keep := map[string]bool{}
	for _, f := range frozen {
		if k := strings.Trim(f.literal, `'"`); strings.HasPrefix(k, "antbot.") || strings.HasPrefix(k, "antbot-") {
			keep[k] = true
		}
	}
	reKey := regexp.MustCompile(`(?:localStorage|sessionStorage)\.(?:get|set|remove)Item\(\s*['"]([^'"]+)['"]`)
	dir := filepath.Join(root, "internal", "api")
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js")) || strings.HasSuffix(p, "_test.js") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range reKey.FindAllStringSubmatch(string(b), -1) {
			if k := m[1]; strings.HasPrefix(k, Name+".") || strings.HasPrefix(k, Name+"-") {
				if old := "antbot" + k[len(Name):]; keep[old] {
					rel, _ := filepath.Rel(root, p)
					t.Errorf("%s stores %q, which browsers know as %q: keep the old key", filepath.ToSlash(rel), k, old)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
