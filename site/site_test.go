// Package site holds the website (index.html, cloud.html, protocols.html,
// pricing.html, legal.html, 404.html, site.css). It has no Go code; these
// tests keep the pages true of the code beside them. The pages are written
// for customers and cite no files or functions, so the tests do the citing:
// the numbers, colours, personas, licences and commands the pages state
// match the code, claims the code has made false stay gone, the pricing
// page's visibility tables match docs/cloud-trust.md, the product name stays
// in its one token, and the self-hosted fonts cover every character used.
package site

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// root is the repository, one level up from site/.
const root = ".."

var pages = []string{"index.html", "cloud.html", "protocols.html", "pricing.html", "legal.html", "404.html"}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func page(t *testing.T, name string) string { return read(t, name) }

func repoFile(t *testing.T, rel string) string { return read(t, filepath.Join(root, rel)) }

// between returns the part of s after the first start and before the next
// end, and fails the test in plain words when either is missing.
func between(t *testing.T, s, where, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("%s no longer has %s; update this test to find what replaced it", where, start)
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		t.Fatalf("%s has %s but no %s after it; update this test to match", where, start, end)
	}
	return s[:j]
}

var reComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// body is a page without its comments, which document the page rather than
// make claims.
func body(t *testing.T, name string) string { return reComment.ReplaceAllString(page(t, name), "") }

// The pages are for customers: what a reader sees names no source files,
// functions, packages, design-doc sections or work packages, and there are
// no evidence notes. The tests below check the facts against the code
// instead. Each page carries one quiet line saying the code is open.
func TestCopyCitesNoCode(t *testing.T) {
	reCite := regexp.MustCompile(`(?:^|[\s(“"])(?:internal|cmd|cloud|scripts|packaging)/[\w./-]+|\b\w+\.go\b|§|\bWP-\d+|(?i:evidence)`)
	const line = `Check it yourself: <span class="pn" translate="no">Mirrin</span> is <a href="https://github.com/MavrkAI/Mirrin">open source</a>.`
	for _, p := range pages {
		raw := body(t, p)
		for _, gone := range []string{`class="evidence"`, `class="ev-`, `class="src"`, "Show the sources"} {
			if strings.Contains(raw, gone) {
				t.Errorf("%s still has %s: the evidence notes are gone from the site; the tests check the facts instead", p, gone)
			}
		}
		text := visibleText(raw)
		for _, loc := range reCite.FindAllStringIndex(text, -1) {
			lo, hi := max(0, loc[0]-60), min(len(text), loc[1]+40)
			t.Errorf("%s cites code in its copy: %q. Say it in plain words; a test checks it against the code.", p, strings.Join(strings.Fields(text[lo:hi]), " "))
		}
		if n := strings.Count(raw, line); n != 1 {
			t.Errorf("%s has the line %q %d times; every page has it once", p, line, n)
		}
	}
}

// countDirs counts the directories directly inside dir.
func countDirs(t *testing.T, dir string) int {
	t.Helper()
	es, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range es {
		if e.IsDir() {
			n++
		}
	}
	return n
}

func TestNumbersMatchTheCode(t *testing.T) {
	index, cloud := body(t, "index.html"), body(t, "cloud.html")

	// Thirteen channels, eleven of them messaging; pictures on seven.
	if n := countDirs(t, "internal/channels"); n != 13 {
		t.Errorf("internal/channels has %d channels; both pages say thirteen (eleven messaging, plus voice and the terminal)", n)
	}
	if n := strings.Count(between(t, index, "index.html", `<ul class="chips"`, "</ul>"), "<li"); n != 13 {
		t.Errorf("the channel chips in index.html list %d channels; there are thirteen", n)
	}
	// Counted per channel: one channel may have more than one file that
	// sends pictures (whatsapp's none.go, the -tags nowhatsapp stand-in).
	sends := map[string]bool{}
	reImage := regexp.MustCompile(`func \([^)]*\) SendImage\(`)
	err := filepath.WalkDir(filepath.Join(root, "internal/channels"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && reImage.Match(b) {
			// The channel is the first folder under internal/channels, so a
			// subpackage of one (whatsapp/media) doesn't count as another.
			rel, _ := filepath.Rel(filepath.Join(root, "internal/channels"), path)
			sends[strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]] = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if images := len(sends); images != 7 {
		t.Errorf("%d channels send pictures (SendImage); the pages say seven (index.html sections 1 and 8, cloud.html section 2 and Table 1)", images)
	}
	for _, claim := range []string{"On seven of these apps it can send you pictures too", "with a picture of the page on seven"} {
		if !strings.Contains(index+cloud, claim) {
			t.Errorf("the pages no longer say %q; update this test with the new wording", claim)
		}
	}

	// Things the pages quote, and where the code says them.
	facts := []struct{ file, code, page, says string }{
		{"internal/daemon/channels.go", `" is about to submit"`, index, `“What <span class="pn" translate="no">Mirrin</span> is about to submit”`},
		{"internal/config/config.go", "PerActionLimit: 100, MonthlyLimit: 500", index, "100 a payment and 500 a month"},
		{"internal/daemon/daemon.go", `"0 8 * * 0"`, index, "Every Sunday morning it writes a short portrait"},
		{"internal/protocols/protocols.go", `schedule: "0 7 * * *"`, index, "a morning briefing at 7:00"},
		{"internal/heartbeat/heartbeat.go", "BackupIfDue(ctx, 24*time.Hour, 7)", index, "copies it, keeping the last seven"},
		{"internal/api/ui.html", ".idle{--c1:#6d7cff", index, `text-anchor="middle">#6d7cff</text>`},
		{"internal/api/ui.html", ".listening{--c1:var(--ok)", index, "green while listening"},
		{"internal/api/ui.html", "--ok:#3ddc84", index, `text-anchor="middle">#3ddc84</text>`},
		{"internal/api/ui.html", ".thinking{--c1:var(--accent)", index, "blue while thinking or speaking"},
		{"internal/api/ui.html", ".speaking{--c1:var(--accent)", index, "blue while thinking or speaking"},
		{"internal/api/ui.html", "--accent:#7aa2ff", index, `text-anchor="middle">#7aa2ff</text>`},
		{"internal/agent/asking.go", "maxApprovalValue = 10_000", index, "shows you the whole command, script or message"},
		{"install.sh", "checksum_ok() {", index, "both installers check every download"},
		{"install.sh", "verify_signature() {", index, "also checks the Sigstore signature"},
		{"install.ps1", "SHA256]::Create().ComputeHash", index, "both installers check every download"},
		{"internal/service/service.go", "func (in installer) keepSecrets() error", index, "Keys you've exported in your shell go into <code>~/.mirrin/secrets.env</code>"},
		{"internal/service/service.go", "func waitUp(", index, "starts your twin straight away"},
		{"cmd/mirrin/selfcare.go", `if goos == "darwin" && wantTray && trayAvailable`, index, "<code>service install</code> runs your twin headless, with no app indicator"},
		{"internal/api/channels.go", `"/channels?token=" + s.masterKey()`, index, "http://127.0.0.1:7742/channels?token=$(cat ~/.mirrin/data/api.token)"},
		{"internal/config/secrets.go", `"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"`, index, "Paste your Claude, OpenAI or Gemini key"},
		{"Makefile", "PORTABLE := linux/amd64 linux/arm64 windows/amd64 windows/arm64", index, "Linux and Windows (<span class=\"nw\">x86-64</span> and ARM64)"},
		{".github/workflows/release.yml", "dist/mirrin-darwin-amd64", index, "Macs (Apple silicon and Intel)"},
		{".github/workflows/release.yml", "cosign sign-blob", cloud, "signed with Sigstore by the release workflow"},
		{"internal/protocols/packs.go", "return BuiltinRegistry()", index, "It keeps its own copy of the list"},
		{"internal/protocols/packs.go", "return nil, errNoRegistryKey", index, "Once the list is signed, your twin will fetch the latest from GitHub when you search"},
		{"cmd/mirrin/update.go", `defaultRepo = "MavrkAI/Mirrin"`, index, "and our releases when you run <code>mirrin update</code>"},
		{"internal/channels/voice/setup.go", "Voice setup downloads about 850 MB", index, "downloads about 850&nbsp;MB of models"},
		{"internal/channels/voice/voice.go", "func unfinished(text string) bool", index, "it waits for the rest and hears the two as one request"},
		{"internal/channels/voice/voice.go", "t, _ = stripWakeLoose(t, c.cfg.WakeWord)", index, "so you can reply without the name"},
		{"internal/config/usage.go", "DefaultMonthlyBudget = 25", index, "US$25 out of the box"},
		{"internal/daemon/usage.go", "sp.Used < 0.8", index, "warns you at 80% of a monthly budget"},
		{"internal/daemon/timekeeping.go", `weatherURL = "https://api.open-meteo.com/v1/forecast"`, index + cloud, "The screen's weather still asks Open-Meteo, with your time zone's city, unless you set <code>ui.weather: false</code>"},
		{"internal/channels/voice/voice.go", `"349.30 AUD" is said "$349.30"`, index, "“$349.30”, not “349.30 AUD”"},
		{"internal/channels/voice/dates.go", "the second of October", index, "“the second of October”, not “Oct 2”"},
		{"internal/channels/voice/aloud.go", "is never\n// approved by voice", index, "a dangerous one is never approved by voice"},
		{"internal/skills/spend/spend.go", `"Pay %.2f %s"`, index, "A payment reads “Pay 389.00 AUD for Jetstar MEL-SYD Tue 6am”"},
		{"internal/api/pages_spending.go", "they change on this computer only", index, "changes them on this computer only"},
		{"internal/tray/tray.go", `"Spending…"`, index, "Spending… in the menu bar or tray"},
		{"internal/api/ui.html", "browser_act:'Act on a website'", index, `<span class="need-tag">Act on a website</span>`},
		{"internal/api/ui.html", "dangerous:'Needs care'", index, `<span class="need-tag risk">Needs care</span>`},
		{"internal/api/browser_live.go", "control) on this computer only", index, "taking over happens on this computer only"},
		{"internal/daemon/browserlive.go", "carryOnAfterHandBack(key string)", index, "then Hand back, and it carries on where it left off"},
		{"internal/stepup/stepup.go", "a passkey (Face ID, a fingerprint, a device PIN)", index, "a passkey: Face ID, a fingerprint or your device PIN"},
		{"internal/api/devices_add.html", "Add it to your Home Screen", index, "a web app you add to your Home Screen (no app store)"},
		{".github/workflows/release.yml", "dist/Mirrin-*-macos.dmg", index, "<code>Mirrin-&lt;version&gt;-macos.dmg</code>"},
		{"scripts/release-macos.sh", "a universal Mirrin.app", index, "(one app for Apple silicon and Intel)"},
		{"internal/tray/tray.go", "go openWelcome(ctx, backend)", index, "a Welcome window finds or checks your model key"},
		{"internal/api/welcome.html", "Who would you like?", index, "the Welcome window asks who you'd like"},
		{"internal/api/welcome.html", "Restore my twin", index, "can restore a twin from an encrypted backup"},
		{"README.md", "Open Anyway", index, "Privacy &amp; Security → Open Anyway"},
	}
	for _, f := range facts {
		if !strings.Contains(repoFile(t, f.file), f.code) {
			t.Errorf("%s no longer has %s, which the site relies on for %q. Check the claim and update the page.", f.file, f.code, f.says)
		}
		if !strings.Contains(f.page, f.says) {
			t.Errorf("the page no longer says %q; if the claim moved, update this test", f.says)
		}
	}
}

// The default spending caps are 100 and 500 only where the unit is worth
// about a dollar: capScale in internal/config/locale.go scales them for
// currencies whose unit is worth far less. The page names the plain ones
// (dollars, pounds, euros, francs) and gives the yen as its example.
func TestSpendingCapsMatchTheLocales(t *testing.T) {
	src := repoFile(t, "internal/config/locale.go")
	caps := between(t, src, "internal/config/locale.go", "var capScale = map[string]float64{", "}")
	scale := map[string]float64{}
	for _, m := range regexp.MustCompile(`"([A-Z]{3})": ([0-9.]+)`).FindAllStringSubmatch(caps, -1) {
		f, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatal(err)
		}
		scale[m[1]] = f
	}
	if len(scale) < 5 {
		t.Fatalf("only %d currencies read from capScale; the map's format changed", len(scale))
	}
	index := body(t, "index.html")
	jpy := scale["JPY"]
	for _, says := range []string{
		"100 a payment and 500 a month in dollars, pounds, euros or francs",
		"¥" + commas(int(100*jpy)) + " and ¥" + commas(int(500*jpy)) + " in yen",
	} {
		if !strings.Contains(index, says) {
			t.Errorf("index.html should say %q (capScale in internal/config/locale.go has JPY at %v)", says, jpy)
		}
	}
	// Every currency a locale uses unscaled is a dollar, a pound, a euro or a
	// franc, which is what the page says 100 and 500 are in.
	plain := map[string]bool{"USD": true, "CAD": true, "AUD": true, "NZD": true, "SGD": true, "HKD": true, "GBP": true, "EUR": true, "CHF": true}
	for _, m := range regexp.MustCompile(`\{"([A-Z]{3})", "Polly`).FindAllStringSubmatch(src, -1) {
		if c := m[1]; scale[c] == 0 && !plain[c] {
			t.Errorf("locale.go uses %s with the caps unscaled; index.html says 100 and 500 are in dollars, pounds, euros or francs", c)
		}
	}
}

// commas writes n with thousands separators: 10000 is "10,000".
func commas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Two components never share a class name in site.css: the Protocols
// gallery's cards, once also called .pcard, restyled Fig. 7's Needs-you card
// on the home page. A top-level rule for a class is written once.
func TestCSSClassesDefinedOnce(t *testing.T) {
	seen := map[string]int{}
	for i, line := range strings.Split(read(t, "site.css"), "\n") {
		m := regexp.MustCompile(`^\.([A-Za-z0-9_-]+) \{`).FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if first, ok := seen[m[1]]; ok {
			t.Errorf("site.css defines .%s at the top level twice (lines %d and %d): two components may be sharing a name; merge the rules or rename one", m[1], first, i+1)
			continue
		}
		seen[m[1]] = i + 1
	}
	if len(seen) < 50 {
		t.Fatalf("only %d top-level class rules found in site.css; the pattern no longer matches how it is written", len(seen))
	}
}

// The Mac app doesn't start itself at sign-in; the page says how to make it.
// When the app learns to add itself to the login items, say so instead.
func TestMacAppSignIn(t *testing.T) {
	says := "The app doesn't start itself when you sign in"
	if !strings.Contains(body(t, "index.html"), says) {
		t.Fatalf("index.html no longer says %q; if the app now starts itself, check the code and update this test", says)
	}
	re := regexp.MustCompile(`SMAppService|LoginItem|SMLoginItem|login item`)
	err := filepath.WalkDir(filepath.Join(root, "internal/tray"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if b, err := os.ReadFile(path); err == nil && re.Match(b) {
			t.Errorf("%s looks like it adds a login item; check it, then drop %q from Install in index.html", path, says)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// With no signing key, the twin never fetches the community list: search and
// install answer from the copy built in (fetchSignedRegistry returns before
// any request). The pages must not say it fetches the list until a key is in.
func TestRegistryFetchClaims(t *testing.T) {
	pub := repoFile(t, "registry/minisign.pub")
	code := repoFile(t, "internal/protocols/packs.go")
	keyless := strings.Contains(pub, "Until then") && strings.Contains(code, "return nil, errNoRegistryKey")
	for _, p := range pages {
		s := body(t, p)
		for _, claim := range []string{"fetched on protocol search", "fetches them only when you search", "search would fall back to the copy", "only when you search, install or update", "only when you update or search for routines", "the community list of routines, any pack"} {
			if keyless && strings.Contains(s, claim) {
				t.Errorf("%s says %q, but with no key in registry/minisign.pub the twin never fetches the list (fetchSignedRegistry)", p, claim)
			}
		}
	}
	if !keyless {
		t.Error("registry/minisign.pub may hold a key now: the twin fetches the signed list when you search. Mark the signed index available now on protocols.html, and say so in index.html section 7, the shared FAQ and legal.html")
	}
}

// Claims the site used to make that the code has since made false. Each is
// tied to the code that made it false, so the check means something.
func TestNoStaleClaims(t *testing.T) {
	stale := []struct{ file, code, claim, why string }{
		{"internal/service/service.go", "func (in installer) keepSecrets() error", "passes on only", "service install now keeps every provider key (secrets.env)"},
		{"internal/service/service.go", "func (in installer) keepSecrets() error", "One catch for now", "service install now keeps every provider key (secrets.env)"},
		{".github/workflows/release.yml", "dist/mirrin-darwin-amd64", "On an Intel Mac, build from source", "releases now include Intel Mac builds"},
		{".github/workflows/release.yml", "dist/mirrin-darwin-amd64", "on an Intel Mac, build from source", "releases now include Intel Mac builds"},
		{"internal/config/config.go", "Spending{Currency: loc.Currency", "A$100", "the spending currency now follows the owner's locale"},
		{"install.sh", "checksum_ok() {", "it verifies nothing yet", "install.sh now checks SHA256SUMS"},
		{"install.sh", "checksum_ok() {", "doesn't check a signature yet", "install.sh now checks SHA256SUMS, and its signature with cosign"},
		{"install.sh", "main/install.ps1 | iex", "doesn't handle Windows", "Windows now has install.ps1"},
		{"internal/service/service.go", "installed, but couldn't start it", "mirrin service install &amp;&amp; mirrin service start", "service install now starts the service itself"},
		{"internal/service/service.go", "a Windows service runs as LocalSystem", "runs your twin as a background service, with no icon", "on Windows it needs an administrator and runs without the tray, voice or a browser you can see"},
		{"cmd/mirrin/selfcare.go", `if goos == "darwin" && wantTray && trayAvailable`, "background service on all three, with a menu bar icon, app indicator or system tray", "only the Mac service shows an icon"},
		{"internal/skills/browser/teach.go", "el.type === 'password'", "Teach mode never records a password.", "teach mode masks password fields, not every password"},
		{"internal/approvals/approvals.go", `case "auto":`, "Sensitive things need a yes", "sensitive files are dangerous, which autonomy.dangerous: auto lets run"},
		{"internal/approvals/floor.go", `return "sensitive file"`, "so they keep asking unless you've let dangerous actions run on their own", "the safety floor makes sensitive files ask whatever autonomy says"},
		{"internal/approvals/floor.go", `return "payment"`, "by default they always come to you", "the safety floor makes payments ask whatever autonomy says"},
		{"internal/memory/forget.go", "own messages are left as they are", "its daily copies too.</p>", "the owner's own messages keep a forgotten fact until /forget"},
		{"CHANGELOG.md", "## 0.2.0", "Every release lists its files' checksums", "0.2.0 shipped without SHA256SUMS"},
		// Wave A: the loopback gap was closed, and devices and encrypted backup
		// are in the code (not yet released).
		{"internal/api/auth.go", "Sec-Fetch-Site", "There is one known gap", "the loopback issue was closed by sameOrigin (WP-01)"},
		{"internal/backup/engine.go", "Package backup keeps end-to-end encrypted snapshots", "encrypted backup, reach and the relay don't exist yet", "encrypted backup is in internal/backup"},
		{"internal/backup/engine.go", "Package backup keeps end-to-end encrypted snapshots", "reach or encrypted backup.</li>", "encrypted backup is in internal/backup"},
		{"internal/devices/devices.go", "KindPWA", "None of them is in the code today: devices", "a key for each device is in internal/devices"},
		{"internal/daemon/alwaysallow.go", `"remember_sensitive": true`, "is set to <code>auto</code>: <code>DecideRisk</code>", "always_allow is the other way a sensitive memory stops asking, and \"yes, always\" can't set it"},
		// Waves B and C: the phone app, push, passkeys, HTTPS reach, the relay,
		// S3 backup and Cloud's own code are in the tree (not yet released).
		{"internal/push/dispatch.go", "func (c Config) IsEnabled", "The lock screen isn't built yet", "lock-screen push is in internal/push"},
		{"internal/stepup/stepup.go", "func ParseLevel", "§7 and §8. Not built yet.", "passkey step-up is in internal/stepup"},
		{"internal/relay/server/connlog.go", "rawKeep", "and the relay don't exist yet", "the relay is in cmd/mirrin-relay and internal/relay/server"},
		{"internal/relay/server/connlog.go", "rawKeep", "<p>Not yet: it's coming in the next release, as its own program", "the relay is in cmd/mirrin-relay and internal/relay/server"},
		{"cmd/mirrin/reach.go", "func useRelay", "Design, not yet built", "reach, the relay and the certificate watch are in the code"},
		{"internal/backup/target_s3.go", "type S3Config struct", "an S3 bucket later", "the S3 target is in internal/backup"},
		{"internal/backup/target_s3.go", "type S3Config struct", "(later, any S3-compatible bucket)", "the S3 target is in internal/backup"},
		{"internal/cloud/egress_test.go", "package cloud", "A test in the code will check that a default install sends nothing", "the zero-egress test exists"},
		{"internal/cloud/client.go", "package cloud", "None of the Cloud code exists yet", "Cloud's client, relay and control plane are in the tree"},
		// The home page's drift, found in review before the launch.
		{"internal/api/ui.html", ".thinking{--c1:var(--accent)", "amber while it thinks", "the screen is blue while it thinks (--accent); amber is --warn, for what needs you"},
		{"internal/api/characters.js", "// Mirrin: a man", "is a little penguin butler", "Mirrin is a man in a dinner jacket (maverickSVG); the penguin is Pickoo's"},
		{"internal/api/characters.js", "// Mirrin: a man", "a dry, loyal butler", "Mirrin is the gentleman who's two steps ahead (internal/persona/mirrin.yaml)"},
		{".github/workflows/release.yml", "dist/Mirrin-*-macos.dmg", "no one-click installer for Mac", "releases carry a Mac app on a disk image"},
		{"internal/channels/voice/setup.go", "Voice setup downloads about 850 MB", "about 1&nbsp;GB", "voice setup downloads about 850 MB"},
		{"internal/api/ui.html", "browser_act:'Act on a website'", `<span class="need-tag">browser_act</span>`, "the card shows the tool's label, not its name"},
		{"internal/stepup/stepup.go", "a passkey (Face ID, a fingerprint, a device PIN)", "ask for Face ID.", "step-up is a passkey: Face ID, a fingerprint or a device PIN"},
		{"CHANGELOG.md", "Downloads with WhatsApp are GPL-3.0", "<dd>MIT</dd>", "the default downloads are GPL-3.0; only the source and the nowhatsapp build are MIT"},
		{"CHANGELOG.md", "Downloads with WhatsApp are GPL-3.0", "is MIT licensed and needs no account", "the default downloads are GPL-3.0"},
		{"CHANGELOG.md", "Downloads with WhatsApp are GPL-3.0", "MIT licence. Made by MavrkAI", "the default downloads are GPL-3.0"},
		{"CHANGELOG.md", "Downloads with WhatsApp are GPL-3.0", ">MIT licence</a>", "the default downloads are GPL-3.0"},
		{"CHANGELOG.md", "Downloads with WhatsApp are GPL-3.0", "the code is MIT on GitHub", "the default downloads are GPL-3.0"},
		// Found in review: what the voice and the free twin's traffic really do.
		{"internal/config/config.go", "Weather shows current conditions on the ambient screen (default on)", "choose Ollama and nothing leaves your machine", "the screen's weather asks Open-Meteo by default (internal/daemon/timekeeping.go)"},
		{"internal/channels/voice/listen.go", "if !c.wakeLive.Load() {", "Say its name and it stops talking", "only the trained detector's loop (wakeLoop) interrupts playback, and the public build has none"},
		{"internal/channels/voice/listen.go", "if !c.wakeLive.Load() {", "it listens for the answer too", "ListenNow works only in the detector's loop, which the public build doesn't run"},
		{"internal/channels/voice/voice.go", `"silence", "1", "0.2", c.soundLevel(), "1", "1.2"`, "A pause mid-sentence doesn't cut you off", "without the detector a recording stops after 1.2 s of silence; only a trailing word a sentence can't end on waits for the rest (unfinished)"},
		{"internal/config/locale.go", `"JPY": 100`, "500 a month out of the box, in your local currency", "the caps are scaled for currencies with small units (capScale)"},
	}
	for _, s := range stale {
		if !strings.Contains(repoFile(t, s.file), s.code) {
			continue // the code changed back; nothing to say
		}
		for _, p := range pages {
			if strings.Contains(page(t, p), s.claim) {
				t.Errorf("%s still says %q, but %s (%s)", p, s.claim, s.why, s.file)
			}
		}
	}
}

// On Windows, `service install` isn't a Windows service (one runs as
// LocalSystem, away from the desktop, with no tray, voice or browser the
// owner can see): it puts a script in the user's own Startup folder that
// starts the tray at sign-in, as the setup program does, and needs no
// administrator. So the page gives every system the same two commands and
// says what the Windows one does.
func TestWindowsSetupMatchesTheCode(t *testing.T) {
	startup := repoFile(t, "internal/service/startup.go")
	if !strings.Contains(startup, "func startupScript(") || !strings.Contains(startup, `"\" tray\r\n"`) {
		t.Fatal("internal/service/startup.go no longer writes a Startup-folder script that starts the tray. Check what `mirrin service install` does on Windows now, then update Install in index.html and this test.")
	}
	if !strings.Contains(repoFile(t, "internal/service/service.go"), "on Windows as an entry in the user's Startup") {
		t.Error("internal/service's package doc no longer says Windows gets a Startup entry; check Install in index.html")
	}
	if !strings.Contains(repoFile(t, "install.ps1"), "mirrin service install           # the tray icon") {
		t.Error("install.ps1 no longer suggests `mirrin service install` for the tray; make Install in index.html suggest what it does now")
	}
	iss := repoFile(t, "packaging/windows/mirrin.iss")
	if !strings.Contains(iss, `WizardIsTaskSelected('startup')`) || !strings.Contains(iss, `" tray'`) {
		t.Error("the Windows setup program no longer offers the tray at sign-in; update Install in index.html")
	}
	if !strings.Contains(iss, "PrivilegesRequired=lowest") {
		t.Error("the Windows setup program now asks for more than a user's rights; index.html says it needs no administrator")
	}

	index := body(t, "index.html")
	reBlock := regexp.MustCompile(`(?s)<div class="code-bar"><span>([^<]+)</span>.*?<pre id="[^"]+"[^>]* data-copy-text="([^"]*)"`)
	windowsService := false
	for _, m := range reBlock.FindAllStringSubmatch(index, -1) {
		label, cmds := m[1], m[2]
		if strings.Contains(label, "Windows") && strings.Contains(cmds, "mirrin service install") {
			windowsService = true
		}
	}
	if !windowsService {
		t.Error("index.html has no block for Windows that runs `mirrin service install`, which install.ps1 suggests")
	}
	for _, says := range []string{"puts a small script in your own Startup folder that starts your twin in the system tray", "so it needs no administrator", "It installs for you alone, so it needs no administrator"} {
		if !strings.Contains(index, says) {
			t.Errorf("index.html doesn't say %q about Windows", says)
		}
	}
	for _, stale := range []string{"needs PowerShell opened as administrator", "starts when the computer does", "no tray icon, no voice, and no browser window you can see"} {
		if strings.Contains(index, stale) {
			t.Errorf("index.html still says %q about `service install` on Windows, but internal/service/startup.go makes a per-user Startup entry for the tray", stale)
		}
	}
}

// The known issue in section 7 is the loopback cookie. When the loopback
// checks land (docs/cloud-design.md §2, WP-01), the page must stop saying so.
func TestKnownIssueStillOpen(t *testing.T) {
	src := map[string]string{}
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		src[filepath.ToSlash(path)] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fixedIn := loopbackFix(src)
	// The loopback disclosure itself, not any known issue: others are
	// disclosed with the same marker where their claim is made.
	idx := body(t, "index.html")
	says := strings.Contains(idx, "served from another port on this computer") || strings.Contains(idx, "could press Approve for you") || strings.Contains(idx, "authAny")
	if fixedIn != "" && says {
		t.Errorf("%s now checks where requests come from, so the loopback issue may be fixed: check it, then remove the Known issue lines from index.html (sections 3 and 7), its header comment and site/README.md", fixedIn)
	}
	if fixedIn == "" && !says {
		t.Error("index.html no longer discloses the loopback cookie issue, but nothing under internal/ checks Sec-Fetch-Site, CrossOriginProtection or the Origin header yet")
	}
}

// reOriginRead is code reading a request's Origin header (setting one on an
// outgoing request, as a WebSocket client might, doesn't count).
var reOriginRead = regexp.MustCompile(`\.Get\("Origin"\)|Header\["Origin"\]`)

// loopbackFix names a file whose code looks like the loopback fix, or "":
// a Sec-Fetch-Site check, net/http's CrossOriginProtection, or an Origin
// header read, in any package under internal/.
func loopbackFix(src map[string]string) string {
	var files []string
	for f, code := range src {
		if strings.Contains(code, "Sec-Fetch-Site") || strings.Contains(code, "CrossOriginProtection") || reOriginRead.MatchString(code) {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return ""
	}
	slices.Sort(files)
	return strings.Join(files, ", ")
}

func TestLoopbackFixIsSeenWherever(t *testing.T) {
	cases := []struct {
		file, code string
		fixed      bool
	}{
		{"internal/api/memory.go", `func (s *Server) authAny(next http.Handler) http.Handler {`, false},
		{"internal/api/origin.go", `if r.Header.Get("Sec-Fetch-Site") == "cross-site" {`, true},
		{"internal/api/api.go", `h = http.NewCrossOriginProtection().Handler(h)`, true},
		{"internal/api/guard/guard.go", `o := r.Header.Get("Origin")`, true},
		{"internal/auth/same.go", `if len(r.Header["Origin"]) > 0 {`, true},
		{"internal/channels/discord/ws.go", `h.Set("Origin", "https://discord.com")`, false},
	}
	for _, c := range cases {
		if got := loopbackFix(map[string]string{c.file: c.code}) != ""; got != c.fixed {
			t.Errorf("loopbackFix(%s: %s) = %v, want %v", c.file, c.code, got, c.fixed)
		}
	}
}

// reTokenCount is the count each page's header comment gives for its name
// tokens ("prints 11 today: 9 in the page and 2 in this comment").
var reTokenCount = regexp.MustCompile(`prints (\d+) today`)

// The product name appears only in its token, so one command renames it
// (site/README.md, "The product name token").
func TestProductNameStaysInItsToken(t *testing.T) {
	const name = "Mirrin"
	allowed := []*regexp.Regexp{
		regexp.MustCompile(`<span class="pn" translate="no">Mirrin</span>`),
		regexp.MustCompile(`/Mirrin`),                                              // URLs keep the repository name
		regexp.MustCompile(`(?s)<title>.*?</title>`),                               // listed in each header comment
		regexp.MustCompile(`<meta [^>]*>`),                                         // likewise
		regexp.MustCompile(`aria-label="Mirrin home"`),                             // likewise
		regexp.MustCompile(`(?s)<script type="application/ld\+json">.*?</script>`), // the home page's JSON-LD, likewise
		regexp.MustCompile(`Mirrin-&lt;version&gt;-windows-setup\.exe`),            // the installers' file names, exception (d)
		regexp.MustCompile(`Mirrin-&lt;version&gt;-macos\.dmg`),
	}
	for _, p := range pages {
		s := body(t, p)
		for _, re := range allowed {
			s = re.ReplaceAllString(s, "")
		}
		if i := strings.Index(s, name); i >= 0 {
			lo, hi := max(0, i-60), min(len(s), i+60)
			t.Errorf("%s names the product outside its token: %q. Use <span class=\"pn\" translate=\"no\">%s</span>.", p, s[lo:hi], name)
		}
		m := reTokenCount.FindStringSubmatch(page(t, p))
		if m == nil {
			t.Errorf("%s's header comment no longer says how many name tokens it has (\"prints N today\")", p)
			continue
		}
		if n, want := strings.Count(page(t, p), `class="pn"`), m[1]; strconv.Itoa(n) != want {
			t.Errorf("%s has %d name tokens; its header comment says %s. Update the comment if this is on purpose.", p, n, want)
		}
	}
	if strings.Contains(read(t, "site.css"), name) {
		t.Error("site.css contains the product name; it must not")
	}
}

// The product's earlier names are gone from the pages, comments included:
// the token test looks for "Mirrin" and would miss a leftover link or
// command. The branch the work happens on isn't named either: what is built
// but not released is "coming in the next release".
func TestOldNameIsGone(t *testing.T) {
	reOld := regexp.MustCompile(`(?i)antbot|openhuman`)
	for _, p := range append(append([]string{}, pages...), "assets/og-card.html", "site.css") {
		s := page(t, p)
		if loc := reOld.FindStringIndex(s); loc != nil {
			lo, hi := max(0, loc[0]-60), min(len(s), loc[1]+60)
			t.Errorf("%s still names an earlier name of the product: %q. It is Mirrin now.", p, s[lo:hi])
		}
	}
}

// fontRanges is the character list the fonts are cut to (site/README.md, "Fonts").
const fontRanges = "U+0020-007E,U+00A0-00FF,U+0131,U+0152-0153,U+2013-2014,U+2018-201A,U+201C-201E,U+2022,U+2026,U+2032-2033,U+2039-203A,U+20AC,U+2122,U+2212"

// systemGlyphs are drawn from the reader's own fonts on purpose.
const systemGlyphs = "→✓✗"

func inFonts(r rune) bool {
	if strings.ContainsRune(systemGlyphs, r) || r == '\n' || r == '\t' || r == '\r' {
		return true
	}
	for _, span := range strings.Split(fontRanges, ",") {
		lo, hi, _ := strings.Cut(strings.TrimPrefix(span, "U+"), "-")
		if hi == "" {
			hi = lo
		}
		a, _ := strconv.ParseUint(lo, 16, 32)
		b, _ := strconv.ParseUint(hi, 16, 32)
		if uint64(r) >= a && uint64(r) <= b {
			return true
		}
	}
	return false
}

var (
	reTag    = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]*>`)
	reEntity = regexp.MustCompile(`&(#x?[0-9a-fA-F]+|[a-zA-Z]+);`)
)

// visibleText is roughly what a page renders: tags and scripts removed,
// numeric entities and the few named ones the pages use decoded.
func visibleText(s string) string {
	s = reTag.ReplaceAllString(s, " ")
	named := map[string]string{"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "nbsp": " "}
	return reEntity.ReplaceAllStringFunc(s, func(e string) string {
		k := e[1 : len(e)-1]
		if v, ok := named[k]; ok {
			return v
		}
		if strings.HasPrefix(k, "#x") {
			if n, err := strconv.ParseUint(k[2:], 16, 32); err == nil {
				return string(rune(n))
			}
		} else if strings.HasPrefix(k, "#") {
			if n, err := strconv.ParseUint(k[1:], 10, 32); err == nil {
				return string(rune(n))
			}
		}
		return e
	})
}

func TestFontsCoverThePages(t *testing.T) {
	for _, p := range pages {
		seen := map[rune]bool{}
		for _, r := range visibleText(body(t, p)) {
			if !inFonts(r) && !seen[r] {
				seen[r] = true
				t.Errorf("%s uses %q (U+%04X), which the site's fonts don't have: reword it, or re-cut the fonts with it (site/README.md, \"Fonts\")", p, r, r)
			}
		}
	}
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(read(t, "site.css"), "")
	reContent := regexp.MustCompile(`content:\s*"((?:[^"\\]|\\.)*)"`)
	reEscape := regexp.MustCompile(`\\([0-9a-fA-F]{1,6}) ?`)
	for _, m := range reContent.FindAllStringSubmatch(css, -1) {
		s := reEscape.ReplaceAllStringFunc(m[1], func(e string) string {
			n, _ := strconv.ParseUint(strings.TrimSpace(e[1:]), 16, 32)
			return string(rune(n))
		})
		for _, r := range s {
			if !inFonts(r) {
				t.Errorf("site.css draws %q (U+%04X) with content:, which the site's fonts don't have", r, r)
			}
		}
	}
	if regexp.MustCompile(`font-style:\s*italic|font:\s*italic`).MatchString(css) {
		t.Error("site.css asks for italic, but the site ships no italic face; the browser would slant the upright letters")
	}
	for _, m := range regexp.MustCompile(`url\("([^"]+)"\)`).FindAllStringSubmatch(css, -1) {
		if _, err := os.Stat(m[1]); err != nil {
			t.Errorf("site.css loads %s, which is missing", m[1])
		}
	}
}

// Local links, images and anchors resolve, on both pages and between them.
func TestLinksResolve(t *testing.T) {
	reID := regexp.MustCompile(`\sid="([^"]+)"`)
	ids := map[string]map[string]bool{}
	for _, p := range pages {
		ids[p] = map[string]bool{}
		for _, m := range reID.FindAllStringSubmatch(body(t, p), -1) {
			if ids[p][m[1]] {
				t.Errorf("%s uses id %q twice", p, m[1])
			}
			ids[p][m[1]] = true
		}
	}
	reRef := regexp.MustCompile(`\s(?:href|src|poster)="([^"]+)"`)
	for _, p := range pages {
		for _, m := range reRef.FindAllStringSubmatch(body(t, p), -1) {
			ref := m[1]
			if strings.HasPrefix(ref, "http") || strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "mailto:") {
				continue
			}
			file, frag, _ := strings.Cut(ref, "#")
			file = strings.TrimPrefix(file, "/") // 404.html's links start at the site's root
			target := p
			if file != "" {
				if _, err := os.Stat(file); err != nil {
					t.Errorf("%s links to %s, which is missing", p, file)
					continue
				}
				target = file
			}
			if frag != "" && ids[target] != nil && !ids[target][frag] {
				t.Errorf("%s links to %s#%s, which has no such id", p, target, frag)
			}
		}
	}
}

func TestReadmeChecksStillFind(t *testing.T) {
	// The README's re-check list names code by pattern; a pattern that finds
	// nothing means the list has gone stale.
	readme := read(t, "README.md")
	i := strings.Index(readme, "Before each launch, also re-check")
	if i < 0 {
		t.Fatal("site/README.md lost its re-check list")
	}
	block := between(t, readme[i:], "site/README.md's re-check list", "```sh", "```")
	reGrep := regexp.MustCompile(`^grep -n '([^']+)' (\S+(?: \S+)*?)\s+#`)
	n := 0
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, ";") {
			continue // two commands on a line; checked by hand
		}
		m := reGrep.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		n++
		pats := strings.Split(strings.NewReplacer(`\*`, "*", `\{`, "{", `\}`, "}").Replace(m[1]), `\|`)
		found := false
		for _, f := range strings.Fields(m[2]) {
			src, err := os.ReadFile(filepath.Join(root, f))
			if errors.Is(err, fs.ErrNotExist) {
				t.Errorf("README check reads %s, which is missing", f)
				continue
			}
			for _, pat := range pats {
				pat = strings.TrimPrefix(pat, "^")
				if strings.Contains(string(src), pat) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("README check %q finds nothing in %s", m[1], m[2])
		}
	}
	if n < 8 {
		t.Fatalf("only %d grep checks parsed from site/README.md", n)
	}
}

// The pricing page's "what we would and couldn't see" tables are copied from
// docs/cloud-trust.md, the single source, cell for cell. And every "can't"
// there comes with its mechanism and a command to check it.
func TestVisibilityTablesMatchTheirSource(t *testing.T) {
	md := repoFile(t, "docs/cloud-trust.md")
	block := between(t, md, "docs/cloud-trust.md", "<!-- visibility:start -->", "<!-- visibility:end -->")
	var tables [][][]string
	var cur [][]string
	for _, line := range append(strings.Split(block, "\n"), "") {
		if !strings.HasPrefix(line, "|") {
			if cur != nil {
				tables = append(tables, cur)
				cur = nil
			}
			continue
		}
		var cells []string
		for _, c := range strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|") {
			cells = append(cells, strings.TrimSpace(c))
		}
		if strings.Trim(strings.Join(cells, ""), "-: ") == "" {
			continue // the header rule
		}
		cur = append(cur, cells)
	}
	if len(tables) != 2 {
		t.Fatalf("docs/cloud-trust.md has %d tables between its visibility markers; the pricing page expects two (can see, can't see)", len(tables))
	}
	space := regexp.MustCompile(`\s+`)
	norm := func(s string) string { return strings.TrimSpace(space.ReplaceAllString(s, " ")) }
	pricing := norm(visibleText(strings.NewReplacer("<code>", "", "</code>", "").Replace(body(t, "pricing.html"))))
	// A cell that names a source file ("The relay's source: `internal/…`")
	// links the file on the page instead of naming it.
	reDocPath := regexp.MustCompile(":?\\s*`(?:internal|cmd|cloud)/[^`]*`")
	cells := 0
	for _, tb := range tables {
		for _, row := range tb {
			for _, c := range row {
				want := norm(strings.ReplaceAll(reDocPath.ReplaceAllString(c, ""), "`", ""))
				cells++
				if !strings.Contains(pricing, want) {
					t.Errorf("pricing.html is missing %q from docs/cloud-trust.md; copy the tables across again", want)
				}
			}
		}
	}
	if cells < 40 {
		t.Fatalf("only %d cells read from docs/cloud-trust.md; the table format changed", cells)
	}
	cant := tables[1]
	if len(cant[0]) != 3 {
		t.Fatalf("the can't-see table in docs/cloud-trust.md should have three columns (what, why, check), not %d", len(cant[0]))
	}
	for _, row := range cant[1:] {
		if len(row) != 3 || row[1] == "" {
			t.Errorf("docs/cloud-trust.md: %q has no mechanism", row[0])
			continue
		}
		if !strings.Contains(row[2], "`mirrin ") && !strings.Contains(row[2], "`openssl") && !strings.Contains(row[2], "`age") {
			t.Errorf("docs/cloud-trust.md: %q has no command to check it", row[0])
		}
	}
}

// The commands the new docs and pages tell people to run are commands the
// CLI has: each "mirrin <command> <subcommand>" and each flag.
func TestDocumentedCommandsExist(t *testing.T) {
	src := ""
	for _, f := range []string{"main.go", "reach.go", "reach_cloud.go", "cloud.go", "backup.go", "backup_s3.go", "backup_cloud.go", "pair.go", "devices.go"} {
		src += repoFile(t, filepath.Join("cmd/mirrin", f))
	}
	// A subcommand must be named in its own command's files, so a bad
	// `reach alarm <x>` can't pass because some other command has <x>.
	own := map[string]string{}
	for cmd, files := range map[string][]string{
		"reach":   {"reach.go", "reach_cloud.go"},
		"cloud":   {"cloud.go"},
		"backup":  {"backup.go", "backup_s3.go", "backup_cloud.go"},
		"restore": {"backup.go", "backup_s3.go", "backup_cloud.go"},
		"devices": {"devices.go", "pair.go"},
		"pair":    {"pair.go"},
		"connect": {"pair.go"},
	} {
		for _, f := range files {
			own[cmd] += repoFile(t, filepath.Join("cmd/mirrin", f))
		}
	}
	sources := map[string]string{}
	for _, f := range []string{"README.md", "docs/cloud.md", "docs/cloud-trust.md", "docs/reach.md", "docs/backup.md"} {
		sources[f] = repoFile(t, f)
	}
	for _, p := range []string{"index.html", "pricing.html", "legal.html", "cloud.html"} {
		sources["site/"+p] = visibleText(body(t, p))
	}
	// A command runs to the end of its code span, table cell or line. Its
	// first word after mirrin is the command; the next is a subcommand when
	// it is a plain word ("connect ab2.…" has none); every --word is a flag.
	reCmd := regexp.MustCompile("mirrin ((?:cloud|reach|backup|devices|pair|restore|connect)\\b[^`|\\n<]*)")
	reWord := regexp.MustCompile(`^[a-z][a-z-]*$`)
	has := func(w string) bool { return strings.Contains(src, `"`+w+`"`) }
	seen, flags := 0, 0
	for where, text := range sources {
		for _, m := range reCmd.FindAllStringSubmatch(text, -1) {
			words := strings.Fields(m[1])
			seen++
			if !has(words[0]) {
				t.Errorf("%s names `mirrin %s`, which cmd/mirrin doesn't have", where, words[0])
			}
			if len(words) > 1 && reWord.MatchString(words[1]) && !strings.Contains(own[words[0]], `"`+words[1]+`"`) && !strings.Contains(own[words[0]], words[0]+" "+words[1]) {
				t.Errorf("%s names `mirrin %s %s`, which cmd/mirrin doesn't have", where, words[0], words[1])
			}
			for _, w := range words[1:] {
				if !strings.HasPrefix(w, "--") {
					continue
				}
				f, _, _ := strings.Cut(strings.TrimRight(w, ".,;:)"), "=")
				flags++
				if !has(f) && !has(strings.TrimLeft(f, "-")) {
					t.Errorf("%s names `mirrin %s … %s`, but cmd/mirrin has no %s flag", where, words[0], f, f)
				}
			}
		}
	}
	if flags < 10 {
		t.Fatalf("only %d flags found in the docs; the pattern no longer matches how they're written", flags)
	}
	if seen < 40 {
		t.Fatalf("only %d commands found in the docs; the pattern no longer matches how they're written", seen)
	}
}

// The config keys the new docs name (`reach.step_up`, `backup.s3.bucket`)
// are keys the config has.
func TestDocumentedConfigKeysExist(t *testing.T) {
	tags := repoFile(t, "internal/config/config.go") + repoFile(t, "internal/config/backup.go") + repoFile(t, "internal/push/dispatch.go")
	reKey := regexp.MustCompile("`((?:reach|push|backup|api|phone|cloud)(?:\\.[a-z0-9_]+)+)`")
	n := 0
	for _, f := range []string{"docs/cloud.md", "docs/cloud-trust.md", "docs/reach.md", "docs/backup.md", "docs/threat-model.md"} {
		for _, m := range reKey.FindAllStringSubmatch(repoFile(t, f), -1) {
			n++
			for _, p := range strings.Split(m[1], ".") {
				if !strings.Contains(tags, `yaml:"`+p+`"`) && !strings.Contains(tags, `yaml:"`+p+`,`) {
					t.Errorf("%s names the setting %s, but the config has no %q key", f, m[1], p)
				}
			}
		}
	}
	if n < 15 {
		t.Fatalf("only %d settings found in the docs; the pattern no longer matches how they're written", n)
	}
}

// cssVar is the value site.css's first block (the light tokens) gives name.
func cssVar(t *testing.T, css, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `:\s*([^;]+);`).FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("site.css no longer defines %s; update this test", name)
	}
	return strings.TrimSpace(m[1])
}

// The drawn presence screen wears the states' colours the real one does:
// ui.html's dark theme, where listening is --ok and thinking and speaking
// are both --accent, and amber (--warn) is for what needs you.
func TestStateColoursMatchTheScreen(t *testing.T) {
	ui := repoFile(t, "internal/api/ui.html")
	dark := between(t, ui, "internal/api/ui.html", ":root{", "}")
	uiVar := func(name string) string {
		m := regexp.MustCompile(regexp.QuoteMeta(name) + `:(#[0-9a-fA-F]+)`).FindStringSubmatch(dark)
		if m == nil {
			t.Fatalf("internal/api/ui.html's first theme block has no %s; update this test", name)
		}
		return m[1]
	}
	for _, rule := range []string{".listening{--c1:var(--ok)", ".thinking{--c1:var(--accent)", ".speaking{--c1:var(--accent)"} {
		if !strings.Contains(ui, rule) {
			t.Fatalf("internal/api/ui.html no longer has %s: the states' colours changed, so check Fig. 1 and Fig. 11 in index.html and the tokens in site.css", rule)
		}
	}
	css := read(t, "site.css")
	for _, c := range []struct{ token, ui string }{{"--listen", "--ok"}, {"--think", "--accent"}, {"--speak", "--accent"}, {"--needs", "--warn"}} {
		if got, want := cssVar(t, css, c.token), uiVar(c.ui); !strings.EqualFold(got, want) {
			t.Errorf("site.css has %s: %s, but the screen's %s is %s (internal/api/ui.html)", c.token, got, c.ui, want)
		}
	}
	if m := regexp.MustCompile(`\.os-t \{[^}]*--c1: (#[0-9a-fA-F]+)`).FindStringSubmatch(css); m == nil || !strings.EqualFold(m[1], uiVar("--accent")) {
		t.Errorf("site.css's thinking orb (.os-t) isn't the screen's --accent %s", uiVar("--accent"))
	}
	index := body(t, "index.html")
	for _, says := range []string{"green while he listens, blue while he thinks and speaks", "Amber is kept for what needs you"} {
		if !strings.Contains(index, says) {
			t.Errorf("index.html no longer says %q; if the states changed, check the screen first", says)
		}
	}
}

// The characters on the site are the screen's: drawn by
// internal/api/characters.js (site/assets/characters/draw.js), and described
// as that file describes them.
func TestCharactersMatchTheCode(t *testing.T) {
	js := repoFile(t, "internal/api/characters.js")
	index := body(t, "index.html")
	claims := []struct{ code, page string }{
		{"in a black dinner jacket", "a man with swept-back dark hair and a short beard, in a black dinner jacket"},
		{"The pin on his lapel", "the pin on his lapel takes the state's colour"},
		{"The sparkle clip in her hair is her only\n// part in the state's colour", "the sparkle clip in Nyra's hair"},
		{"in a navy blazer", "A woman in a navy blazer."},
		{"the bow tie\n// is its only part in the state's colour", "the bow tie on the penguin (Pickoo and any persona without a drawing of its own)"},
		{"everyone else (Pickoo, a pack's) the penguin", "A persona without a drawing of its own gets the penguin."},
	}
	for _, c := range claims {
		if !strings.Contains(js, c.code) {
			t.Errorf("internal/api/characters.js no longer says %q, which index.html relies on for %q; check the drawing and the page", c.code, c.page)
		}
		if !strings.Contains(index, c.page) {
			t.Errorf("index.html no longer says %q; if the description moved, update this test", c.page)
		}
	}
	// Paths that pass through the drawing code unchanged: one from each
	// character, so a redrawn character shows up here.
	marks := map[string]string{
		"mirrin":  "M84,138h32l-16,64z",
		"nyra":    `class="smile"`,
		"penguin": `<path d="M100 152 L79 141 Q73 152 79 163 Z"`,
	}
	idle := strings.Contains(repoFile(t, "internal/api/ui.html"), ".idle{--c1:#6d7cff")
	for name, mark := range marks {
		if !strings.Contains(js, mark) {
			t.Fatalf("internal/api/characters.js no longer draws %q; run node site/assets/characters/draw.js and pick a new mark for %s here", mark, name)
		}
		svg := read(t, "assets/characters/"+name+".svg")
		if !strings.Contains(svg, mark) {
			t.Errorf("assets/characters/%s.svg isn't the screen's drawing any more; run node site/assets/characters/draw.js", name)
		}
		if !idle || !strings.Contains(svg, "--c1:#6d7cff") {
			t.Errorf("assets/characters/%s.svg isn't in the screen's resting colour (.idle in internal/api/ui.html); run node site/assets/characters/draw.js", name)
		}
	}
	hero := between(t, page(t, "index.html"), "index.html", "<!-- mirrin:start -->", "<!-- mirrin:end -->")
	if !strings.Contains(hero, marks["mirrin"]) || !strings.Contains(hero, "var(--c1)") {
		t.Error("Fig. 1 in index.html isn't Mirrin as the screen draws him; run node site/assets/characters/draw.js")
	}
}

// The personas the page shows are the ones the code ships, with their voices.
func TestPersonasMatchTheCode(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(root, "internal/persona/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Errorf("internal/persona ships %d personas; index.html (section 2) shows three", len(files))
	}
	// a card for each persona the code ships, and none for one it doesn't
	if cards := strings.Count(between(t, body(t, "index.html"), "index.html", `<ul class="cast">`, "</ul>"), "<li"); cards != len(files) {
		t.Errorf("index.html's personas section has %d cards, but internal/persona ships %d personas", cards, len(files))
	}
	if tiles := strings.Count(between(t, body(t, "index.html"), "index.html", `<div class="hero-cast">`, "</ul>"), "<li"); tiles != len(files)-1 {
		t.Errorf("the hero shows %d others beside the default persona, but internal/persona ships %d personas", tiles, len(files))
	}
	yaml := ""
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		yaml += string(b) + "\n"
	}
	index := body(t, "index.html")
	// the default persona carries the product's name, so his is the name token
	for _, name := range []string{`<span class="pn" translate="no">Mirrin</span>`, "Nyra", "Pickoo"} {
		if !strings.Contains(index, "<h3>"+name+"</h3>") {
			t.Errorf("index.html's personas section has no card for %s", name)
		}
	}
	for _, v := range []struct{ id, says string }{
		{"bm_george", "Voice: George, a British man"},
		{"af_nova", "Voice: Nova, an American woman"},
		{"am_puck", "Voice: Puck, pitched up"},
	} {
		if !strings.Contains(yaml, "voice: "+v.id+"\n") {
			t.Errorf("no persona in internal/persona speaks with %s any more; index.html says %q", v.id, v.says)
		}
		if !strings.Contains(index, v.says) {
			t.Errorf("index.html no longer says %q (the persona that speaks with %s)", v.says, v.id)
		}
	}
	if pickoo := repoFile(t, "internal/persona/pickoo.yaml"); !strings.Contains(pickoo, "voice: am_puck") || !strings.Contains(pickoo, "voice_pitch:") {
		t.Error("Pickoo's voice is no longer am_puck pitched up; update the personas section in index.html")
	}
	m := regexp.MustCompile(`tagline: (.+)\.\n`).FindStringSubmatch(repoFile(t, "internal/persona/mirrin.yaml"))
	if m == nil {
		t.Fatal("internal/persona/mirrin.yaml has no tagline")
	}
	if tag := strings.ToLower(m[1][:1]) + m[1][1:]; !strings.Contains(index, tag) {
		t.Errorf("Mirrin's tagline is now %q (internal/persona/mirrin.yaml); index.html should use it", m[1])
	}
}

// The bubbles on the home page's persona cards (and the hero's three tiles)
// say what each persona really opens with: its greeting in internal/persona,
// with the optional form of address ("{, address}") left out.
func TestGreetingsMatchThePersonas(t *testing.T) {
	reGreeting := regexp.MustCompile(`(?m)^greeting: "?(.+?)"?$`)
	reSay := regexp.MustCompile(`<q class="say"[^>]*>(?:<span class="sr-only">[^<]*</span>)?([^<]+)</q>`)
	said := map[string]int{}
	for _, m := range reSay.FindAllStringSubmatch(body(t, "index.html"), -1) {
		said[strings.TrimSpace(m[1])]++
	}
	// file, and how many bubbles carry it: a card each, and a hero tile for all but the default
	for file, n := range map[string]int{"mirrin": 1, "nyra": 2, "pickoo": 2} {
		m := reGreeting.FindStringSubmatch(repoFile(t, "internal/persona/"+file+".yaml"))
		if m == nil {
			t.Fatalf("internal/persona/%s.yaml has no greeting; update the bubbles in index.html and this test", file)
		}
		g := strings.ReplaceAll(m[1], "{, address}", "")
		if said[g] != n {
			t.Errorf("index.html shows %q in %d greeting bubbles, want %d: it is what internal/persona/%s.yaml says", g, said[g], n, file)
		}
		delete(said, g)
	}
	for g := range said {
		t.Errorf("index.html has a greeting bubble %q that no persona says; use the greeting from internal/persona", g)
	}
}

// The site calls her Nyra. The code's old name for her, and what it stood
// for, appear nowhere a reader can see: not in the pages, their comments,
// the social card or the drawings. The pattern is written so that it never
// spells the old name itself: scripts/publish refuses a public copy that
// does, this file included.
func TestNyraHasHerName(t *testing.T) {
	reOld := regexp.MustCompile(`(?i)s[i]fra|Super[ ]Intelligent[ ]Female[ ]Robot`)
	files := append(append([]string{}, pages...), "assets/og-card.html", "assets/icon-card.html", "README.md", "robots.txt", "sitemap.xml", "favicon.svg", "site.css", "assets/characters/mirrin.svg", "assets/characters/nyra.svg", "assets/characters/penguin.svg")
	for _, p := range files {
		s := page(t, p)
		if loc := reOld.FindStringIndex(s); loc != nil {
			lo, hi := max(0, loc[0]-60), min(len(s), loc[1]+60)
			t.Errorf("%s names the persona by her old name: %q. She is Nyra on the site.", p, s[lo:hi])
		}
	}
	if !strings.Contains(body(t, "index.html"), "<h3>Nyra</h3>") {
		t.Error("index.html no longer introduces Nyra")
	}
}

// The public build has no trained wake-word model (its training data can't
// be used in a product; a licence-clean one is WP-24, docs/cloud-design.md
// §12). Every persona hears its name by transcribing on the computer, a
// beat slower. The pages must not promise a detector.
func TestNoTrainedWakeWordClaimed(t *testing.T) {
	if !strings.Contains(repoFile(t, "docs/cloud-design.md"), "**WP-24** rebuilds the pipeline") {
		t.Fatal("docs/cloud-design.md §12 no longer plans WP-24's licence-clean wake model; check what the public build ships, then the voice copy in index.html and this test")
	}
	if !strings.Contains(repoFile(t, "internal/channels/voice/voice.go"), "func stripWakeWith(") {
		t.Fatal("internal/channels/voice/voice.go no longer matches the name in the transcript (stripWakeWith); check the voice copy in index.html")
	}
	// Barge-in by name and answering an unprompted question need the
	// detector's loop (wakeLoop: interruptTurn, ListenNow); transcribeLoop,
	// which every persona uses in the public build, does neither.
	reClaim := regexp.MustCompile(`(?i)nothing is transcribed|transcribed or stored until|listens for the sound|a small model listens|hey_maverick|retrain the wake|on-device (wake|detector)|wake-word detector|openWakeWord|trained (wake|detector)|stops talking|listens for the answer`)
	for _, p := range pages {
		s := body(t, p)
		for _, loc := range reClaim.FindAllStringIndex(s, -1) {
			lo, hi := max(0, loc[0]-60), min(len(s), loc[1]+60)
			if strings.Contains(strings.ToLower(s[lo:hi]), "no trained wake-word model") {
				continue
			}
			t.Errorf("%s claims what only a trained wake-word detector does: %q. The public build hears its name by transcribing (docs/cloud-design.md §12).", p, s[lo:hi])
		}
	}
	index := body(t, "index.html")
	for _, says := range []string{"The first release has no trained wake-word model", "every persona, <span class=\"pn\" translate=\"no\">Mirrin</span> included, answers a beat slower", "by transcribing what it hears on your computer", "none of it leaves your machine"} {
		if !strings.Contains(index, says) {
			t.Errorf("index.html no longer says %q", says)
		}
	}
}

// The status chips on the home, Cloud, Protocols and pricing pages use one
// vocabulary, in customers' words (site/README.md, "How the truth is kept"):
// no Mirrin release is out, and the site goes up with the first one, so what
// is built is "Available now" and nothing is "coming in the next release".
// Built but not yet checked on real phones is "Being tested", for as long as
// docs/pwa-testing.md's matrix hasn't been run.
func TestStatusChips(t *testing.T) {
	allowed := map[string]bool{
		"Available now": true, "Today": true, "Free, for good": true,
		"Being tested": true,
		"Planned":      true, "Planned · not on sale": true, "Cloud: planned, not on sale": true,
		"Drafts · not in force": true,
	}
	reChip := regexp.MustCompile(`<span class="st(?: [a-z ]+)?">([^<]+)</span>`)
	for p, least := range map[string]int{"index.html": 12, "cloud.html": 12, "pricing.html": 8, "protocols.html": 12} {
		s := body(t, p)
		n := 0
		for _, m := range reChip.FindAllStringSubmatch(s, -1) {
			n++
			if !allowed[m[1]] {
				t.Errorf("%s has a status chip %q, which isn't in the site's vocabulary (site/README.md, \"How the truth is kept\")", p, m[1])
			}
		}
		if n < least {
			t.Fatalf("only %d status chips found in %s; the pattern no longer matches how they're written", n, p)
		}
		if i := strings.Index(strings.ToLower(s), "next release"); i >= 0 {
			t.Errorf("%s still says %q: no Mirrin release is out, so what's built is in the first release", p, s[max(0, i-60):min(len(s), i+40)])
		}
	}
	matrix := repoFile(t, "docs/pwa-testing.md")
	if !strings.Contains(matrix, "Unverified") {
		t.Fatal("docs/pwa-testing.md's phone matrix has been run: if it passed, mark the Home Screen app, lock-screen approvals and passkeys \"Available now\" on every page, and update this test")
	}
	index := body(t, "index.html")
	for _, says := range []string{`<span class="st small next">Being tested</span>`, "it is still being tested on real phones", "Being tested on phones:</b> lock-screen approvals"} {
		if !strings.Contains(index, says) {
			t.Errorf("docs/pwa-testing.md's phone matrix hasn't been run, but index.html no longer says %q about the lock screen", says)
		}
	}
	for p, says := range map[string]string{
		"cloud.html":   `<span class="st small next">Being tested</span> <strong>Lock-screen approvals.</strong>`,
		"pricing.html": `<span class="soon">Being tested on phones.</span> Your machine sends them itself`,
	} {
		if !strings.Contains(body(t, p), says) {
			t.Errorf("docs/pwa-testing.md's phone matrix hasn't been run, but %s no longer says the lock screen is being tested (%q)", p, says)
		}
	}
}

// The licence is stated whole wherever it's stated: the source is MIT, the
// default downloads carry WhatsApp (libsignal, GPL-3.0) and are GPL-3.0,
// and an MIT build comes without it (with MPL-2.0 yamux).
func TestLicenceStatedWhole(t *testing.T) {
	if !strings.Contains(repoFile(t, "CHANGELOG.md"), "The source stays MIT. Downloads with WhatsApp link GPL-3.0 code, so they are GPL-3.0 as a whole; builds made with `-tags nowhatsapp` are MIT.") {
		t.Fatal("CHANGELOG.md's Governance no longer states the licences as index.html does; check them, then the page")
	}
	gomod := repoFile(t, "go.mod")
	for _, dep := range []string{"go.mau.fi/libsignal", "github.com/hashicorp/yamux"} {
		if !strings.Contains(gomod, dep) {
			t.Errorf("go.mod no longer has %s, which index.html's licence note names", dep)
		}
	}
	index := body(t, "index.html")
	for _, says := range []string{`MIT source, <a href="#licence"><span class="nw">GPL-3.0</span> downloads</a>`, "so those downloads are GPL-3.0 as a whole", "an MIT build of the program without WhatsApp (it contains one MPL-2.0 library, yamux)", "Source MIT; default downloads GPL-3.0"} {
		if !strings.Contains(index, says) {
			t.Errorf("index.html no longer says %q", says)
		}
	}
	// The pricing page tells the whole story in plain words, contributions
	// included: what goes into the repository is MIT (CONTRIBUTING.md); a
	// pack in its own repository keeps its author's licence, so the site
	// never says packs are MIT.
	if !strings.Contains(repoFile(t, "install.sh"), "MIRRIN_BUILD=nowhatsapp") {
		t.Error("install.sh no longer installs the MIT build with MIRRIN_BUILD=nowhatsapp; pricing.html's licence section says it does")
	}
	if !strings.Contains(repoFile(t, "CONTRIBUTING.md"), "MIT license applies to all contributions") {
		t.Error("CONTRIBUTING.md no longer says contributions are MIT; check pricing.html's licence section")
	}
	pricing := body(t, "pricing.html")
	for _, says := range []string{"<strong>The source is MIT.</strong>", "so those downloads are GPL-3.0 as a whole", "It contains one MPL-2.0 library (yamux", "<code>MIRRIN_BUILD=nowhatsapp</code>", "is MIT like the rest", "under the licence you give it there"} {
		if !strings.Contains(pricing, says) {
			t.Errorf("pricing.html's licence section no longer says %q", says)
		}
	}
	for _, p := range pages {
		if regexp.MustCompile(`(?i)packs? (are|is) (under )?(the )?MIT`).MatchString(body(t, p)) {
			t.Errorf("%s says packs are MIT, but a pack in its own repository is under its author's licence", p)
		}
	}
}

// marked returns what sits between <!-- name:start --> and <!-- name:end -->
// in a page, with each line's indent taken off.
func marked(t *testing.T, p, name string) string {
	t.Helper()
	s := between(t, page(t, p), p, "<!-- "+name+":start -->", "<!-- "+name+":end -->")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// The Free list, the tier cards and the offer strip appear on more than one
// page. They are copies of one text, so a change to one must be made to all
// of them (pricing.html's header comment says so).
func TestOneCopyOfEachList(t *testing.T) {
	for _, c := range []struct {
		name  string
		pages []string
	}{
		{"free-list", []string{"index.html", "pricing.html"}},
		{"offer-strip", []string{"index.html", "cloud.html", "pricing.html"}},
		{"build-on-it", pages},
	} {
		want := marked(t, c.pages[0], c.name)
		for _, p := range c.pages[1:] {
			if got := marked(t, p, c.name); got != want {
				t.Errorf("%s's %s differs from %s's; they are one text, so make them the same again", p, c.name, c.pages[0])
			}
		}
	}
	// The tier cards are on the pricing page only: the Cloud page gives the
	// prices in brief and links there, so the two pages don't repeat a section.
	marked(t, "pricing.html", "tiers")
	for _, p := range pages {
		if p != "pricing.html" && strings.Contains(page(t, p), "<!-- tiers:start -->") {
			t.Errorf("%s has the tier cards again; they are on pricing.html only (link there instead)", p)
		}
	}
	// Each page has its own headline: the home page, the Cloud page and the
	// pricing page once shared one, so a click from one to the next looked
	// like the same page.
	seen := map[string]string{}
	reH1 := regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>`)
	for _, p := range pages {
		m := reH1.FindStringSubmatch(body(t, p))
		if m == nil {
			t.Errorf("%s has no h1", p)
			continue
		}
		h := strings.Join(strings.Fields(visibleText(m[1])), " ")
		if q, ok := seen[h]; ok {
			t.Errorf("%s and %s have the same h1, %q; give each page its own", q, p, h)
		}
		seen[h] = p
	}
}

// reFAQ is one FAQ entry: its question and its answer.
var reFAQ = regexp.MustCompile(`(?s)<summary>(.*?)</summary><div class="a">(.*?)</div></details>`)

// A question asked on both the home page and the Cloud page has one answer.
// The Cloud page adds its sources (class="src"); the answer itself is the same.
func TestSharedFAQAnswersMatch(t *testing.T) {
	reSrc := regexp.MustCompile(`(?s)<p class="src">.*?</p>`)
	answers := func(p string) map[string]string {
		m := map[string]string{}
		for _, f := range reFAQ.FindAllStringSubmatch(body(t, p), -1) {
			m[f[1]] = strings.Join(strings.Fields(reSrc.ReplaceAllString(f[2], "")), " ")
		}
		return m
	}
	index, cloud := answers("index.html"), answers("cloud.html")
	shared := 0
	for q, a := range index {
		c, ok := cloud[q]
		if !ok {
			continue
		}
		shared++
		if a != c {
			t.Errorf("index.html and cloud.html answer %q differently; copy one answer to both", q)
		}
	}
	if shared < 6 {
		t.Fatalf("only %d questions found on both pages; the FAQ markup changed", shared)
	}
}

// Following along needs no account: every "watch releases" link goes to the
// releases page, and the Cloud and pricing pages offer the Atom feed too.
func TestFollowAlongNeedsNoAccount(t *testing.T) {
	reWatch := regexp.MustCompile(`<a [^>]*href="([^"]+)"[^>]*>(?:Follow along: )?[Ww]atch releases on GitHub`)
	for _, p := range pages {
		for _, m := range reWatch.FindAllStringSubmatch(body(t, p), -1) {
			if m[1] != "https://github.com/MavrkAI/Mirrin/releases" {
				t.Errorf("%s's \"watch releases on GitHub\" goes to %s; point it at the releases page", p, m[1])
			}
		}
	}
	for _, p := range []string{"cloud.html", "pricing.html", "index.html"} {
		if !strings.Contains(body(t, p), `href="https://github.com/MavrkAI/Mirrin/releases.atom"`) {
			t.Errorf("%s no longer links the releases feed, which needs no account", p)
		}
	}
}

// What the pages say about Cloud's limits and lapse is what Cloud's code
// does: the speed limit is per tunnel (every connection to an address
// shares it), a pass lasts at most seven days past the paid-through date,
// and backups can be fetched for 90 days after it.
func TestCloudTermsMatchTheCode(t *testing.T) {
	code := []struct{ file, has, why string }{
		{"internal/relay/server/config.go", "bits per second per tunnel", "20 Mbit/s for your address, shared by all its connections"},
		{"internal/relay/server/config.go", "BPS: 20e6", "20 Mbit/s"},
		{"internal/relay/server/config.go", "Abuse{DistinctNetsPerDay: 200, SuspendFor: 7 * 24 * time.Hour}", "more than 200 networks in a day pauses an address for seven days"},
		{"cloud/internal/server/server.go", "entGrace      = 7 * 24 * time.Hour", "up to seven days past the end of your paid period"},
		{"cloud/internal/server/backup.go", "readGrace = 90 * 24 * time.Hour", "90 days after the paid period ends"},
		{"cloud/internal/store/schema.sql", "Handles are never reassigned", "your address would never be given to anyone else"},
	}
	for _, c := range code {
		if !strings.Contains(repoFile(t, c.file), c.has) {
			t.Errorf("%s no longer has %q, which the Cloud, pricing and legal pages rely on for %q; check them", c.file, c.has, c.why)
		}
	}
	says := map[string][]string{
		"pricing.html": {"up to 20&nbsp;Mbit/s for your address, shared by all its connections", "would be paused for seven days", "for up to seven days past the end of your paid period", "for 90 days after it you could still list and download every backup"},
		"cloud.html":   {"up to 20&nbsp;Mbit/s for your address, shared by all its connections", "for up to seven days past the end of your paid period", "for 90 days after it you could still list and download every backup"},
		"legal.html":   {"up to 20&nbsp;Mbit/s for your address, shared by all its connections", "pauses it automatically for seven days", "keeps routing for up to 7 days after it", "For 90 days after it you can still list and download"},
	}
	for p, ss := range says {
		for _, s := range ss {
			if !strings.Contains(body(t, p), s) {
				t.Errorf("%s no longer says %q; if the wording moved, update this test", p, s)
			}
		}
	}
	for _, p := range pages {
		s := body(t, p)
		for _, stale := range []string{"per connection", "held for 12 months", "held 12 months", "subscription number", "paused for review"} {
			if strings.Contains(s, stale) {
				t.Errorf("%s says %q, which isn't what Cloud's code does (per tunnel; never reassigned; an account number; a seven-day pause)", p, stale)
			}
		}
	}
}

// collapse is s with every run of white space made one space, so a YAML
// file and the page's copy of it compare by their words.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// The Protocols page shows real files: the example is the morning briefing
// as it ships, the gallery has one card per starter with that file's
// description, schedule and requires, and the rain check is the quickstart's.
func TestProtocolsPageMatchesTheFiles(t *testing.T) {
	want := repoFile(t, "examples/protocols/morning-briefing.yaml")
	if got := visibleText(marked(t, "protocols.html", "example")); collapse(got) != collapse(want) {
		t.Errorf("protocols.html's example isn't examples/protocols/morning-briefing.yaml any more; copy the file in again.\npage: %s\nfile: %s", collapse(got), collapse(want))
	}
	rain := collapse(visibleText(marked(t, "protocols.html", "rain-check")))
	if !strings.Contains(collapse(repoFile(t, "docs/protocols/quickstart.md")), rain) {
		t.Error("protocols.html's rain check is no longer the one in docs/protocols/quickstart.md; copy it from there")
	}

	files, err := filepath.Glob(filepath.Join(root, "examples/protocols/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 6 {
		t.Errorf("examples/protocols has %d starters; protocols.html says six (hero, section 2 and section 6)", len(files))
	}
	page := body(t, "protocols.html")
	if n := strings.Count(page, `<li class="pstarter"`); n != len(files) {
		t.Errorf("protocols.html shows %d starter cards; examples/protocols has %d files", n, len(files))
	}
	field := func(src, key string) string {
		m := regexp.MustCompile(`(?m)^` + key + `:\s*(.*)$`).FindStringSubmatch(src)
		if m == nil {
			return ""
		}
		return strings.Trim(strings.TrimSpace(m[1]), `"`)
	}
	for _, f := range files {
		src := read(t, f)
		name := field(src, "name")
		card := between(t, page, "protocols.html", `data-protocol="`+name+`"`, "</li>")
		if d := field(src, "description"); !strings.Contains(card, "“"+d+"”") {
			t.Errorf("protocols.html's %s card doesn't quote its description %q", name, d)
		}
		if sched := field(src, "schedule"); sched == "" {
			if !strings.Contains(card, "<dd>When you ask</dd>") {
				t.Errorf("%s has no schedule, so its card should say When you ask", name)
			}
		} else if !strings.Contains(card, "<code>"+sched+"</code>") {
			t.Errorf("protocols.html's %s card doesn't show its schedule %s", name, sched)
		}
		for _, r := range strings.Split(strings.Trim(field(src, "requires"), "[]"), ",") {
			if r = strings.TrimSpace(r); r != "" && !strings.Contains(card, "<code>"+r+"</code>") {
				t.Errorf("protocols.html's %s card doesn't list %s, which it requires", name, r)
			}
		}
	}
}

// Every `mirrin` command on the Protocols page is one cmd/mirrin has: the
// command is a case in main's switch, a `mirrin protocols` subcommand is a
// case in protocolsCmd, another command's subcommand is named in cmd/mirrin,
// and every flag is there too.
func TestProtocolsPageCommandsExist(t *testing.T) {
	main := repoFile(t, "cmd/mirrin/main.go")
	protocolsCmd := between(t, main, "cmd/mirrin/main.go", "func protocolsCmd(", "\n}\n")
	all := ""
	files, err := filepath.Glob(filepath.Join(root, "cmd/mirrin/*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			all += read(t, f)
		}
	}
	isCase := func(src, w string) bool {
		return regexp.MustCompile(`case [^:\n]*"` + regexp.QuoteMeta(w) + `"`).MatchString(src)
	}
	// A command runs to the end of its code span, or of its line in a block.
	reSpan := regexp.MustCompile(`(?s)<code>(.*?)</code>|<pre[^>]*>(.*?)</pre>`)
	reCmd := regexp.MustCompile(`(?m)\bmirrin ([a-z][^\n]*)`)
	reWord := regexp.MustCompile(`^[a-z][a-z-]*$`)
	var cmds []string
	for _, sp := range reSpan.FindAllStringSubmatch(body(t, "protocols.html"), -1) {
		for _, m := range reCmd.FindAllStringSubmatch(visibleText(sp[1]+sp[2]), -1) {
			cmds = append(cmds, m[1])
		}
	}
	n := 0
	for _, c := range cmds {
		words := strings.Fields(c)
		n++
		if !isCase(main, words[0]) {
			t.Errorf("protocols.html names `mirrin %s`, which cmd/mirrin/main.go doesn't have", words[0])
			continue
		}
		if len(words) > 1 && reWord.MatchString(words[1]) {
			if words[0] == "protocols" && !isCase(protocolsCmd, words[1]) {
				t.Errorf("protocols.html names `mirrin protocols %s`, which protocolsCmd doesn't have", words[1])
			}
			if words[0] != "protocols" && !strings.Contains(all, `"`+words[1]+`"`) {
				t.Errorf("protocols.html names `mirrin %s %s`, which cmd/mirrin doesn't have", words[0], words[1])
			}
		}
		for _, w := range words[1:] {
			if f := strings.TrimRight(w, ".,;:)"); strings.HasPrefix(f, "--") && !strings.Contains(all, `"`+f+`"`) {
				t.Errorf("protocols.html names `mirrin %s … %s`, but cmd/mirrin has no %s flag", words[0], f, f)
			}
		}
	}
	// The page's flags are in prose, not after the command.
	for _, flag := range regexp.MustCompile(`<code>(--[a-z-]+)</code>`).FindAllStringSubmatch(body(t, "protocols.html"), -1) {
		if !strings.Contains(all, `"`+flag[1]+`"`) {
			t.Errorf("protocols.html names the flag %s, which cmd/mirrin doesn't have", flag[1])
		}
	}
	if n < 10 {
		t.Fatalf("only %d `mirrin` commands found on protocols.html; the pattern no longer matches how they're written", n)
	}
}

// githubSlug is the anchor GitHub gives a Markdown heading.
func githubSlug(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Every link into the repository on GitHub (docs/protocols/*.md, the
// footer's Build on it, and the rest) points at a file that exists here,
// and at a heading that exists in it.
func TestRepositoryLinksExist(t *testing.T) {
	reLink := regexp.MustCompile(`href="https://github\.com/MavrkAI/Mirrin/(?:blob|tree)/main/([^"#]+)(?:#([^"]+))?"`)
	reHeading := regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*$`)
	protocolDocs := 0
	for _, p := range pages {
		for _, m := range reLink.FindAllStringSubmatch(body(t, p), -1) {
			rel, frag := m[1], m[2]
			if strings.HasPrefix(rel, "docs/protocols/") {
				protocolDocs++
			}
			b, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Errorf("%s links to %s on GitHub, which isn't in the repository", p, rel)
				continue
			}
			if frag == "" || !strings.HasSuffix(rel, ".md") {
				continue
			}
			found := false
			for _, h := range reHeading.FindAllStringSubmatch(string(b), -1) {
				if githubSlug(strings.NewReplacer("`", "", "*", "").Replace(h[1])) == frag {
					found = true
				}
			}
			if !found {
				t.Errorf("%s links to %s#%s, but %s has no such heading", p, rel, frag, rel)
			}
		}
	}
	if protocolDocs < 8 {
		t.Fatalf("only %d links to docs/protocols found; the pattern no longer matches how the pages link", protocolDocs)
	}
}

// The site is ready to publish at https://mirrin.app (site/README.md, "Deploy
// with GitHub Pages"): the custom domain, a canonical address and absolute
// social-card URLs on every page, short descriptions, a sitemap that lists
// exactly the pages that may be indexed, real icons, honest structured data,
// fonts that keep the OFL's Reserved Font Name off modified copies, and a
// workflow that leaves the working files out of what it publishes.
func TestReadyToPublish(t *testing.T) {
	const site = "https://mirrin.app/"
	if got := strings.TrimSpace(read(t, "CNAME")); got != "mirrin.app" {
		t.Errorf("CNAME says %q; the site's domain is mirrin.app", got)
	}
	meta := func(s, attr, key string) string {
		m := regexp.MustCompile(`<meta ` + attr + `="` + regexp.QuoteMeta(key) + `" content="([^"]*)">`).FindStringSubmatch(s)
		if m == nil {
			return ""
		}
		return m[1]
	}
	reCanonical := regexp.MustCompile(`<link rel="canonical" href="([^"]+)">`)
	var indexable []string
	for _, p := range pages {
		s := body(t, p)
		for _, icon := range []string{`<link rel="icon" href="(/?)favicon.svg" type="image/svg\+xml">`, `<link rel="apple-touch-icon" href="(/?)apple-touch-icon.png">`} {
			if !regexp.MustCompile(icon).MatchString(s) {
				t.Errorf("%s has no %s", p, icon)
			}
		}
		noindex := strings.Contains(s, `<meta name="robots" content="noindex">`)
		if p == "404.html" {
			if !noindex {
				t.Error("404.html must be noindex")
			}
			continue
		}
		want := site + p
		if p == "index.html" {
			want = site
		}
		if m := reCanonical.FindStringSubmatch(s); m == nil || m[1] != want {
			t.Errorf("%s's canonical link should be %s", p, want)
		}
		if got := meta(s, "property", "og:url"); got != want {
			t.Errorf("%s's og:url is %q; want %s", p, got, want)
		}
		for _, k := range []struct{ attr, key string }{{"property", "og:image"}, {"name", "twitter:image"}} {
			if got := meta(s, k.attr, k.key); got != site+"assets/og.png" {
				t.Errorf("%s's %s is %q; social cards need the absolute %sassets/og.png", p, k.key, got, site)
			}
		}
		for _, k := range []string{"og:image:alt", "og:description"} {
			if meta(s, "property", k) == "" {
				t.Errorf("%s has no %s", p, k)
			}
		}
		d := meta(s, "name", "description")
		if n := len([]rune(visibleText(d))); n == 0 || n > 160 {
			t.Errorf("%s's meta description is %d characters; keep it to 160 or fewer", p, n)
		}
		if !noindex {
			indexable = append(indexable, want)
		}
	}
	var listed []string
	for _, m := range regexp.MustCompile(`<loc>([^<]+)</loc>`).FindAllStringSubmatch(read(t, "sitemap.xml"), -1) {
		listed = append(listed, m[1])
	}
	slices.Sort(listed)
	slices.Sort(indexable)
	if !slices.Equal(listed, indexable) {
		t.Errorf("sitemap.xml lists %v; the pages that may be indexed are %v", listed, indexable)
	}
	if !strings.Contains(read(t, "robots.txt"), "Sitemap: "+site+"sitemap.xml") {
		t.Error("robots.txt no longer points at the sitemap")
	}
	for _, f := range []string{"favicon.svg", "apple-touch-icon.png", "assets/og.png"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s is missing", f)
		}
	}

	// Structured data: what the page says, and nothing it can't back up.
	ld := between(t, page(t, "index.html"), "index.html", `<script type="application/ld+json">`, "</script>")
	for _, want := range []string{`"@type": "SoftwareApplication"`, `"price": "0"`, `"isAccessibleForFree": true`, `"operatingSystem": "macOS, Windows, Linux"`} {
		if !strings.Contains(ld, want) {
			t.Errorf("index.html's JSON-LD no longer has %s", want)
		}
	}
	for _, never := range []string{"aggregateRating", "review", "softwareVersion"} {
		if strings.Contains(ld, never) {
			t.Errorf("index.html's JSON-LD has %s; there are no ratings or reviews to cite, and no release is named on the site", never)
		}
	}

	// IBM Plex is under the OFL with Reserved Font Name "Plex": the cut-down
	// copies served here must not be called Plex (site/README.md, "Fonts").
	css := read(t, "site.css")
	for _, m := range regexp.MustCompile(`font-family:\s*"([^"]+)"`).FindAllStringSubmatch(css, -1) {
		if strings.Contains(m[1], "Plex") {
			t.Errorf("site.css names a font %q; Plex is a Reserved Font Name, which a modified copy may not use", m[1])
		}
	}
	if regexp.MustCompile(`(?i)assets/fonts/[\w.-]*plex`).MatchString(css) {
		t.Error("site.css loads a font file named for Plex; the modified copies are Colophon")
	}

	// The Pages workflow publishes site/ without the working files.
	wf := repoFile(t, ".github/workflows/pages.yml")
	for _, f := range []string{"README.md", "site_test.go", "assets/og-card.html", "assets/icon-card.html", "assets/characters/draw.js"} {
		if !strings.Contains(wf, "site/"+f) {
			t.Errorf(".github/workflows/pages.yml no longer leaves out site/%s", f)
		}
	}
}
