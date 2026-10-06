package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A release's Sigstore signature names the repository its workflow ran in,
// and GitHub's redirect after a rename doesn't reach it. The repository has
// been renamed twice, so the check commands people copy from the docs and
// the site must name the module's repository, as the installer and
// `mirrin update` do, or they reject every genuine release.
func TestVerifyCommandsNameTheRepository(t *testing.T) {
	gomod, err := os.ReadFile(repoFile("go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^module github\.com/(\S+)$`).FindSubmatch(gomod)
	if m == nil {
		t.Fatal("go.mod names no github.com module")
	}
	repo := string(m[1])
	workflow := "https://github.com/" + repo + "/.github/workflows/release.yml"
	for _, c := range []struct{ file, want string }{
		{"docs/maintainers-release.md", "--certificate-identity " + workflow + "@refs/tags/vX.Y.Z"},
		{"docs/maintainers-release.md", "gh attestation verify mirrin-linux-amd64 -R " + repo + "\n"},
		{"site/cloud.html", "wf='^" + regexp.QuoteMeta(workflow) + "'"},
		{"install.sh", "REPO=${MIRRIN_REPO:-" + repo + "}"},
		{"cmd/mirrin/update.go", `defaultRepo = "` + repo + `"`},
	} {
		b, err := os.ReadFile(repoFile(c.file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), c.want) {
			t.Errorf("%s doesn't say %q: the repository is %s (go.mod)", c.file, strings.TrimSpace(c.want), repo)
		}
	}
}

// The site has a stale-claims table (site/site_test.go); the root docs had
// none, and README, ARCHITECTURE, the skills guide and the threat model
// drifted from the code in several places with every test green. Each row
// is a claim that stops being true once the code holds a marker: while the
// marker is there, no doc may make the claim.
func TestRootDocsHaveNoStaleClaims(t *testing.T) {
	docs := []string{"README.md", "ARCHITECTURE.md", "CONTRIBUTING.md", "CHANGELOG.md", "docs/skills.md", "docs/threat-model.md"}
	stale := []struct{ file, code, claim, why string }{
		{"internal/api/pages.go", "l.kind == kindLoopback && s.isMaster", "and the cookie sticks", "?token= sets a cookie only on this computer; a wall screen pairs with `mirrin pair --kiosk`"},
		{"internal/api/pages.go", "l.kind == kindLoopback && s.isMaster", "The token becomes a cookie", "?token= sets a cookie only on this computer; a wall screen pairs with `mirrin pair --kiosk`"},
		{"internal/api/pages.go", "l.kind == kindLoopback && s.isMaster", "/ui?token=<token>", "?token= sets a cookie only on this computer; a wall screen pairs with `mirrin pair --kiosk`"},
		{"cmd/mirrin/pair.go", "computer's master key.", "pairing code carries the access token", "a pairing code or link never holds the master key"},
		{"cmd/mirrin/pair.go", "computer's master key.", "embeds the address and the access token", "a pairing code or link never holds the master key"},
		{"cmd/mirrin/pair.go", "computer's master key.", "prints a pairing code for each address", "`mirrin pair` prints one single-use code"},
		{"cmd/mirrin/pair.go", "computer's master key.", "a token per device and a new pairing flow are", "per-device keys and pairing v2 are in internal/devices and internal/api"},
		{"internal/config/secrets.go", "var exported []string", "such as `ELEVENLABS_API_KEY`", "nothing is exported from secrets.env any more"},
		{"internal/backup/contents.go", "nameSecrets", "Keys and tokens are the one part that never travels", "encrypted backups carry the keys"},
		{"internal/channels/media.go", "func VoiceNote(", "Voice notes, photos and files get a quick reply saying the twin can't open them", "voice notes are transcribed and photos shown to the model"},
		{"internal/channels/media.go", "func VoiceNote(", "voice notes and photos the twin can open", "voice notes and photos are opened now"},
		{"internal/skills/browser/browser.go", "func (s *Session) AllowHosts", "The browser is not yet behind the egress check", "the browser's guard proxy shares fetch_url's allow-list"},
		{"internal/skills/google/client.go", "computer it started on", "Google in one click", "Google needs the owner's own OAuth client, pasted once"},
		{"internal/daemon/devices.go", "func deviceCantCommand", "calendar token, mailbox password", "the Google self-check is called google now"},
	}
	for _, s := range stale {
		code, err := os.ReadFile(filepath.Join("..", s.file))
		if err != nil {
			t.Fatalf("%s: %v (the marker's file moved: update this table)", s.file, err)
		}
		if !strings.Contains(string(code), s.code) {
			continue // the code changed back; nothing to say
		}
		for _, d := range docs {
			b, err := os.ReadFile(filepath.Join("..", d))
			if err != nil {
				t.Fatal(err)
			}
			text := string(b)
			if d == "CHANGELOG.md" {
				text = unreleased(text) // released sections are history
			}
			if strings.Contains(text, s.claim) {
				t.Errorf("%s still says %q, but %s (%s)", d, s.claim, s.why, s.file)
			}
		}
	}
}

// unreleased is the CHANGELOG's Unreleased section.
func unreleased(changelog string) string {
	_, rest, ok := strings.Cut(changelog, "## Unreleased")
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "\n## "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
