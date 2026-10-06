package protocols

import (
	"context"
	"encoding/json"
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
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/registry"
	"gopkg.in/yaml.v3"
)

// Pack metadata lives in pack.yaml at the root of a pack repository.
type Pack struct {
	// Dir is the installed directory name (derived from the repo URL); empty for registry entries.
	Dir         string   `yaml:"-" json:"dir,omitempty"`
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Author      string   `yaml:"author" json:"author,omitempty"`
	Repo        string   `yaml:"repo" json:"repo"`
	Tags        []string `yaml:"tags" json:"tags,omitempty"`
	Version     string   `yaml:"version" json:"version,omitempty"`
	// Commit pins a registry entry to the commit that was reviewed. For an
	// installed pack it is the commit on disk.
	Commit string `yaml:"-" json:"commit,omitempty"`
}

// Source is what to hand AddPack: the repo, pinned to Commit when there is one.
func (p Pack) Source() string {
	if p.Commit != "" {
		return p.Repo + "#" + p.Commit
	}
	return p.Repo
}

// Registry is the community index: a JSON file listing packs.
type Registry struct {
	Packs []Pack `json:"packs"`
}

// DefaultRegistryURL is where `mirrin protocols search` looks.
const DefaultRegistryURL = "https://raw.githubusercontent.com/MavrkAI/Mirrin/main/registry/index.json"

// legacyRegistryURL was the default before the rename. Configs, installed
// packs and identity archives that name it mean the default.
const legacyRegistryURL = "https://raw.githubusercontent.com/MavrkAI/AntBot/main/registry/index.json" // rename:keep

// IsDefaultRegistry reports whether url names the default index: "", its
// address, or its address from before the rename. The default index is the
// signed one, with the copy built into this version to fall back on.
func IsDefaultRegistry(url string) bool {
	return url == "" || url == DefaultRegistryURL || url == legacyRegistryURL
}

// registryURL is DefaultRegistryURL; tests point it elsewhere.
var registryURL = DefaultRegistryURL

// FetchRegistry downloads the index. When the default index can't be reached,
// or its signature (index.json.minisig, checked against the key built in)
// doesn't check out, it answers from the copy built into this version, so
// search works offline and on first run and a tampered index is never used.
// A registry set in config is the user's own and says why it failed instead.
func FetchRegistry(ctx context.Context, url string) (*Registry, error) {
	if IsDefaultRegistry(url) {
		if r, err := fetchSignedRegistry(ctx, registryURL); err == nil {
			return r, nil
		}
		return BuiltinRegistry()
	}
	r, err := fetchRegistry(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the pack registry at %s (%v); check protocol_registry in config.yaml", url, err)
	}
	return r, nil
}

// BuiltinRegistry is the index as it was when this version was built.
func BuiltinRegistry() (*Registry, error) {
	var r Registry
	if err := json.Unmarshal(registry.Index, &r); err != nil {
		return nil, fmt.Errorf("the built-in pack index is damaged: %w", err)
	}
	return &r, nil
}

// registryClient refuses to be redirected off https.
var registryClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return checkRegistryURL(req.URL.String())
	},
}

// fetchSignedRegistry reads the index at u and its signature at u.minisig,
// and returns it only when the signature is good.
func fetchSignedRegistry(ctx context.Context, u string) (*Registry, error) {
	if registryKey == nil {
		return nil, errNoRegistryKey
	}
	data, err := fetchURL(ctx, u, 4<<20)
	if err != nil {
		return nil, err
	}
	sig, err := fetchURL(ctx, u+".minisig", 4<<10)
	if err != nil {
		return nil, err
	}
	if err := verifyMinisign(registryKey, data, sig); err != nil {
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, errors.New("not a pack index")
	}
	return &r, nil
}

func fetchRegistry(ctx context.Context, u string) (*Registry, error) {
	data, err := fetchURL(ctx, u, 4<<20)
	if err != nil {
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, errors.New("not a pack index")
	}
	return &r, nil
}

// fetchURL gets up to limit bytes from a registry address.
func fetchURL(ctx context.Context, u string, limit int64) ([]byte, error) {
	if err := checkRegistryURL(u); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := registryClient.Do(req)
	if err != nil {
		if errors.Is(err, errRegistryNotHTTPS) {
			return nil, errRegistryNotHTTPS
		}
		return nil, errors.New("can't be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, errors.New("can't be reached")
	}
	return data, nil
}

var errRegistryNotHTTPS = errors.New("the pack registry must be an https:// address")

// checkRegistryURL allows https, and plain http only to this machine.
func checkRegistryURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a web address", raw)
	}
	if u.Scheme == "https" || u.Scheme == "http" && isLoopback(u.Hostname()) {
		return nil
	}
	return errRegistryNotHTTPS
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Search returns packs whose name, description or tags match term.
func (r *Registry) Search(term string) []Pack {
	term = strings.ToLower(strings.TrimSpace(term))
	var out []Pack
	for _, p := range r.Packs {
		hay := strings.ToLower(p.Name + " " + p.Description + " " + strings.Join(p.Tags, " "))
		if term == "" || strings.Contains(hay, term) {
			out = append(out, p)
		}
	}
	return out
}

// Find returns the pack with this name.
func (r *Registry) Find(name string) (Pack, bool) {
	for _, p := range r.Packs {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Pack{}, false
}

// packName derives a directory name from a repo URL or path: the last path
// element, lowercased, with anything but letters, digits, '.', '_' and '-'
// replaced, and never "." or "..".
func packName(src string) string {
	s := strings.TrimSuffix(strings.TrimSuffix(src, "/"), ".git")
	if i := strings.LastIndexAny(s, `/\:`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(s))
	if s = strings.TrimLeft(s, ".-"); s == "" {
		s = "pack"
	}
	return s
}

// ProvenanceFile, at the root of an installed pack, records where it came
// from and the commit it is pinned to. It travels with identity exports,
// which leave .git behind.
const ProvenanceFile = ".antbot-pack.json"

// Provenance is what ProvenanceFile holds. Identity exports and imports read
// and write it through this type, so there is one reader.
type Provenance struct {
	Source string `json:"source"`
	// Ref is the tag, branch or commit the user pinned it to with #ref.
	Ref string `json:"ref,omitempty"`
	// Registry is the pack's name in the index it was installed from, and
	// Index that index ("" for the default). Such a pack follows the commit
	// its entry lists, so a bump in the index reaches everyone who has it.
	Registry  string    `json:"registry,omitempty"`
	Index     string    `json:"index,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	Installed time.Time `json:"installed,omitzero"`
	Updated   time.Time `json:"updated,omitzero"`
}

// ReadProvenance reads the provenance file of the pack at root. The file
// travels with identity exports, so callers check what it says before use.
func ReadProvenance(root string) (Provenance, bool) {
	var pv Provenance
	b, err := os.ReadFile(filepath.Join(root, ProvenanceFile))
	return pv, err == nil && json.Unmarshal(b, &pv) == nil
}

func writeProvenance(root string, pv Provenance) error {
	b, err := json.MarshalIndent(pv, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, ProvenanceFile), append(b, '\n'), 0o600); err != nil {
		return err
	}
	// Keep it out of the checkout's git status.
	exclude := filepath.Join(root, ".git", "info", "exclude")
	if st, err := os.Stat(filepath.Join(root, ".git")); err != nil || !st.IsDir() {
		return nil
	}
	if cur, _ := os.ReadFile(exclude); strings.Contains(string(cur), ProvenanceFile) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString("\n/" + ProvenanceFile + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

var (
	// reSCP is git's scp-like ssh form: git@github.com:you/pack.git. Nothing
	// in it may start with '-', which ssh would read as an option.
	reSCP = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*:[A-Za-z0-9._~/][A-Za-z0-9._~/-]*$`)
	// reRef is a tag, branch or commit to pin to.
	reRef    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	reCommit = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// validRef reports whether ref is safe to hand git as a tag, branch or commit
// ("" is the default branch). Nothing git could read as an option gets through.
func validRef(ref string) bool {
	return ref == "" || reRef.MatchString(ref) && !strings.Contains(ref, "..")
}

// checkGitURL accepts the addresses packs may be cloned from: https, ssh
// (ssh://… or git@host:path) and file:// for a repository on this machine.
// Anything else, including git's ext:: transports and plain http, is refused.
func checkGitURL(src string) error {
	if reSCP.MatchString(src) {
		return nil
	}
	u, err := url.Parse(src)
	switch {
	case err == nil && (u.Scheme == "https" || u.Scheme == "ssh") && u.Host != "" &&
		!strings.HasPrefix(u.Host, "-") && !strings.HasPrefix(u.User.Username(), "-"):
		return nil
	case err == nil && u.Scheme == "file" && u.Path != "":
		return nil
	case err == nil && u.Scheme == "http":
		return fmt.Errorf("%s uses plain http, which can be tampered with on the way; use its https:// address", src)
	}
	return fmt.Errorf("%q isn't a folder on this machine or a git address (https://…); to install from the registry by name, use `mirrin protocols install <name>`", src)
}

// AddPack installs a pack from a git URL or a local folder into dir/packs and
// returns its directory name. A git URL may end in #<tag, branch or commit>
// to pin it there. The commit installed is recorded, and the pack only moves
// off it when an update is applied. The pack is fetched into a hidden folder
// and moved into place once complete, so a failed install leaves nothing
// half-loaded.
func AddPack(ctx context.Context, dir, src string) (string, error) {
	return addPack(ctx, dir, src, Provenance{})
}

// AddRegistryPack installs pk, an entry of the index at index ("" for the
// default), at the commit the entry lists. `protocols update` then follows
// that entry rather than the repository's branch.
func AddRegistryPack(ctx context.Context, dir, index string, pk Pack) (string, error) {
	return addPack(ctx, dir, pk.Source(), Provenance{Registry: pk.Name, Index: index})
}

// addPack is AddPack; from carries the registry origin, if any. A registry
// entry is always a git address, never a folder on this machine.
func addPack(ctx context.Context, dir, src string, from Provenance) (string, error) {
	src = strings.TrimSpace(src)
	local := false
	if st, err := os.Stat(src); err == nil && st.IsDir() && from.Registry == "" {
		local = true
	}
	repo, ref := src, ""
	if !local {
		repo, ref, _ = strings.Cut(src, "#")
		if err := checkGitURL(repo); err != nil {
			return "", err
		}
		if !validRef(ref) {
			return "", fmt.Errorf("%q isn't a tag, branch or commit name", ref)
		}
	}
	name := packName(repo)
	dest := filepath.Join(PacksDir(dir), name)
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("pack %q is already installed (use `mirrin protocols update`)", name)
	}
	if err := os.MkdirAll(PacksDir(dir), 0o700); err != nil {
		return "", err
	}
	stage := filepath.Join(PacksDir(dir), "."+name+".installing")
	if err := os.RemoveAll(stage); err != nil {
		return "", err
	}
	pv := Provenance{Source: repo, Ref: ref, Registry: from.Registry, Index: from.Index, Installed: time.Now().UTC()}
	if pv.Registry != "" {
		pv.Ref = "" // the registry entry decides, not a pin of the user's
	}
	fetch := func() error {
		if local {
			abs, err := filepath.Abs(src)
			if err != nil {
				return err
			}
			pv.Source = abs
			return copyDir(src, stage)
		}
		commit, err := cloneAt(ctx, repo, ref, stage)
		pv.Commit = commit
		return err
	}
	if err := fetch(); err != nil {
		os.RemoveAll(stage)
		return "", err
	}
	if err := writeProvenance(stage, pv); err != nil {
		os.RemoveAll(stage)
		return "", err
	}
	if err := os.Rename(stage, dest); err != nil {
		os.RemoveAll(stage)
		return "", err
	}
	return name, nil
}

// cloneAt clones repo into stage, checked out at ref (a tag, branch or
// commit; "" for the default branch), and returns the commit checked out.
// Both are checked by the caller.
func cloneAt(ctx context.Context, repo, ref, stage string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git is required to install packs from a repository")
	}
	if _, err := runGit(ctx, "", "clone", "-q", "--depth", "1", "--", repo, stage); err != nil {
		return "", fmt.Errorf("couldn't download the pack from %s: %w", repo, err)
	}
	if ref != "" {
		if _, err := runGit(ctx, stage, "fetch", "-q", "--depth", "1", "origin", ref); err != nil {
			return "", fmt.Errorf("couldn't get %s from %s: %w", ref, repo, err)
		}
		if _, err := runGit(ctx, stage, "checkout", "-q", "--detach", "FETCH_HEAD"); err != nil {
			return "", err
		}
	}
	return runGit(ctx, stage, "rev-parse", "HEAD")
}

// runGit runs git (in dir, if set) with prompts off, so a private or missing
// repository fails straight away instead of waiting for a password or an ssh
// answer nobody will type. In dir, git never looks above it for a repository,
// so a pack with a damaged .git can't send it to one further up. It returns
// trimmed stdout, or an error in plain words.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if ssh := sshCommand(os.Getenv, gitSSHCommand); ssh != "" {
		env = append(env, "GIT_SSH_COMMAND="+ssh)
	}
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
		if abs, err := filepath.Abs(dir); err == nil {
			env = append(env, "GIT_CEILING_DIRECTORIES="+filepath.Dir(abs))
		}
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", gitError(err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// sshCommand is the ssh git uses for packs: ssh in batch mode, so an unknown
// host key or a key with a passphrase fails instead of prompting. Anyone who
// set their own (GIT_SSH_COMMAND, GIT_SSH or core.sshCommand) keeps it.
func sshCommand(getenv func(string) string, configured func() string) string {
	if getenv("GIT_SSH_COMMAND") != "" || getenv("GIT_SSH") != "" || configured() != "" {
		return ""
	}
	return "ssh -o BatchMode=yes"
}

// gitSSHCommand is core.sshCommand from the user's git config, read once.
var gitSSHCommand = sync.OnceValue(func() string {
	cmd := exec.Command("git", "config", "--get", "core.sshCommand")
	cmd.Dir = string(filepath.Separator) // not inside any repository's own config
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
})

// gitError turns git's stderr into a sentence.
func gitError(err error, stderr string) error {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "host key verification failed"):
		return errors.New("this machine doesn't know that server's ssh host key yet; connect to it once with ssh, or use its https:// address")
	case strings.Contains(low, "permission denied (publickey"):
		return errors.New("the server didn't accept your ssh key; use its https:// address, or add your key with ssh-add")
	case strings.Contains(low, "repository not found"), strings.Contains(low, "could not read username"),
		strings.Contains(low, "terminal prompts disabled"), strings.Contains(low, "authentication failed"),
		strings.Contains(low, "does not appear to be a git repository"):
		return errors.New("the repository doesn't exist or isn't public")
	case strings.Contains(low, "could not resolve host"), strings.Contains(low, "failed to connect"):
		return errors.New("couldn't reach the server; check the connection")
	case strings.Contains(low, "couldn't find remote ref"), strings.Contains(low, "not our ref"):
		return errors.New("that tag, branch or commit isn't there (a commit needs all 40 characters)")
	case strings.Contains(low, "would be overwritten"), strings.Contains(low, "please commit your changes"):
		return errors.New("files in it were edited by hand; copy your changes into your own protocols folder, then remove and re-add the pack")
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := strings.TrimSpace(strings.TrimPrefix(lines[len(lines)-1], "fatal: ")); last != "" {
		return errors.New(last)
	}
	return err
}

// packDir finds an installed pack by directory name or pack.yaml name. It only
// ever returns a directory directly inside dir/packs.
func packDir(dir, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) {
		if st, err := os.Lstat(filepath.Join(PacksDir(dir), name)); err == nil && st.IsDir() {
			return filepath.Join(PacksDir(dir), name), nil
		}
	}
	packs, _ := InstalledPacks(dir)
	for _, p := range packs {
		if strings.EqualFold(p.Name, name) {
			return filepath.Join(PacksDir(dir), p.Dir), nil
		}
	}
	return "", fmt.Errorf("no pack named %q is installed", name)
}

// RemovePack deletes an installed pack by directory name or pack.yaml name.
func RemovePack(dir, name string) error {
	dest, err := packDir(dir, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dest)
}

// PackUpdate is what an update brings to one installed pack.
type PackUpdate struct {
	Dir string
	// From is the commit installed, To the one available; equal when current.
	From, To string
	// Changes has a line per file that differs: "changed protocols/news.yaml".
	Changes []string
	// Note says why the pack was left alone: pinned, or copied from a folder.
	Note string
	// Err is why the check failed.
	Err error
	// Reconnect is set for a pack that is a copy (it came with an identity
	// import, which leaves .git behind) with a repository it came from:
	// applying clones it again at To, so it updates like any other.
	Reconnect bool
	// Source is the repository a Reconnect clones.
	Source string
}

// Pending reports whether applying u would change the pack.
func (u PackUpdate) Pending() bool {
	return u.Err == nil && u.Note == "" && u.To != "" && (u.From != u.To || u.Reconnect)
}

// CheckPackUpdates fetches the latest commit of every pack installed with git
// and lists what differs, without changing what is installed. A pack from the
// registry follows the commit its entry lists; one pinned to a tag or branch
// follows that; one pinned to a commit stays put.
func CheckPackUpdates(ctx context.Context, dir string) ([]PackUpdate, error) {
	entries, err := os.ReadDir(PacksDir(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type fetched struct {
		r   *Registry
		err error
	}
	indexes := map[string]fetched{}
	index := func(u string) (*Registry, error) {
		f, ok := indexes[u]
		if !ok {
			f.r, f.err = FetchRegistry(ctx, u)
			indexes[u] = f
		}
		return f.r, f.err
	}
	var out []PackUpdate
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, checkPack(ctx, filepath.Join(PacksDir(dir), e.Name()), index))
		}
	}
	return out, nil
}

func checkPack(ctx context.Context, root string, index func(string) (*Registry, error)) PackUpdate {
	u := PackUpdate{Dir: filepath.Base(root)}
	if st, err := os.Stat(filepath.Join(root, ".git")); err != nil || !st.IsDir() {
		if pv, ok := ReadProvenance(root); ok && checkGitURL(pv.Source) == nil {
			return checkCopiedPack(ctx, u, pv, index)
		}
		u.Note = "copied from a folder, so there's nothing to fetch; remove it and add it again to refresh"
		return u
	}
	pv, _ := ReadProvenance(root)
	ref := pv.Ref
	switch {
	case pv.Registry != "":
		if ref, u.Note, u.Err = registryRef(pv, index); u.Note != "" || u.Err != nil {
			return u
		}
	case !validRef(ref):
		// The file travels with identity exports, so it is read with suspicion.
		u.Err = fmt.Errorf("its %s names %q, which isn't a tag, branch or commit; remove the pack and add it again", ProvenanceFile, ref)
		return u
	case reCommit.MatchString(ref):
		u.Note = fmt.Sprintf("pinned to commit %s; to move it, remove it and add it again with #<tag or commit>", ShortCommit(ref))
		return u
	}
	if ref == "" {
		ref = "HEAD"
	}
	var err error
	if u.From, err = runGit(ctx, root, "rev-parse", "HEAD"); err != nil {
		u.Err = err
		return u
	}
	if _, err := runGit(ctx, root, "fetch", "-q", "--depth", "1", "origin", ref); err != nil {
		u.Err = err
		return u
	}
	if u.To, err = runGit(ctx, root, "rev-parse", "FETCH_HEAD^{commit}"); err != nil || u.To == u.From {
		u.Err = err
		return u
	}
	diff, err := runGit(ctx, root, "diff", "--name-status", "--no-renames", u.From, u.To)
	if err != nil {
		u.Err = err
		return u
	}
	verbs := map[string]string{"A": "added", "D": "removed"}
	for _, line := range strings.Split(diff, "\n") {
		status, file, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		verb := verbs[status[:1]]
		if verb == "" {
			verb = "changed"
		}
		u.Changes = append(u.Changes, verb+" "+file)
	}
	return u
}

// registryRef is the commit a pack installed from the registry should be at,
// per its entry in the index it came from ("" for the repository's default
// branch when the entry lists no commit). note says why it is left alone.
func registryRef(pv Provenance, index func(string) (*Registry, error)) (ref, note string, err error) {
	reg, err := index(pv.Index)
	if err != nil {
		return "", "", err
	}
	pk, ok := reg.Find(pv.Registry)
	switch {
	case !ok:
		return "", "no longer in the pack registry, so it was left as it is; remove it if you don't want it any more", nil
	case !sameRepo(pk.Repo, pv.Source):
		return "", fmt.Sprintf("the registry now lists it at %s; remove it and install it again to follow", pk.Repo), nil
	case pk.Commit != "" && !reCommit.MatchString(pk.Commit):
		return "", "", fmt.Errorf("the registry lists %q for it, which isn't a full commit", pk.Commit)
	}
	return pk.Commit, "", nil
}

// SameRepo reports whether a and b name the same repository, ignoring case,
// a trailing slash and ".git".
func SameRepo(a, b string) bool { return sameRepo(a, b) }

func sameRepo(a, b string) bool {
	norm := func(s string) string {
		return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(s), "/"), ".git")
	}
	return strings.EqualFold(norm(a), norm(b))
}

// ApplyPackUpdate moves a pack to the commit CheckPackUpdates found.
func ApplyPackUpdate(ctx context.Context, dir string, u PackUpdate) error {
	if !u.Pending() {
		return nil
	}
	if !reCommit.MatchString(u.To) {
		return fmt.Errorf("%s was left as it was: %q isn't a commit", u.Dir, u.To)
	}
	root, err := packDir(dir, u.Dir)
	if err != nil {
		return err
	}
	if u.Reconnect {
		pv, ok := ReadProvenance(root)
		if !ok {
			return fmt.Errorf("%s was left as it was: its %s is gone", u.Dir, ProvenanceFile)
		}
		if err := ReconnectPack(ctx, dir, filepath.Base(root), pv, u.To); err != nil {
			return fmt.Errorf("%s was left as it was: %w", u.Dir, err)
		}
		return nil
	}
	if _, err := runGit(ctx, root, "checkout", "-q", "--detach", u.To); err != nil {
		return fmt.Errorf("%s was left as it was: %w", u.Dir, err)
	}
	pv, ok := ReadProvenance(root)
	if !ok {
		pv.Source, _ = runGit(ctx, root, "remote", "get-url", "origin")
	}
	pv.Commit, pv.Updated = u.To, time.Now().UTC()
	return writeProvenance(root, pv)
}

// UpdatePacks checks every pack and applies what it finds. Show the user
// CheckPackUpdates first when there is someone to ask.
func UpdatePacks(ctx context.Context, dir string) ([]PackUpdate, error) {
	ups, err := CheckPackUpdates(ctx, dir)
	if err != nil {
		return nil, err
	}
	for i, u := range ups {
		if err := ApplyPackUpdate(ctx, dir, u); err != nil {
			ups[i].Err = err
		}
	}
	return ups, nil
}

// ShortCommit is a commit as people read it.
func ShortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// InstalledPacks lists packs with their metadata (pack.yaml if present) and
// the commit each is pinned to.
func InstalledPacks(dir string) ([]Pack, error) {
	entries, err := os.ReadDir(PacksDir(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Pack
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		root := filepath.Join(PacksDir(dir), e.Name())
		p := Pack{Dir: e.Name(), Name: e.Name()}
		if b, err := os.ReadFile(filepath.Join(root, "pack.yaml")); err == nil {
			_ = yaml.Unmarshal(b, &p)
			if p.Name == "" {
				p.Name = e.Name()
			}
			p.Dir = e.Name()
		}
		if pv, ok := ReadProvenance(root); ok {
			p.Commit = pv.Commit
			if p.Repo == "" {
				p.Repo = pv.Source
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// Scaffold writes a template protocol file for a new contribution.
func Scaffold(dir, name string) (string, error) {
	p := Protocol{
		Name:        name,
		Description: "What it does, in one line.",
		Version:     "0.1.0",
		Author:      "your name or handle",
		Tags:        []string{"example"},
		Schedule:    "",
		Requires:    []string{"web"},
		Vars:        map[string]Var{"topic": {Description: "What to look for", Default: "the weather"}},
		Prompt:      "Look up {{topic}} with fetch_url and tell the user the one thing that matters, in two sentences.",
	}
	return Write(dir, p)
}

// copyDir copies a pack folder. It leaves out .git, since a copy is a
// snapshot, and symbolic links, which could point anywhere on this machine.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}
