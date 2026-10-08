package main

// mirrin update installs a newer release when asked, and only then: Mirrin
// never updates itself. It finds the latest release the way install.sh does,
// checks every download against the release's SHA256SUMS (and that list's
// Sigstore signature when cosign is installed) before anything is replaced,
// tries the new program, swaps it in, and restarts the background twin so it
// runs the new version too.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/identity"
	"github.com/MavrkAI/Mirrin/internal/service"
	"github.com/MavrkAI/Mirrin/internal/tray"
)

func init() { commands = append(commands, "update", "uninstall") }

const (
	updateUsage = "usage: mirrin update [--check] [--version vX.Y.Z]"
	defaultRepo = "MavrkAI/Mirrin"
	// maxDownload bounds a download; the largest release file is the disk image.
	maxDownload = 1 << 30
)

type updateOptions struct {
	check   bool
	version string // a specific release; "" for the latest
}

func parseUpdateArgs(args []string) (updateOptions, error) {
	var o updateOptions
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--check" || a == "-check":
			o.check = true
		case a == "--version" || a == "-version":
			if i+1 >= len(args) {
				return o, errors.New(updateUsage)
			}
			i++
			o.version = args[i]
		case strings.HasPrefix(a, "--version="):
			o.version = strings.TrimPrefix(a, "--version=")
		default:
			return o, fmt.Errorf("unknown option %s\n%s", a, updateUsage)
		}
	}
	if o.version != "" {
		if !strings.HasPrefix(o.version, "v") {
			o.version = "v" + o.version
		}
		if !releaseTag.MatchString(o.version) {
			return o, fmt.Errorf("%q isn't a release version; they look like v0.3.0", o.version)
		}
	}
	return o, nil
}

// installLine is the one-line install of the latest release from repo, for
// the shell goos has.
func installLine(goos, repo string) string {
	if goos == "windows" {
		return "irm https://raw.githubusercontent.com/" + repo + "/main/install.ps1 | iex"
	}
	return "curl -fsSL https://raw.githubusercontent.com/" + repo + "/main/install.sh | sh"
}

// updateCmd runs `mirrin update`.
func updateCmd(ctx context.Context, args []string) error {
	opts, err := parseUpdateArgs(args)
	if err != nil {
		return err
	}
	u, err := newUpdater(os.Stdout)
	if err != nil {
		return err
	}
	return u.run(ctx, opts)
}

// twinService is what an update needs from the background service and the
// running twin.
type twinService interface {
	Installed() bool
	Restart(out io.Writer) error
	// RunningVersion is the version the twin running from this home reports,
	// and whether one answered.
	RunningVersion() (string, bool)
}

type updater struct {
	out          io.Writer
	client       *http.Client
	releases     string // https://github.com/<repo>/releases, or a mirror
	repo         string
	current      string // this program's version
	goos, goarch string
	exe          string   // this program, links resolved
	appDirs      []string // where Mirrin.app may be installed (macOS)
	cosign       string   // cosign's path, or "" when it isn't installed
	whatsapp     bool     // this build has the WhatsApp channel
	sudo         bool     // running as root through sudo
	svc          twinService
	command      func(ctx context.Context, name string, args ...string) (string, error)
	tmp          string // a scratch folder for this run
	signedAdHoc  func(ctx context.Context, app string) bool
	attach       func(ctx context.Context, dmg, mountpoint string) error
	detach       func(mountpoint string)
	remove       func(path string) error // os.Remove; fails on Windows for a program still running
	settle       time.Duration           // how long a restarted twin gets to report its new version
}

func newUpdater(out io.Writer) (*updater, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	repo := os.Getenv("MIRRIN_REPO")
	if repo == "" {
		repo = defaultRepo
	}
	releases := strings.TrimRight(os.Getenv("MIRRIN_DOWNLOAD_URL"), "/")
	if releases == "" {
		releases = "https://github.com/" + repo + "/releases"
	}
	cosign, _ := exec.LookPath("cosign")
	u := &updater{
		out: out, client: &http.Client{}, releases: releases, repo: repo, current: version,
		goos: runtime.GOOS, goarch: runtime.GOARCH, exe: exe, cosign: cosign, whatsapp: whatsapp.Built,
		sudo: runtime.GOOS != "windows" && os.Geteuid() == 0 && os.Getenv("SUDO_USER") != "",
		svc:  localTwin{}, command: runCommand, signedAdHoc: signedAdHoc, attach: attachDMG, detach: detachDMG, remove: os.Remove,
		settle: 15 * time.Second,
	}
	if runtime.GOOS == "darwin" {
		u.appDirs = []string{"/Applications"}
		if h, err := os.UserHomeDir(); err == nil {
			u.appDirs = append(u.appDirs, filepath.Join(h, "Applications"))
		}
	}
	return u, nil
}

func (u *updater) say(format string, a ...any) { fmt.Fprintf(u.out, format+"\n", a...) }

func (u *updater) run(ctx context.Context, o updateOptions) error {
	fromSource := !isRelease(u.current)
	if fromSource && o.version == "" {
		u.say("This mirrin was built from source (%s), so it isn't updated from releases.", u.current)
		if tag, err := u.latest(ctx); err == nil {
			u.say("Rebuild it where you built it (git pull && make install), or switch to the latest release, %s:", tag)
			u.say("  mirrin update --version %s", tag)
		} else {
			u.say("Rebuild it where you built it (git pull && make install), or install a release with: %s", installLine(u.goos, u.repo))
		}
		return nil
	}
	tag := o.version
	if tag == "" {
		var err error
		if tag, err = u.latest(ctx); err != nil {
			return err
		}
	}
	notes := u.releases + "/tag/" + tag
	brew := homebrewPath(u.exe)
	cli, app := u.targets()
	if brew {
		app = "" // Homebrew's formula has no app; it updates the CLI
	}
	if !u.whatsapp && app != "" {
		// The published Mirrin.app includes WhatsApp, so a build without it
		// takes only the matching -nowhatsapp program.
		if cli == "" {
			u.say("This Mirrin.app was built without WhatsApp, and the published Mirrin.app includes it.")
			u.say("To stay without WhatsApp, rebuild it from source: git pull && make install TAGS=nowhatsapp")
			return nil
		}
		u.say("%s is left as it is: the published Mirrin.app includes WhatsApp, and this program was built without it. If the background service runs the app, point it at this program with: mirrin service uninstall && mirrin service install", app)
		app = ""
	}
	// An app left behind by an earlier update that couldn't finish is
	// brought up to date on its own.
	appBehind := ""
	if app != "" && cli != "" {
		if v := u.versionOf(ctx, filepath.Join(app, "Contents", "MacOS", "mirrin")); isRelease(v) && compareVersions(tag, v) > 0 {
			appBehind = v
		}
	}
	switch c := compareVersions(tag, u.current); {
	case fromSource:
	case c == 0 && appBehind != "":
		cli = ""
		if o.check {
			u.say("You have Mirrin %s, but Mirrin.app is still %s. Run `mirrin update` to bring it up to date.", u.current, appBehind)
			return nil
		}
		u.say("You have Mirrin %s, but Mirrin.app is still %s; updating it.", u.current, appBehind)
	case c == 0 && o.version != "":
		u.say("You already have %s.", tag)
		return nil
	case c == 0:
		u.say("You have the latest Mirrin (%s).", u.current)
		return nil
	case c < 0 && o.version == "":
		u.say("You have %s, which is newer than the latest release (%s). Nothing to do.", u.current, tag)
		return nil
	case c < 0:
		u.say("Going back to %s (you have %s).", tag, u.current)
		if !o.check {
			u.say("Your twin in %s stays as it is, but an older Mirrin may not understand everything a newer one saved. If you're unsure, run `mirrin identity export` first.", u.home())
		}
	}

	if o.check {
		u.say("Mirrin %s is available (you have %s). What's new: %s", tag, u.current, notes)
		if brew {
			u.say("Homebrew installed this mirrin, so update it with: brew upgrade mirrin")
		} else {
			u.say("Run `mirrin update` to install it.")
		}
		return nil
	}
	if brew {
		u.say("Mirrin %s is available (you have %s). Homebrew installed this mirrin, so update it with:", tag, u.current)
		u.say("  brew upgrade mirrin")
		return nil
	}

	tmp, err := os.MkdirTemp("", "mirrin-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	u.tmp = tmp

	if app != "" && !u.appDirWritable(app) {
		return fmt.Errorf("can't replace %s: this account can't write to %s. Download the disk image from %s and drag Mirrin to Applications, or ask an administrator", app, filepath.Dir(app), notes)
	}
	if cli != "" {
		if err := writable(filepath.Dir(cli)); err != nil {
			hint := "reinstall with: " + installLine(u.goos, u.repo)
			if u.goos != "windows" {
				hint = "run `sudo mirrin update`, or " + hint
			}
			return fmt.Errorf("can't replace %s: this account can't write to %s. To update it, %s", cli, filepath.Dir(cli), hint)
		}
	}

	u.say("Updating Mirrin %s to %s", u.current, tag)
	sums, err := u.sums(ctx, tag)
	if err != nil {
		return err
	}
	// Until something is swapped in, a failure changes nothing, and says so.
	if cli != "" {
		if err := u.updateCLI(ctx, tag, sums, cli); err != nil {
			return nothingChanged(err)
		}
	}
	if app != "" {
		if err := u.updateApp(ctx, tag, sums, app); err != nil {
			if cli == "" {
				return nothingChanged(err)
			}
			// The background service runs the app's copy, so it can't be
			// restarted onto the new version yet.
			return fmt.Errorf("the mirrin command is now %s, but Mirrin.app wasn't updated. Run `mirrin update` again to finish.\nWhat went wrong: %v", tag, err)
		}
	}
	u.say("Mirrin is now %s. What's new: %s", tag, notes)
	u.restartTwin(tag)
	return nil
}

// nothingChanged adds, to an error from before anything was replaced, that
// nothing was.
func nothingChanged(err error) error { return fmt.Errorf("%w\nNothing was changed.", err) }

// home is the twin's folder, for messages.
func (u *updater) home() string { return config.Home() }

// versionOf is the version an installed mirrin reports, or "".
func (u *updater) versionOf(ctx context.Context, bin string) string {
	if name, v := reportedVersion(ctx, u.command, bin, 30*time.Second); ourName(name) {
		return v
	}
	return ""
}

// ourName reports whether name, the first word a program's `version`
// prints, is Mirrin's.
func ourName(name string) bool { return name == brand.Name }

// reportedVersion runs `bin version` and returns the first two words it
// prints: the program's name and version for Mirrin ("mirrin v0.3.0").
// Both are "" when it doesn't start, fails, or prints less.
func reportedVersion(ctx context.Context, command func(ctx context.Context, name string, args ...string) (string, error), bin string, wait time.Duration) (name, v string) {
	if !fileExists(bin) {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	out, err := command(ctx, bin, "version")
	f := strings.Fields(out)
	if err != nil || len(f) < 2 {
		return "", ""
	}
	return f[0], f[1]
}

// releaseTag is a release version: v1.2.3 or v1.2.3-rc.1.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// describeBuild matches what `git describe` adds to a build between releases.
var describeBuild = regexp.MustCompile(`-\d+-g[0-9a-f]{7,}(-dirty)?$|-dirty$`)

// isRelease reports whether v is a published release rather than a build
// from source ("dev", or v0.3.0-4-gabc1234 from git describe).
func isRelease(v string) bool { return releaseTag.MatchString(v) && !describeBuild.MatchString(v) }

// compareVersions orders two release versions as semantic versions: a
// pre-release comes before its release.
func compareVersions(a, b string) int {
	pa, pb := splitVersion(a), splitVersion(b)
	for i := 0; i < 3; i++ {
		if pa.nums[i] != pb.nums[i] {
			return cmpInt(pa.nums[i], pb.nums[i])
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0
	case pa.pre == "":
		return 1
	case pb.pre == "":
		return -1
	}
	x, y := strings.Split(pa.pre, "."), strings.Split(pb.pre, ".")
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] == y[i] {
			continue
		}
		nx, errx := strconv.Atoi(x[i])
		ny, erry := strconv.Atoi(y[i])
		switch {
		case errx == nil && erry == nil:
			return cmpInt(nx, ny)
		case errx == nil:
			return -1
		case erry == nil:
			return 1
		}
		return strings.Compare(x[i], y[i])
	}
	return cmpInt(len(x), len(y))
}

type parsedVersion struct {
	nums [3]int
	pre  string
}

func splitVersion(v string) parsedVersion {
	var p parsedVersion
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	v, p.pre, _ = strings.Cut(v, "-")
	for i, s := range strings.SplitN(v, ".", 3) {
		p.nums[i], _ = strconv.Atoi(s)
	}
	return p
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// latest is the newest release's tag, from where releases/latest redirects:
// no API token, no rate limit.
func (u *updater) latest(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := *u.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for moves := 0; ; moves++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.releases+"/latest", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "mirrin/"+u.current)
		resp, err := c.Do(req)
		if err != nil {
			return "", u.trouble("the latest release", 0)
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		switch {
		case resp.StatusCode == http.StatusNotFound || (resp.StatusCode < 300 && loc == ""):
			return "", fmt.Errorf("couldn't find a published Mirrin release at %s", u.releases)
		case resp.StatusCode < 300 || resp.StatusCode >= 400:
			return "", u.trouble("the latest release", resp.StatusCode)
		}
		// A repository renamed on GitHub first sends its releases/latest
		// to the new name's.
		if to, ok := movedReleases(req.URL, loc); ok && moves < 3 {
			u.moveTo(req.URL, to)
			continue
		}
		// GitHub sends a repository without releases to /releases instead.
		loc = strings.TrimRight(loc, "/")
		tag := loc[strings.LastIndex(loc, "/")+1:]
		if !releaseTag.MatchString(tag) {
			return "", fmt.Errorf("couldn't find a published Mirrin release at %s", u.releases)
		}
		return tag, nil
	}
}

// movedReleases is where a redirect from latest, a releases/latest page,
// leads when that is another releases/latest page on the same server: the
// releases of the repository's new name.
func movedReleases(latest *url.URL, loc string) (*url.URL, bool) {
	to, err := latest.Parse(loc)
	if err != nil || to.Scheme != latest.Scheme || !strings.EqualFold(to.Host, latest.Host) {
		return nil, false
	}
	base, ok := strings.CutSuffix(strings.TrimRight(to.Path, "/"), "/releases/latest")
	if !ok || base == "" || to.Path == latest.Path {
		return nil, false
	}
	return &url.URL{Scheme: to.Scheme, Host: to.Host, Path: base + "/releases"}, true
}

// moveTo takes releases from a renamed repository's new address from now
// on. When the old address named the repository, the new name is used for
// the signature check too: the release workflow signs with the name the
// repository has when it runs.
func (u *updater) moveTo(latest, to *url.URL) {
	if strings.EqualFold(strings.TrimSuffix(latest.Path, "/latest"), "/"+u.repo+"/releases") {
		if repo := strings.Trim(strings.TrimSuffix(to.Path, "/releases"), "/"); strings.Count(repo, "/") == 1 {
			u.repo = repo
		}
	}
	u.releases = to.String()
}

// host is the server the releases come from, for messages.
func (u *updater) host() string {
	h := u.releases
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	h, _, _ = strings.Cut(h, "/")
	return h
}

// trouble explains a download that failed for a reason other than the file
// not existing: status is the HTTP status, or 0 when nothing answered.
func (u *updater) trouble(what string, status int) error {
	if status == 0 {
		return fmt.Errorf("couldn't reach %s for %s. Check your internet connection and try again", u.host(), what)
	}
	return fmt.Errorf("%s answered HTTP %d for %s, so it may be busy or limiting downloads. Try again in a minute", u.host(), status, what)
}

// errMissing is a release file that doesn't exist.
var errMissing = errors.New("not in this release")

// download saves a release file as dst and returns its SHA-256.
func (u *updater) download(ctx context.Context, tag, name, dst string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.releases+"/download/"+tag+"/"+name, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "mirrin/"+u.current)
	resp, err := u.client.Do(req)
	if err != nil {
		return "", u.trouble(name, 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errMissing
	}
	if resp.StatusCode != http.StatusOK {
		return "", u.trouble(name, resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownload+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("%s didn't finish downloading. Try again", name)
		}
		return "", err
	}
	if n > maxDownload {
		os.Remove(dst)
		return "", fmt.Errorf("%s is larger than any Mirrin release file should be, so it wasn't used", name)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sums downloads the release's checksum list, checks its signature when
// cosign is installed, and returns the checksum of each file by name.
func (u *updater) sums(ctx context.Context, tag string) (map[string]string, error) {
	path := filepath.Join(u.tmp, "SHA256SUMS")
	if _, err := u.download(ctx, tag, "SHA256SUMS", path); err != nil {
		if errors.Is(err, errMissing) {
			return nil, fmt.Errorf("Mirrin %s has no checksum list (SHA256SUMS), so its downloads can't be checked. Nothing was changed. Pick a newer release from %s", tag, u.releases)
		}
		return nil, err
	}
	if u.cosign != "" {
		if err := u.verifySignature(ctx, tag, path); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sums := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && len(fields[0]) == 64 {
			sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
		}
	}
	return sums, sc.Err()
}

// verifySignature checks SHA256SUMS against the Sigstore signature the
// release workflow made. Every release since checksums began is signed, so a
// missing signature is refused like a bad one.
func (u *updater) verifySignature(ctx context.Context, tag, sums string) error {
	bundle := sums + ".sigstore.json"
	if _, err := u.download(ctx, tag, "SHA256SUMS.sigstore.json", bundle); err != nil {
		if errors.Is(err, errMissing) {
			return fmt.Errorf("the %s checksum list has no Sigstore signature (SHA256SUMS.sigstore.json), so nothing was changed. Every Mirrin release is signed, so this may mean someone changed the release. Please report it: https://github.com/%s/issues", tag, u.repo)
		}
		return err
	}
	out, err := u.command(ctx, u.cosign, "verify-blob", "--bundle", bundle,
		"--certificate-identity", "https://github.com/"+u.repo+"/.github/workflows/release.yml@refs/tags/"+tag,
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com", sums)
	if err != nil {
		why := lastLine(out)
		if why == "" {
			why = err.Error()
		}
		return fmt.Errorf("the Sigstore signature on the %s checksum list didn't verify with cosign, so nothing was changed.\ncosign said: %s\nIf your cosign is older than 2.4, update it and try again; if it still fails, please report it: https://github.com/%s/issues", tag, why, u.repo)
	}
	u.say("signature verified with cosign (Sigstore)")
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// fetchChecked downloads a release file to dst and checks it against the list.
func (u *updater) fetchChecked(ctx context.Context, tag, name, dst string, sums map[string]string) error {
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("Mirrin %s has no %s. See %s for what's available", tag, name, u.releases+"/tag/"+tag)
	}
	got, err := u.download(ctx, tag, name, dst)
	if errors.Is(err, errMissing) {
		return fmt.Errorf("Mirrin %s lists %s but it can't be downloaded. Try again later", tag, name)
	}
	if err != nil {
		return err
	}
	if got != want {
		os.Remove(dst)
		return fmt.Errorf("%s doesn't match its published checksum, so it may be damaged or tampered with, and it wasn't used. Try again; if it keeps happening, please report it: https://github.com/%s/issues", name, u.repo)
	}
	return nil
}

// targets are the program and the macOS app this update replaces. The app
// is updated with the CLI because the background service runs the app's copy
// when there is one.
func (u *updater) targets() (cli, app string) {
	if a := appBundle(u.exe); a != "" {
		return "", a
	}
	cli = u.exe
	for _, d := range u.appDirs {
		if a := filepath.Join(d, "Mirrin.app"); isDir(a) {
			return cli, a
		}
	}
	return cli, ""
}

func (u *updater) appDirWritable(app string) bool { return writable(filepath.Dir(app)) == nil }

// assetName is the release file for this machine's CLI. A build without
// WhatsApp takes the matching -nowhatsapp program, so an update never adds it.
func (u *updater) assetName() string {
	name := "mirrin-" + u.goos + "-" + u.goarch
	if !u.whatsapp {
		name += "-nowhatsapp"
	}
	if u.goos == "windows" {
		name += ".exe"
	}
	return name
}

// updateCLI stages the new program beside the old one, checks it starts and
// is the version asked for, then swaps it in with a rename, so the program is
// never half-written.
func (u *updater) updateCLI(ctx context.Context, tag string, sums map[string]string, cli string) error {
	name := u.assetName()
	if _, ok := sums[name]; !ok {
		return fmt.Errorf("Mirrin %s has no build for %s/%s (%s). See %s", tag, u.goos, u.goarch, name, u.releases+"/tag/"+tag)
	}
	dir := filepath.Dir(cli)
	staged := filepath.Join(dir, ".mirrin.new")
	if u.goos == "windows" {
		staged = filepath.Join(dir, "mirrin.new.exe")
	}
	if err := u.fetchChecked(ctx, tag, name, staged, sums); err != nil {
		return err
	}
	defer os.Remove(staged) // gone once renamed; removed if anything fails
	u.say("checksum matches")
	if err := os.Chmod(staged, 0o755); err != nil {
		return err
	}
	if err := u.tryVersion(ctx, staged, tag); err != nil {
		return err
	}
	if u.goos == "windows" {
		// A running .exe can be renamed but not replaced or deleted. What
		// earlier updates moved aside goes now, unless it still runs (a twin
		// started by hand); then this one goes beside it, under a new name.
		old := filepath.Join(dir, "mirrin.old.exe")
		earlier, _ := filepath.Glob(filepath.Join(dir, "mirrin.old*.exe"))
		for _, p := range earlier {
			_ = u.remove(p)
		}
		if fileExists(old) {
			old = filepath.Join(dir, fmt.Sprintf("mirrin.old-%d.exe", time.Now().UnixNano()))
		}
		if err := os.Rename(cli, old); err != nil {
			return fmt.Errorf("couldn't move the old mirrin aside: %w", err)
		}
		if err := os.Rename(staged, cli); err != nil {
			_ = os.Rename(old, cli)
			return fmt.Errorf("couldn't put the new mirrin in place: %w", err)
		}
		_ = u.remove(old) // fails while it runs; the next update or uninstall tidies it
	} else if err := os.Rename(staged, cli); err != nil {
		return fmt.Errorf("couldn't put the new mirrin in place: %w", err)
	}
	u.say("installed %s", cli)
	return nil
}

// tryVersion runs `mirrin version` from a new copy, which must start and be
// the release that was asked for.
func (u *updater) tryVersion(ctx context.Context, bin, tag string) error {
	if u.versionOf(ctx, bin) != tag {
		return fmt.Errorf("the downloaded mirrin won't start on this machine (%s/%s), so it wasn't installed. Please report it: https://github.com/%s/issues", u.goos, u.goarch, u.repo)
	}
	return nil
}

// updateApp replaces Mirrin.app from the release's disk image: copied out
// beside the old app, tried, then swapped in with two renames.
func (u *updater) updateApp(ctx context.Context, tag string, sums map[string]string, app string) error {
	dmgName := "Mirrin-" + tag + "-macos.dmg"
	if _, ok := sums[dmgName]; !ok {
		return fmt.Errorf("the %s release has no Mirrin.app", tag)
	}
	dmg := filepath.Join(u.tmp, dmgName)
	u.say("downloading Mirrin.app")
	if err := u.fetchChecked(ctx, tag, dmgName, dmg, sums); err != nil {
		return err
	}
	mnt := filepath.Join(u.tmp, "mnt")
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return err
	}
	if err := u.attach(ctx, dmg, mnt); err != nil {
		return fmt.Errorf("couldn't open the Mirrin disk image: %w", err)
	}
	staged := filepath.Join(filepath.Dir(app), ".Mirrin.app.new")
	_ = os.RemoveAll(staged)
	_, err := u.command(ctx, "ditto", filepath.Join(mnt, "Mirrin.app"), staged)
	u.detach(mnt)
	defer os.RemoveAll(staged)
	if err != nil {
		return fmt.Errorf("couldn't copy the new Mirrin.app: %w", err)
	}
	if err := u.tryVersion(ctx, filepath.Join(staged, "Contents", "MacOS", "mirrin"), tag); err != nil {
		return err
	}
	old := filepath.Join(filepath.Dir(app), ".Mirrin.app.old")
	_ = os.RemoveAll(old)
	if err := os.Rename(app, old); err != nil {
		return fmt.Errorf("couldn't move the old Mirrin.app aside: %w", err)
	}
	if err := os.Rename(staged, app); err != nil {
		_ = os.Rename(old, app)
		return fmt.Errorf("couldn't put the new Mirrin.app in place: %w", err)
	}
	_ = os.RemoveAll(old)
	u.say("installed %s", app)
	if u.signedAdHoc(ctx, app) {
		u.say("This release isn't signed with an Apple Developer ID yet, so macOS may ask again for the microphone and other permissions. Allow them, and the wake word works as before.")
	}
	return nil
}

// restartTwin puts the running twin on the new version: the background
// service is restarted; a twin started by hand is asked to be restarted.
func (u *updater) restartTwin(tag string) {
	if u.sudo {
		u.say("If Mirrin runs in the background, restart it without sudo to use %s: mirrin service restart", tag)
		return
	}
	installed := u.svc.Installed()
	if installed {
		u.say("Restarting the background twin…")
		if err := u.svc.Restart(u.out); err != nil {
			u.say("It didn't restart: %v. Try `mirrin service restart`; `mirrin doctor` says what's wrong.", err)
			return
		}
	}
	v, ok := u.svc.RunningVersion()
	// The old process can answer for a moment while the service swaps them.
	for deadline := time.Now().Add(u.settle); installed && ok && v != tag && time.Now().Before(deadline); {
		time.Sleep(250 * time.Millisecond)
		v, ok = u.svc.RunningVersion()
	}
	switch {
	case !ok:
	case v == tag:
		if installed {
			u.say("The background twin now runs %s.", tag)
		}
	case installed:
		u.say("The background twin still runs %s: its service starts a copy of mirrin from another folder. To run this one: mirrin service uninstall && mirrin service install", v)
	default:
		u.say("Your twin is still running %s. Quit it (from the menu bar, or Ctrl-C in its terminal) and start it again to use %s.", v, tag)
	}
}

// homebrewPath reports whether Homebrew installed a program (Homebrew
// updates it, and removing it by hand would confuse Homebrew).
func homebrewPath(p string) bool {
	p = filepath.ToSlash(p)
	return strings.Contains(p, "/Cellar/") || strings.Contains(p, "/homebrew/") || strings.Contains(p, "/linuxbrew/")
}

// appBundle is the .app a program is inside, or "".
func appBundle(p string) string {
	p = filepath.ToSlash(p)
	i := strings.Index(p, ".app/Contents/MacOS/")
	if i < 0 {
		return ""
	}
	return filepath.FromSlash(p[:i+len(".app")])
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// writable reports whether files can be created in dir.
func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".mirrin-write-test-")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// runCommand runs a program and returns what it printed.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// signedAdHoc reports whether a macOS app has only an ad-hoc signature, which
// macOS ties its permissions to, so they are asked for again after an update.
func signedAdHoc(ctx context.Context, app string) bool {
	out, _ := exec.CommandContext(ctx, "codesign", "-dv", app).CombinedOutput()
	return strings.Contains(string(out), "Signature=adhoc")
}

func attachDMG(ctx context.Context, dmg, mountpoint string) error {
	out, err := exec.CommandContext(ctx, "hdiutil", "attach", dmg, "-nobrowse", "-readonly", "-quiet", "-mountpoint", mountpoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func detachDMG(mountpoint string) { _ = exec.Command("hdiutil", "detach", mountpoint, "-quiet").Run() }

// localTwin is this home's background service and running twin.
type localTwin struct{}

func (localTwin) Installed() bool {
	installed, _ := service.State()
	return installed
}

func (localTwin) Restart(out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Default()
	}
	service.Mode = serviceMode(runtime.GOOS, cfg.Tray, tray.Available())
	listen, dataDir := identity.LocalAPI(config.Home())
	var up func() bool
	if listen != "" {
		up = func() bool { return api.Connect(listen, dataDir) != nil }
	}
	return service.Control(cfg, "restart", up, out)
}

func (localTwin) RunningVersion() (string, bool) {
	listen, dataDir := identity.LocalAPI(config.Home())
	if listen == "" {
		return "", false
	}
	c := api.Connect(listen, dataDir)
	if c == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := c.Status(ctx)
	if err != nil {
		return "", false
	}
	return st.Version, true
}
