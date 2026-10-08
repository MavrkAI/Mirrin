# The Mirrin website

Five static pages and a 404 page, one stylesheet, no build step and no JavaScript framework. It is published at https://mirrin.app by `.github/workflows/pages.yml`.

| File | What it is |
|---|---|
| `index.html` | Part one: the twin. The short version first: the line, Mirrin on the drawn presence screen with Nyra and Pickoo beneath him, and Free and Cloud in two lines, all in the first screen; what it does in three cards and how it works in three steps, each linking down. Then the long version: text, personas, voice, approvals, memory, routines, ownership, safety, Free vs Cloud, install, what's inside, eight questions (seven shared with the Cloud page, plus what the model will cost). |
| `cloud.html` | Part two: Cloud, planned. Status, Free and Cloud in two lines, promises, the free routes already built, the planned prices in brief (the tier cards are on the pricing page), how reach and backup would work, what we would and couldn't see, verifying, leaving, the shutdown pledge, fifteen questions, follow along. |
| `protocols.html` | Protocols: what one is (the morning briefing, whole, as it ships), the six starters with their schedules and what they need, writing your first one in five steps, packs and the registry, what keeps it safe, and three ways to contribute. Links into `docs/protocols/`. |
| `pricing.html` | The pricing page: Free and Cloud in two lines, Free, Cloud and Supporter (Free the longest column), the side-by-side table, the licences in plain words (source, downloads, the MIT build, contributions and packs), what we would and couldn't see with a command for each line (Tables 2 and 3, copied from `docs/cloud-trust.md`), leaving and the shutdown pledge in brief (in full on the Cloud page; `docs/cloud-design.md` §17 asks the pricing page to carry both), and links to the legal drafts. |
| `legal.html` | Drafts for legal review of the Cloud terms of service, acceptable use policy, privacy policy and data processing agreement. Every document carries a DRAFT banner; placeholders are in square brackets. Marked `noindex`. |
| `404.html` | What GitHub Pages serves for an address the site doesn't have, at any depth, so its links and assets start with `/`: preview it through the local server, not from disk. Marked `noindex`. |
| `CNAME`, `robots.txt`, `sitemap.xml` | The domain (`mirrin.app`); crawl everything and find the sitemap; the four pages that may be indexed (not `legal.html` or `404.html`, which are `noindex`). |
| `favicon.svg`, `apple-touch-icon.png` | The mark, a gradient M on a dark tile (the masthead's `#i-mark` is the same drawing), and its 180×180 version for iPhone Home Screens. |
| `site.css` | Everything visual for every page: tokens for dark (the default) and light, layout, the drawn presence screen, the mock-ups, diagrams. |
| `assets/fonts/` | Newsreader and Colophon Sans (variable, weights 400–600) and Colophon Mono (400 and 500), as WOFF2 cut to English text plus Latin-1, with their SIL Open Font License files. Colophon Sans and Mono are IBM Plex Sans and Mono, cut down and renamed (see "Fonts" below). Served from the site itself. There is no italic. |
| `assets/characters/` | The personas' portraits (`mirrin.svg`, `nyra.svg`, `penguin.svg`, about 23 KB), drawn by the presence screen's own `internal/api/characters.js` through `draw.js`, which also puts Mirrin's drawing into the home page's Fig. 1. Run `node site/assets/characters/draw.js` from the repository root after `characters.js` changes; the test fails until you do. `draw.js` is not linked from the site. |
| `assets/demo.mp4` | The 16-second recording (285 KB). |
| `assets/demo-poster.jpg` | Frame at 13 s of that recording (23 KB), used as the poster. |
| `assets/og.png` | The social card (1200×630): the line, the mark, and the default persona as `assets/characters/mirrin.svg` draws him. |
| `assets/og-card.html`, `assets/icon-card.html` | The sources for `og.png` and `apple-touch-icon.png`. Not linked and not published. |
| `site_test.go` | Checks that keep the pages true of the code: `go test ./site` (see "How the truth is kept"). Not published. |

What is published comes to about 950 KB; a first visit to the home page loads about 290 KB, 72 KB of it fonts (the video loads only when asked, and the persona portraits, 23 KB, as they scroll into view). The page never asks another server for anything: fonts, video and images are all local, and there is no analytics, no cookie and no third-party script. The only thing kept in the browser is the reader's theme choice (`localStorage`).

## Preview locally

```sh
cd site
python3 -m http.server 8000
# open http://localhost:8000/
```

Opening `index.html` straight from disk also works, except for `404.html`, whose paths start at the site's root. A local server is closer to GitHub Pages. Pick another port if 8000 is taken.

## Deploy with GitHub Pages

`.github/workflows/pages.yml` publishes the site from `main` whenever something under `site/` (or the workflow) changes, and on demand (Actions → pages → Run workflow). It runs `go test ./site` first, so a page that has gone stale isn't published. It then copies `site/` to a staging folder, removes the working files (`README.md`, `site_test.go`, `assets/og-card.html`, `assets/icon-card.html` and `assets/characters/draw.js`; `TestReadyToPublish` checks the list), and uploads that. There is nothing to build. Its actions are pinned to commits, like every workflow here (`TestWorkflowActionsPinned` in `scripts/`).

Once, in the public repository `MavrkAI/Mirrin`:

1. **Verify the domain for the organisation** (before anything points at it, so no other account can claim it): Organisation settings → Pages → Add a domain → `mirrin.app`, then add the TXT record GitHub shows (`_github-pages-challenge-MavrkAI.mirrin.app`) at the DNS host and press Verify.
2. **DNS** for `mirrin.app`:
   - apex `A` records: `185.199.108.153`, `185.199.109.153`, `185.199.110.153`, `185.199.111.153`;
   - apex `AAAA` records: `2606:50c0:8000::153`, `2606:50c0:8001::153`, `2606:50c0:8002::153`, `2606:50c0:8003::153`;
   - `www` `CNAME` `mavrkai.github.io` (GitHub then sends www.mirrin.app to mirrin.app).

   Remove whatever the registrar put there first: on 2026-10-04 the apex answered with two AWS addresses, not GitHub's.
3. **Settings → Pages**: Build and deployment → Source **GitHub Actions**; Custom domain `mirrin.app` (an Actions deployment takes it from here; `site/CNAME` only records it); wait for the DNS check and the certificate, then tick **Enforce HTTPS**. `.app` is on the browsers' HTTPS-only list, so the site doesn't load at all over plain HTTP: it shows nothing until the certificate is issued.
4. Run the workflow, then check `https://mirrin.app/`, `https://www.mirrin.app/` (redirects), `https://mirrin.app/nowhere` (the 404 page, styled) and a social card preview of the home page.

## The product name token

The name is Mirrin. It sits in as few places as possible, so one command swaps it if it ever changes again.

- In the pages it is always `<span class="pn" translate="no">Mirrin</span>`, plus the `<title>`, the Open Graph and Twitter meta tags, the home page's JSON-LD, and the `aria-label="Mirrin home"` on the masthead logo. Each page's header comment lists these places and says how many tokens it has ("prints N today"); the test holds it to that number.
- `site.css` never contains the name.
- Every GitHub URL contains `/Mirrin`, so this swaps the name and leaves the URLs alone:

  ```sh
  cd site
  sed -E -i '' 's#(^|[^/])Mirrin#\1NewName#g' index.html cloud.html protocols.html pricing.html legal.html 404.html assets/og-card.html
  grep -n '[^/]Mirrin' index.html cloud.html protocols.html pricing.html legal.html 404.html   # prints nothing when done
  ```

  (On Linux, drop the `''` after `-i`.) "Mirrin Cloud" becomes "NewName Cloud" automatically.
- Outside the token, each needing its own decision: the domain (`mirrin.app` in `CNAME`, `robots.txt`, `sitemap.xml` and every page's canonical, `og:url` and image URLs), the GitHub URLs (`github.com/MavrkAI/Mirrin`, the `raw.githubusercontent.com` install line), the CLI name `mirrin` and the folder `~/.mirrin` in commands, and the installers' file names (`Mirrin-<version>-windows-setup.exe`, `Mirrin-<version>-macos.dmg`). Nyra and Pickoo are persona names and MavrkAI is the company; none of them is the product name. The default persona carries the product's name, so in the pages his name ("Hey Mirrin" included) is the token and the swap renames him too; alt and aria-label text calls him the default persona or your twin. In `assets/og-card.html` he is the first name in the legend.
- After a rename, regenerate the social card (below).

### Social card and icons

`assets/og.png` is a screenshot of `assets/og-card.html` at 1200×630 with reduced motion; `apple-touch-icon.png` is one of `assets/icon-card.html` at 180×180. From `site/`:

```sh
shot() { P=$(mktemp -d); "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new \
  --use-mock-keychain --user-data-dir="$P" --no-first-run --hide-scrollbars \
  --force-prefers-reduced-motion --window-size="$2" --screenshot="$1" "file://$PWD/$3"; rm -rf "$P"; }
shot assets/og.png 1200,630 assets/og-card.html
shot apple-touch-icon.png 180,180 assets/icon-card.html
```

Headless Chrome sometimes writes the file and then doesn't exit; once the PNG is there, stop it. Run it from your own login (not a sandbox with a made-up `HOME`), or macOS asks for the keychain.

The mark is drawn four times, the same path each time: `favicon.svg`, the `#i-mark` symbol in every page, the brand in `og-card.html`, and `icon-card.html` (full-bleed, since iOS rounds the corners). Change them together. The pages give `og:image` and `twitter:image` as absolute `https://mirrin.app/assets/og.png`, which social sites need; the card's alt text is in each page's `og:image:alt` and `twitter:image:alt`.

## How the truth is kept

The site is written for customers, in plain words, and its rule is that every claim is true of the code or of the accepted design (`docs/cloud-design.md`). The pages don't show their working: there are no evidence notes, and what a reader sees names no source files, functions, packages, test names, design-doc sections (§) or work packages (`TestCopyCitesNoCode` checks). Instead `site_test.go` holds the citations: each fact a page states is tied there to the code that makes it true, so a page that drifts from the code fails the build. Each page carries one quiet line in its footer, "Check it yourself: Mirrin is open source.", linking the repository (the same test checks it is there once).

- **Status chips** on every section, from one vocabulary in customers' words. No Mirrin release is out, and the site goes up with the first one (launch precondition 2), so nothing is "coming in the next release":
  - green "Available now": built, and in the first release ("Today" on the mock-ups, "Free, for good" on the Free plan);
  - amber "Being tested": built and in the first release, but not yet checked on real phones. Today that is the Home Screen web app, lock-screen approvals and passkeys, until `docs/pwa-testing.md`'s manual matrix is run;
  - blue "Planned" (Cloud: "Planned · not on sale").

  A section that mixes them shows a chip for each. The mock-ups carry the same chips. The legal drafts are "Drafts · not in force". `TestStatusChips` holds the home, Cloud, Protocols and pricing pages to this list.
- **Where a fact comes from** is in the test that checks it, and in the pages' header comments, never in the copy. Comments may name files and design sections; the reader never sees them.
- **Free is the whole twin; Cloud is planned availability.** Every page that prices anything opens with the same two lines (the offer strip): Free is the whole twin, open source, no account, for good; Cloud would add reach and encrypted backup storage. The legal drafts' facts (limits, lapse, the handle) follow Cloud's code, as the Cloud and pricing pages do: 20 Mbit/s per tunnel, shared by an address's connections; a pass up to seven days past the paid-through date; 90 days after it to fetch backups; a handle never reassigned.
- **Cloud is written in the future tense** throughout, is labelled "planned, not on sale" everywhere it appears (including the nav at every width and the phone table labels), and has no Buy button, waiting list or countdown. Its only call to action is "Follow along: watch releases on GitHub", which links the releases page, beside the releases' Atom feed: neither needs an account.
- **One copy of each repeated text, and no repeated sections.** The Free list (index, pricing), the offer strip (index, cloud, pricing) and the FAQ answers the home and Cloud pages share are copies of one text, between `<!-- name:start -->` and `<!-- name:end -->` markers where they repeat. Change them together; `TestOneCopyOfEachList` and `TestSharedFAQAnswersMatch` fail when they drift. Whole sections aren't repeated: the tier cards are on the pricing page only (the Cloud page gives the prices in brief and links there), leaving and the pledge are in full on the Cloud page only, and each page has its own h1 (the test checks the tiers and the h1s).
- **Known issues are disclosed where the claim is made**, not hidden, and listed in the home page's header comment until the code changes. The loopback cookie issue (then section 7, pointed to from section 3) was closed by WP-01's Host, Origin and Sec-Fetch-Site checks. One narrower issue is open, in section 8: a web server on this computer that the browser visits receives the pages' cookie (browsers don't keep cookies apart by port), which a program behind it could replay (`docs/threat-model.md`, "Local surface").
- Drawings are captioned as drawings. The real recording (`demo.mp4`) is captioned as real, says it was made before the characters replaced the orb, and that its orb stays in its resting colour. The characters are drawn by the screen's own code (`assets/characters/`).
- **Voice: no trained wake-word model.** The public build ships none (the old model's training data can't be used in a product; a licence-clean one is WP-24, `docs/cloud-design.md` §12). Every persona, Mirrin included, hears its name by transcribing speech on the computer and answers a beat slower. The pages never claim an on-device detector, or that nothing is transcribed until it hears its name; `TestNoTrainedWakeWordClaimed` checks.
- **Nyra.** The persona the code still calls by her old id is Nyra on the site, everywhere, comments and drawings included; `TestNyraHasHerName` checks.
- **The licence is stated whole:** the source is MIT; the default downloads include WhatsApp (libsignal, GPL-3.0) and are GPL-3.0; an MIT build of each program comes without WhatsApp (it contains MPL-2.0 yamux). What people contribute to the repository is MIT (`CONTRIBUTING.md`); a pack in its own repository keeps the licence its author gives it, so the site never calls packs MIT. pricing.html section 2 says it in plain words, and links `docs/protocols/packs.md` and `contributing.md`.

- **The Protocols page shows real files.** It is for people writing routines, so it shows `mirrin protocols` commands and YAML, but like every page it cites no source files. Its example is `examples/protocols/morning-briefing.yaml` word for word, its gallery has one card per starter with that file's description, schedule and `requires`, and its rain check is the quickstart's (`TestProtocolsPageMatchesTheFiles`). Every `mirrin` command on it is one `cmd/mirrin` has, with `mirrin protocols <sub>` a case in `protocolsCmd` (`TestProtocolsPageCommandsExist`). The signed registry index is "Planned" until `registry/minisign.pub` holds a key; per-pack limits are planned too.
- **Links into the repository resolve.** Every `github.com/MavrkAI/Mirrin/blob/main/...` link on any page, the `docs/protocols/` ones and the footer's "Build on it" included, names a file in this repository, and a heading in it when it has a `#` (`TestRepositoryLinksExist`). The "Build on it" footer group is one text on every page (`build-on-it` markers).
- **One source for what Cloud could see.** `docs/cloud-trust.md` holds the can-see and can't-see tables between its `visibility` markers; `pricing.html` copies them cell for cell (Tables 2 and 3), except that a cell naming a source file (“The relay's source: `internal/…`”) links the file instead of naming it. Change the Markdown first, then the page; the test fails until they match, and it also fails if a "can't" row has no command to check it.
- **The legal pages are drafts.** Each document on `legal.html` says so in a banner, and the facts it states (what is stored, for how long, who processes it) follow `docs/cloud-trust.md` and `cloud/internal/store/schema.sql`. Final versions must be published before checkout opens.

`go test ./site` (part of `go test ./...`) keeps most of this honest on every change: the copy cites no code and has its open-source line, the numbers the pages state (thirteen channels, pictures on seven, 100 and 500, 07:00 and Sunday 08:00, seven daily copies, the states' colours on the drawn screen and in `site.css`) match the code, the characters and personas are the ones `characters.js` and `internal/persona` ship, the Mac app and the Windows Startup entry are described as the code makes them, claims the code has since made false stay gone, the known issue is removed once the loopback checks land (any `Sec-Fetch-Site` or `CrossOriginProtection` check under `internal/`), Windows readers are told `service install` makes a per-user Startup entry for the tray (no administrator), the product name stays in its token, the pricing page's visibility tables match `docs/cloud-trust.md`, the repeated Free list, offer strip and FAQ answers are the same everywhere and the tier cards appear once, the spending caps quoted for the yen are `capScale`'s, the twin is never said to fetch the community list while `registry/minisign.pub` holds no key, the voice copy claims nothing only the detector's loop does (stopping when it hears its name, listening for the answer to a question it asked unprompted), no two components share a class name in `site.css`, Cloud's limits and lapse match its code, "watch releases" goes to the releases page, every `mirrin` command, subcommand and flag in the new docs (`docs/cloud.md`, `cloud-trust.md`, `reach.md`, `backup.md`) and on the Cloud, pricing and legal pages is one `cmd/mirrin` has, every setting they name is a real config key, the Protocols page's example, starters, rain check and commands match the files and the CLI, every link into the repository names a file and heading that exist, the fonts cover every character the pages use, and the site is ready to publish (`TestReadyToPublish`: the domain, each page's canonical and absolute card URLs, descriptions of 160 characters or fewer, the sitemap, the icons, JSON-LD with no ratings, no font called Plex, and a workflow that leaves the working files out). A failure says which page to change and why.

The test reads code other people change (channels, the API, the installers, the service), so it can fail after an unrelated commit. That is its job: the page has gone stale. After merging work from several branches, run `MIRRIN_HOME=$(mktemp -d) go test ./site` once more on the result.

Before each launch, also re-check by hand the claims that depend on code that moves:

```sh
grep -n 'DefaultRegistryURL\|func BuiltinRegistry' internal/protocols/packs.go   # MavrkAI address 1, and its built-in copy
grep -n '"repo"' registry/index.json                              # MavrkAI address 2+: packs it lists (starter)
grep -n 'Sec-Fetch-Site' internal/api/auth.go                     # section 7: another page on this computer can't approve
grep -rl 'func (.*) SendImage' internal/channels | xargs -n1 dirname | sort -u | wc -l   # 7: channels that send the approval picture (whatsapp has two files)
grep -n 'about to submit' internal/daemon/channels.go           # the picture's caption
grep -n 'Spending{' internal/config/config.go                   # 100 and 500, currency from locale.go
grep -n 'var capScale' internal/config/locale.go                 # ¥10,000 and ¥50,000: the caps scaled for small units (Approvals)
grep -n 'DefaultMonthlyBudget = ' internal/config/usage.go       # US$25 model budget, warned at 80% (home FAQ)
grep -n 'rePayment = ' internal/skills/browser/browser.go       # payment labels
grep -n '"0 8 \* \* 0"' internal/daemon/daemon.go               # Sunday 08:00 portrait
grep -n 'schedule: "0 7' internal/protocols/protocols.go        # 07:00 morning briefing
grep -n 'BackupIfDue(ctx, 24' internal/heartbeat/heartbeat.go   # daily memory copies, seven kept
grep -n 'listening{\|thinking{\|speaking{' internal/api/ui.html # state colours (Fig. 1, Fig. 11)
grep -n 'function maverickSVG\|const kindFor' internal/api/characters.js   # the drawings; rerun draw.js if they changed
grep -n '^checksum_ok()\|^verify_signature()' install.sh        # verified downloads (Install; Cloud page, Verify)
grep -n 'ComputeHash' install.ps1                               # the same check on Windows
grep -n 'func (in installer) keepSecrets\|func waitUp' internal/service/service.go   # service install (Install)
grep -n 'func startupScript' internal/service/startup.go         # Windows: service install is a per-user Startup entry for the tray (Install)
grep -n 'mirrin service install' install.ps1                    # Windows: the installer suggests it (Install)
grep -n 'PrivilegesRequired=lowest' packaging/windows/mirrin.iss   # Windows: the setup program needs no administrator (Install)
grep -n 'Parameters: "tray"' packaging/windows/mirrin.iss       # Windows: the setup program starts the tray (Install)
grep -n 'PORTABLE :=' Makefile; grep -n 'mirrin-darwin' .github/workflows/release.yml   # the builds listed in Install
grep -n 'macos.dmg' .github/workflows/release.yml                # the Mac app (Install)
grep -n 'go openWelcome' internal/tray/tray.go                   # its Welcome window (Install)
grep -n 'func protocolsCmd' cmd/mirrin/main.go                  # the Protocols page's commands (Write your first one, Packs)
grep -n 'Until then' registry/minisign.pub                      # while this finds a line, the signed index stays "Planned" (Protocols, section 4) and the twin never fetches the list: home section 7's caption, the shared FAQ (home and Cloud), legal.html Privacy §3 and Protocols section 4 say so
grep -n 'neverAlways' internal/daemon/alwaysallow.go            # what "yes, always" can't cover (Protocols, section 5)
grep -n 'Unverified' docs/pwa-testing.md                        # while this finds rows, the lock screen stays "Being tested"
ls internal | grep -E 'devices|push|stepup|backup|reach|relay|tlsmgr'   # backup and devices today; the rest arrive later
```

Every page says "Available now" (in the first release) for HTTPS reach, the relay and encrypted backup, and "Being tested" for the Home Screen web app, lock-screen approvals and passkeys. When `docs/pwa-testing.md`'s manual matrix has been run and passed, move those three to "Available now" on every page (`TestStatusChips` says when). Re-read `docs/cloud-design.md` §16 for the dates. Cloud stays "planned" until checkout opens.

## Launch preconditions

Do not publish until every one of these is true. Status as checked on 2026-10-06:

1. **The public repository `MavrkAI/Mirrin` exists.** Not yet. It is a new repository, made by `scripts/publish` (`docs/maintainers-release.md`, "Publishing the public repository"): one commit's tree, minus what `scripts/publish/exclude.txt` lists (the old wake model among it), checked for private names and secrets, pushed as a single commit to `main`. Publish from a commit that has everything below merged. Then `gh repo view MavrkAI/Mirrin --json name,visibility` should say `Mirrin` and `PUBLIC`. Until it exists, "Code on GitHub", "Watch releases on GitHub", Releases, the docs links, `SECURITY.md`, `LICENSE` and the raw `install.sh` and `install.ps1` all 404 for readers.
2. **The first Mirrin release exists**, tagged in `MavrkAI/Mirrin` (the Sigstore signature names the repository it was built in, and the installers check for `MavrkAI/Mirrin`), with what `install.sh` and `install.ps1` ask for: `mirrin-darwin-arm64`, `mirrin-darwin-amd64`, `mirrin-linux-amd64`, `mirrin-linux-arm64`, `mirrin-windows-amd64.exe`, `mirrin-windows-arm64.exe`, plus `Mirrin-<version>-macos.dmg`, `Mirrin-<version>-windows-setup.exe`, `SHA256SUMS` and `SHA256SUMS.sigstore.json`, and the rest `scripts/releasecheck` requires (the `-nowhatsapp` MIT builds, the voice bundles, the relay, source, SBOM and `THIRD_PARTY_NOTICES`). Not yet: there is no release in the new repository. `.github/workflows/release.yml` builds and checks all of it.
3. **The registry and the starter pack resolve.** Not yet. `registry/index.json` already names `MavrkAI/mirrin-pack-starter`, and `DefaultRegistryURL` reads `registry/index.json` from `MavrkAI/Mirrin`'s `main`; but `mirrin-pack-starter` doesn't exist yet (the pack is still private under its earlier name: rename it and make it public), and the index resolves only once the repository is public. Any change to `index.json` goes through a pull request to `main` and the `registry-sign` workflow; never edit it without that signature. The signed index stays "Planned" on the Protocols page while `registry/minisign.pub` holds no key.
4. **The docs are in the published commit.** Done: `docs/protocols/`, `docs/cloud-design.md`, `docs/cloud-trust.md` and the rest the pages link to are merged into this branch, and `TestRepositoryLinksExist` passes, so they reach the published commit as long as it has this branch's tree.
5. **`docs/threat-model.md` is corrected.** Done (its "Local surface" section).
6. **The site is on https://mirrin.app.** The pages are ready: `CNAME`, a canonical link, `og:url` and absolute `og:image` and `twitter:image` on every page, `robots.txt`, `sitemap.xml`, the icons, the 404 page and the Pages workflow are in. Not yet: the domain verification, DNS and Pages settings in "Deploy with GitHub Pages".
7. **Nyra.** Done: the code ships her as `nyra` (`internal/persona/nyra.yaml`) and nothing says her old name; `scripts/publish` refuses a copy that does.
8. **No trained wake-word model ships.** Done: the model is embedded only with `-tags wakemodel`, which no release build uses, and `scripts/publish` leaves it (and every `.onnx`) out of the public tree. The site says every persona hears its name by transcribing speech on the computer; `TestNoTrainedWakeWordClaimed` fails if the old Voice text comes back. A licence-clean model is WP-24; when it lands, change the Voice section and that test together.
9. **The phone parts are checked, or stay amber.** `docs/pwa-testing.md` still has 6 "Unverified" rows, so the Home Screen web app, lock-screen approvals and passkeys stay "Being tested" on every page. Run the matrix on real phones to move them to "Available now" (`TestStatusChips` says when).
10. **The notarization line still holds.** The Install section says the Mac builds aren't notarized by Apple yet. The release workflow notarizes only when the Apple signing secrets are set; if the launch release is notarized, drop that sentence.
11. **Every external link has been clicked once**, once the repository is public:

    ```sh
    cd site
    grep -ho 'https://[^"]*' index.html cloud.html protocols.html pricing.html legal.html 404.html | sort -u | while read u; do
      printf '%s %s\n' "$(curl -s -o /dev/null -w '%{http_code}' -L "$u")" "$u"; done
    ```

12. **The name is cleared.** The name search kept with the launch drafts (in `docs/launch/`, which `scripts/publish` leaves out) records no trade-mark, handle or domain search for Mirrin yet; record one before launch.
13. **Before Cloud opens, not before launch: MavrkAI has registered `mirrin.link`.** It is the tenant zone the Cloud code gives twins their addresses in (`internal/cloud/client.go`), a placeholder until then. `mirrin.app`, the operator zone, is the site's domain too: nothing the free twin fetches may ever be served from it or a name under it, because the egress guard counts it as Cloud.
14. **Before checkout, not before launch: the legal pages are final.** `legal.html` holds drafts. The terms, acceptable use policy and privacy policy (and the DPA) must be reviewed by a lawyer, the placeholders filled, and the DRAFT banners removed at least as early as the CHANGELOG notice, 14 days before checkout opens.

Nice to have: a new recording of the character screen, in which the state colour visibly goes green and blue. The current one predates the characters and stays idle, which is why the site leads with the drawn presence screen and keeps the video in the Voice section.

## Technical notes

- **Themes.** Dark (the Mirror palette's blue-black and periwinkle) is the default for everyone, whatever the system says and with or without JavaScript; the "Dark theme" button switches to light and remembers the choice in this browser. The dark tokens are on `:root` and the light ones under `:root[data-theme="light"]`, one block each; the `theme-color` meta follows the choice.
- **Motion.** Calm and in character, and all of it in the "Motion" part of `site.css` (under `prefers-reduced-motion: no-preference`) plus a short block at the end of the home page's script. Only `transform`, `opacity` and the registered custom properties `--c1`, `--lx` and `--ly` move; there is no `filter`, blur or mask (Safari draws blurred glows as squares), so glows are radial gradients or a faded shadow.
  - *The presence screen* cycles listening, thinking and speaking over 16 seconds: Mirrin's lapel pin turns green, then blue (`--c1`), a soft glow behind him follows it, he breathes and sways, thinks in bubbles and talks while he speaks. It has a Pause button and rests while off screen.
  - *The first screen arrives in order*: the line word by word, the buttons, then the screen and the cast. This is CSS alone (`animation-fill-mode: backwards`), so it needs no script and never leaves anything hidden.
  - *The characters are alive*: with the script, every character blinks at random moments (now and then twice), Mirrin nods hello and turns a little towards the pointer as the app's screen does (`--lx`, `--ly`; fine pointers only), and the portraits are swapped for their drawings (fetched from `assets/characters/`, ids suffixed, the files' own `<style>` repeated in `site.css` as `.cv`) so they can react on hover or keyboard focus: Mirrin nods with a raised brow and a smile, Nyra tilts her head and her hair clip twinkles, Pickoo hops, waves and blushes. Their coloured part turns the listening green while you're on the card, and a bubble shows the greeting each persona opens with (`greeting:` in `internal/persona/*.yaml`; the screen reader reads it as text in the personas section).
  - *Below the fold*, sections and cards rise in once as they scroll into view (`.rv`, IntersectionObserver); only what is still below the fold when the script runs is ever hidden. Buttons lift with an accent glow, nav links slide an underline in, the theme button's half-lit disc turns over, and a copy button shows a tick.
  - *Reduced motion* turns all of it off: no script motion runs, the characters stay the still portraits, everything is visible at once, and the presence screen shows the speaking state and the whole exchange, with no Pause button. Nothing plays sound; the video only loads when "Play with sound" is pressed.
- **JavaScript** is progressive enhancement: theme, copy buttons, the "seven parts" disclosure, pausing the drawing, loading the video, and the motion above. Without it, everything is visible and readable, and the portraits are still images.
- **Accessibility.** One `h1` per page, landmarks, a skip link, visible focus, `lang="en-GB"`, text alternatives for every drawing, and text colours at WCAG AA or better in both themes (the lowest is 4.5:1, amber on the darkest paper in light mode).
- **Layout.** Checked at 360, 390, 640, 768, 790, 800, 1024, 1180 and 1440 pixels wide, in both themes, with no sideways scrolling and the masthead nav on one row at every width (brand and theme button above it on phones, one row from 768). The approvals triptych and the diagrams use container queries, so they lay out by the width of their own column rather than the window. Each diagram shows its wide drawing only when the figure is wide enough to keep labels at 11px or more (49rem for the home page's 880-unit drawing, 60rem for the Cloud page's 960-unit ones); otherwise it shows the tall drawing, up to 420px wide, whose small labels are set a size up so they stay at about 11px on a 360px phone.
- **Fonts.** Newsreader and IBM Plex are © their authors under the SIL Open Font License 1.1; the licence texts are in `assets/fonts/`. Newsreader has no Reserved Font Name. IBM Plex has one, "Plex", and the OFL forbids a Modified Version (ours are cut down) from using it, so the files here are renamed: IBM Plex Sans is **Colophon Sans** and IBM Plex Mono is **Colophon Mono**, in every name record but the copyright line, which says it is a Modified Version with Reserved Font Name "Plex". The footer acknowledges where they come from ("cut from IBM Plex"), which the OFL allows. Four files, 72 KB in all (they were 140 KB with an italic): Newsreader is variable, instanced to weights 400–600 with optical size fixed at 18; Colophon Sans is variable, instanced to 400–600; Colophon Mono is two static weights. Each file keeps English text plus Latin-1 (so a name like Zoë or café never falls back to another font) and only the `kern` and `liga` features (IBM Plex Mono has none). The recipe also asks for `mark`, `mkmk` and `ccmp`, but they come out empty for this character list, so the files end up with `kern` and `liga` only. There is no italic face, so the CSS never asks for one: emphasis is set in weight 600, and the pull quote is upright. From the Latin-subset originals:

  ```sh
  python3 -m pip install fonttools brotli
  U="U+0020-007E,U+00A0-00FF,U+0131,U+0152-0153,U+2013-2014,U+2018-201A,U+201C-201E,U+2022,U+2026,U+2032-2033,U+2039-203A,U+20AC,U+2122,U+2212"
  cut() { pyftsubset "$1" --unicodes="$U" --layout-features=kern,liga,mark,mkmk,ccmp --name-IDs='*' --flavor=woff2 --output-file="$2"; }
  fonttools varLib.instancer Newsreader.woff2 wght=400:600 opsz=18 -o nr.ttf && cut nr.ttf newsreader.woff2
  fonttools varLib.instancer IBMPlexSans.woff2 wght=400:600 -o ps.ttf && cut ps.ttf colophon-sans.woff2
  cut IBMPlexMono-Regular.woff2 colophon-mono-400.woff2
  cut IBMPlexMono-Medium.woff2 colophon-mono-500.woff2
  # Then rename the three Plex cuts (the OFL's Reserved Font Name):
  python3 - <<'EOF'
  from fontTools.ttLib import TTFont
  for f in ["colophon-sans.woff2", "colophon-mono-400.woff2", "colophon-mono-500.woff2"]:
      t = TTFont(f)
      for r in t["name"].names:
          s = r.toUnicode()
          if r.nameID == 0:
              s = s.replace("All rights reserved.", 'All rights reserved. With Reserved Font Name "Plex"; this is a Modified Version (cut to a subset and renamed), under the SIL Open Font License 1.1.')
          else:
              s = s.replace("IBM Plex ", "Colophon ").replace("IBMPlex", "Colophon")
          assert r.nameID == 0 or "Plex" not in s, s
          r.string = s
      t.save(f)
  EOF
  ```

  Do this in a throwaway virtual environment (`python3 -m venv "$(mktemp -d)/v"`), not the system Python.

  Running `cut` again on the files here gives the same files, so a new character outside that list means starting from the originals. Keep the `font-weight` ranges in the `@font-face` rules in `site.css` inside 400–600, and check that a page uses nothing outside the list (`→`, `✓` and `✗` come from the reader's system fonts on purpose).
