package protocols

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// RemovePack used to join any name onto packs/ and os.RemoveAll the result.
func TestRemovePackStaysInsidePacks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "protocols")
	victim := filepath.Join(root, "victim")
	write(t, filepath.Join(victim, "keep.txt"), "precious")
	write(t, filepath.Join(dir, "mine.yaml"), "name: mine\nprompt: x\n")
	write(t, filepath.Join(dir, "packs", "good", "pack.yaml"), "name: goodpack\n")
	write(t, filepath.Join(dir, "packs", "other", "pack.yaml"), "name: other\n")

	for _, name := range []string{"../../victim", "../..", "..", ".", "", "/", victim, "good/../../../victim", `..\..\victim`} {
		if err := RemovePack(dir, name); err == nil {
			t.Errorf("RemovePack(%q) should refuse", name)
		}
	}
	if _, err := os.Stat(filepath.Join(victim, "keep.txt")); err != nil {
		t.Fatal("a directory outside packs/ was deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "mine.yaml")); err != nil {
		t.Fatal("the user's protocols were deleted")
	}
	if err := RemovePack(dir, "goodpack"); err != nil {
		t.Fatalf("remove by pack.yaml name: %v", err)
	}
	if err := RemovePack(dir, "other"); err != nil {
		t.Fatalf("remove by directory: %v", err)
	}
	if pks, _ := InstalledPacks(dir); len(pks) != 0 {
		t.Fatalf("packs left: %+v", pks)
	}
}

// git clone used to get the source with no scheme check and no "--".
func TestAddPackRefusesUnsafeSources(t *testing.T) {
	dir := t.TempDir()
	for _, src := range []string{
		"--upload-pack=touch /tmp/mirrin-pwned",
		"-u touch /tmp/mirrin-pwned",
		"ext::sh -c touch% /tmp/mirrin-pwned",
		"http://example.com/pack.git",
		"ftp://example.com/pack.git",
		"starter",
		"https://github.com/you/pack#--upload-pack=x",
		"https://github.com/you/pack#../../etc",
		"ssh://-oProxyCommand=touch%20/tmp/mirrin-pwned/x",
		"ssh://-o@host/x",
		"-oProxyCommand=x@host:path",
		"git@host:-x",
	} {
		if _, err := AddPack(context.Background(), dir, src); err == nil {
			t.Errorf("AddPack(%q) should refuse", src)
		}
	}
	if entries, _ := os.ReadDir(PacksDir(dir)); len(entries) != 0 {
		t.Fatalf("something was installed: %v", entries)
	}
	if _, err := os.Stat("/tmp/mirrin-pwned"); err == nil {
		t.Fatal("a git option was injected")
	}
}

func TestPackName(t *testing.T) {
	for src, want := range map[string]string{
		"https://github.com/You/Mirrin-Pack-News.git": "mirrin-pack-news",
		"git@github.com:you/news.git":                 "news",
		"https://example.com/..":                      "pack",
		"/some/dir/My Pack/":                          "my-pack",
		`C:\packs\news`:                               "news",
	} {
		if got := packName(src); got != want {
			t.Errorf("packName(%q) = %q, want %q", src, got, want)
		}
	}
}

// The default registry and starter pack URLs returned 404, so search failed
// for everyone. The default index now falls back to the built-in copy.
func TestFetchRegistryFallsBackToBuiltin(t *testing.T) {
	status := http.StatusNotFound
	body := `{"packs":[{"name":"live","description":"d","repo":"https://github.com/x/live"}]}`
	signer := newTestSigner(t)
	useRegistryKey(t, signer)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if strings.HasSuffix(r.URL.Path, ".minisig") {
			_, _ = w.Write([]byte(signer.sign([]byte(body), false)))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	old := registryURL
	registryURL = srv.URL + "/index.json"
	defer func() { registryURL = old }()
	ctx := context.Background()

	r, err := FetchRegistry(ctx, "")
	if err != nil {
		t.Fatalf("default registry should fall back, got %v", err)
	}
	if _, ok := r.Find("starter"); !ok {
		t.Fatalf("built-in index missing starter: %+v", r)
	}
	// a registry the user configured says what went wrong and where to fix it
	_, err = FetchRegistry(ctx, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "protocol_registry") {
		t.Fatalf("custom registry error: %v", err)
	}
	if _, err := FetchRegistry(ctx, "http://example.com/index.json"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http registry should be refused: %v", err)
	}
	// what went wrong is said in words, not as a decoder's or dialer's message
	status, body = http.StatusOK, "<html>sign in</html>"
	if _, err = FetchRegistry(ctx, srv.URL); err == nil || !strings.Contains(err.Error(), "not a pack index") || strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("an HTML page as the registry: %v", err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if _, err = FetchRegistry(ctx, gone.URL); err == nil || !strings.Contains(err.Error(), "can't be reached") || strings.Contains(err.Error(), "dial") {
		t.Fatalf("an unreachable registry: %v", err)
	}
	body = `{"packs":[{"name":"live","description":"d","repo":"https://github.com/x/live"}]}`
	status = http.StatusOK
	if r, err = FetchRegistry(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Find("live"); !ok {
		t.Fatal("reachable default registry should be used")
	}
}

func TestPackSource(t *testing.T) {
	p := Pack{Repo: "https://github.com/x/y"}
	if p.Source() != "https://github.com/x/y" {
		t.Fatal(p.Source())
	}
	p.Commit = strings.Repeat("a", 40)
	if p.Source() != "https://github.com/x/y#"+p.Commit {
		t.Fatal(p.Source())
	}
}

// gitRepo makes a repository to install packs from, isolated from the
// user's git config.
func gitRepo(t *testing.T) (dir string, run func(args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir = t.TempDir()
	run = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	return dir, run
}

// Packs were cloned at whatever HEAD was and `update` pulled silently. Now
// the installed commit is recorded, a check shows what changed without
// moving anything, and applying moves it.
func TestPackIsPinnedAndUpdateShowsWhatChanged(t *testing.T) {
	src, git := gitRepo(t)
	write(t, filepath.Join(src, "pack.yaml"), "name: news\ndescription: d\nrepo: https://github.com/x/news\n")
	write(t, filepath.Join(src, "protocols", "news.yaml"), "name: news\nprompt: OLD PROMPT\n")
	write(t, filepath.Join(src, "protocols", "gone.yaml"), "name: gone\nprompt: soon removed\n")
	git("add", "-A")
	git("commit", "-q", "-m", "one")
	first := git("rev-parse", "HEAD")

	dir := t.TempDir()
	ctx := context.Background()
	name, err := AddPack(ctx, dir, fileURL(src))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(PacksDir(dir), name)
	pv, ok := ReadProvenance(root)
	if !ok || pv.Commit != first || pv.Source != fileURL(src) {
		t.Fatalf("provenance %+v", pv)
	}
	if pks, _ := InstalledPacks(dir); len(pks) != 1 || pks[0].Commit != first {
		t.Fatalf("installed %+v", pks)
	}
	if out, _ := exec.Command("git", "-C", root, "status", "--porcelain").Output(); len(out) != 0 {
		t.Fatalf("provenance file shows up in git status: %s", out)
	}

	// the author changes the pack
	write(t, filepath.Join(src, "protocols", "news.yaml"), "name: news\nprompt: NEW PROMPT\n")
	write(t, filepath.Join(src, "personas", "anchor.yaml"), "name: Anchor\ncharacter: calm\n")
	git("rm", "-q", "protocols/gone.yaml")
	git("add", "-A")
	git("commit", "-q", "-m", "two")
	second := git("rev-parse", "HEAD")

	ups, err := CheckPackUpdates(ctx, dir)
	if err != nil || len(ups) != 1 {
		t.Fatalf("check: %v %+v", err, ups)
	}
	u := ups[0]
	if !u.Pending() || u.From != first || u.To != second {
		t.Fatalf("update %+v", u)
	}
	slices.Sort(u.Changes)
	if want := []string{"added personas/anchor.yaml", "changed protocols/news.yaml", "removed protocols/gone.yaml"}; !slices.Equal(u.Changes, want) {
		t.Fatalf("changes %v, want %v", u.Changes, want)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "protocols", "news.yaml")); !strings.Contains(string(b), "OLD PROMPT") {
		t.Fatal("checking for updates changed the installed pack")
	}

	if err := ApplyPackUpdate(ctx, dir, u); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "protocols", "news.yaml")); !strings.Contains(string(b), "NEW PROMPT") {
		t.Fatal("update was not applied")
	}
	if pv, _ := ReadProvenance(root); pv.Commit != second || pv.Updated.IsZero() {
		t.Fatalf("provenance after update %+v", pv)
	}
	if ups, _ := CheckPackUpdates(ctx, dir); ups[0].Pending() || ups[0].Err != nil {
		t.Fatalf("should be up to date: %+v", ups[0])
	}

	// pinned to a commit: installs that commit and never moves
	pinned := filepath.Join(t.TempDir(), "protocols")
	name, err = AddPack(ctx, pinned, fileURL(src)+"#"+first)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(PacksDir(pinned), name, "protocols", "news.yaml")); !strings.Contains(string(b), "OLD PROMPT") {
		t.Fatal("pinned install did not check out the pinned commit")
	}
	if ups, _ := CheckPackUpdates(ctx, pinned); ups[0].Pending() || !strings.Contains(ups[0].Note, "pinned") || !strings.Contains(ups[0].Note, "add it again") {
		t.Fatalf("pinned pack should say how to move it: %+v", ups[0])
	}
	// a ref that isn't there fails cleanly and leaves nothing behind
	if _, err := AddPack(ctx, t.TempDir(), fileURL(src)+"#no-such-tag"); err == nil || !strings.Contains(err.Error(), "isn't there") {
		t.Fatalf("missing ref: %v", err)
	}
}

func TestAddPackFromMissingRepoFailsInPlainWords(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	_, err := AddPack(context.Background(), dir, fileURL(filepath.Join(t.TempDir(), "nothing-here")))
	if err == nil || !strings.Contains(err.Error(), "couldn't download") {
		t.Fatalf("got %v", err)
	}
	if entries, _ := os.ReadDir(PacksDir(dir)); len(entries) != 0 {
		t.Fatalf("a failed install left %v behind", entries)
	}
}

func TestLocalFolderPackSkipsLinksAndGit(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "protocols", "a.yaml"), "name: a\nprompt: x\n")
	write(t, filepath.Join(src, ".git", "config"), "[core]\n")
	secret := filepath.Join(t.TempDir(), "secret.yaml")
	write(t, secret, "name: s\nprompt: secret\n")
	linked := os.Symlink(secret, filepath.Join(src, "protocols", "s.yaml")) == nil

	dir := t.TempDir()
	name, err := AddPack(context.Background(), dir, src)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(PacksDir(dir), name)
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		t.Error(".git was copied")
	}
	if _, err := os.Lstat(filepath.Join(root, "protocols", "s.yaml")); linked && err == nil {
		t.Error("a symlink was followed and copied")
	}
	pv, _ := ReadProvenance(root)
	if abs, _ := filepath.Abs(src); pv.Source != abs {
		t.Errorf("provenance %+v", pv)
	}
	ups, _ := CheckPackUpdates(context.Background(), dir)
	if len(ups) != 1 || !strings.Contains(ups[0].Note, "copied from a folder") {
		t.Fatalf("folder pack update: %+v", ups)
	}
	var reg Registry
	if err := json.Unmarshal([]byte(`{"packs":[{"name":"x","repo":"r","commit":"c"}]}`), &reg); err != nil || reg.Packs[0].Commit != "c" {
		t.Fatalf("registry commit field: %+v %v", reg, err)
	}
}

// A registry entry's commit used to pin an install for good: bumping it in
// the index, as authors are told to, never reached anyone who had the pack.
func TestRegistryPackFollowsTheIndex(t *testing.T) {
	src, git := gitRepo(t)
	write(t, filepath.Join(src, "protocols", "news.yaml"), "name: news\nprompt: ONE\n")
	git("add", "-A")
	git("commit", "-q", "-m", "one")
	first := git("rev-parse", "HEAD")
	commit := func(prompt string) string {
		write(t, filepath.Join(src, "protocols", "news.yaml"), "name: news\nprompt: "+prompt+"\n")
		git("commit", "-q", "-am", prompt)
		return git("rev-parse", "HEAD")
	}
	repo := fileURL(src)
	entry := Pack{Name: "news", Description: "d", Repo: repo, Commit: first}
	var index string // what the registry serves; "" answers 404
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if index == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(index))
	}))
	defer srv.Close()
	serve := func(pks ...Pack) {
		b, _ := json.Marshal(Registry{Packs: pks})
		index = string(b)
	}
	protos := filepath.Join(t.TempDir(), "protocols")
	ctx := context.Background()
	serve(entry)
	name, err := AddRegistryPack(ctx, protos, srv.URL, entry)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(PacksDir(protos), name)
	if pv, _ := ReadProvenance(root); pv.Registry != "news" || pv.Index != srv.URL || pv.Ref != "" || pv.Commit != first {
		t.Fatalf("provenance %+v", pv)
	}
	checkNow := func() PackUpdate {
		t.Helper()
		ups, err := CheckPackUpdates(ctx, protos)
		if err != nil || len(ups) != 1 {
			t.Fatalf("check: %v %+v", err, ups)
		}
		return ups[0]
	}

	second := commit("TWO")
	commit("THREE, not reviewed yet")
	if u := checkNow(); u.Pending() || u.Err != nil || u.Note != "" {
		t.Fatalf("the index still lists the first commit, so nothing is due: %+v", u)
	}
	entry.Commit = second
	serve(entry)
	u := checkNow()
	if !u.Pending() || u.From != first || u.To != second {
		t.Fatalf("a bumped index should offer its commit: %+v", u)
	}
	if err := ApplyPackUpdate(ctx, protos, u); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "protocols", "news.yaml")); !strings.Contains(string(b), "TWO") {
		t.Fatalf("not moved to the reviewed commit:\n%s", b)
	}
	if pv, _ := ReadProvenance(root); pv.Commit != second || pv.Registry != "news" {
		t.Fatalf("provenance after update %+v", pv)
	}

	cases := []struct {
		name       string
		serve      []Pack
		note, fail string
	}{
		{"delisted", []Pack{{Name: "other", Repo: repo}}, "no longer in the pack registry", ""},
		{"moved", []Pack{{Name: "news", Repo: "https://github.com/someone/else"}}, "now lists it at", ""},
		{"not a commit", []Pack{{Name: "news", Repo: repo, Commit: "--upload-pack=x"}}, "", "isn't a full commit"},
		{"registry down", nil, "", "HTTP 404"},
	}
	for _, c := range cases {
		index = ""
		if c.serve != nil {
			serve(c.serve...)
		}
		u := checkNow()
		if u.Pending() || !strings.Contains(u.Note, c.note) || (c.fail == "") != (u.Err == nil) || u.Err != nil && !strings.Contains(u.Err.Error(), c.fail) {
			t.Errorf("%s: %+v", c.name, u)
		}
	}
}

// .antbot-pack.json travels with identity exports, and its ref was handed to
// git fetch as it was.
func TestProvenanceRefIsCheckedBeforeGit(t *testing.T) {
	src, git := gitRepo(t)
	write(t, filepath.Join(src, "protocols", "a.yaml"), "name: a\nprompt: x\n")
	git("add", "-A")
	git("commit", "-q", "-m", "one")
	protos := t.TempDir()
	name, err := AddPack(context.Background(), protos, fileURL(src))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(PacksDir(protos), name)
	marker := filepath.Join(t.TempDir(), "pwned")
	for _, ref := range []string{"--upload-pack=touch " + marker, "../../x", "-q"} {
		pv, _ := ReadProvenance(root)
		pv.Ref = ref
		if err := writeProvenance(root, pv); err != nil {
			t.Fatal(err)
		}
		ups, _ := CheckPackUpdates(context.Background(), protos)
		if len(ups) != 1 || ups[0].Err == nil || !strings.Contains(ups[0].Err.Error(), "isn't a tag, branch or commit") {
			t.Errorf("ref %q: %+v", ref, ups)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a ref from the provenance file reached git as an option")
	}
	if err := ApplyPackUpdate(context.Background(), protos, PackUpdate{Dir: name, From: "a", To: "--detach"}); err == nil {
		t.Fatal("ApplyPackUpdate should only check out a commit")
	}
}

// A pack whose .git is damaged sent git looking further up, into whatever
// repository holds the Mirrin home, and updates ran there.
func TestDamagedPackNeverReachesAnOuterRepo(t *testing.T) {
	outer, git := gitRepo(t)
	write(t, filepath.Join(outer, "README"), "the user's own repository\n")
	git("add", "-A")
	git("commit", "-q", "-m", "mine")
	git("remote", "add", "origin", fileURL(outer))
	protos := filepath.Join(outer, "protocols")
	if err := os.MkdirAll(filepath.Join(PacksDir(protos), "broken", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	ups, err := CheckPackUpdates(context.Background(), protos)
	if err != nil || len(ups) != 1 || ups[0].Err == nil {
		t.Fatalf("a pack with a damaged .git should fail its check: %v %+v", err, ups)
	}
}

func TestPackProtocolsLinkIsSkipped(t *testing.T) {
	protos := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.yaml"), "name: secret\nprompt: read me\n")
	pack := filepath.Join(PacksDir(protos), "p")
	write(t, filepath.Join(pack, "pack.yaml"), "name: p\ndescription: d\nrepo: https://github.com/x/p\n")
	if err := os.Symlink(outside, filepath.Join(pack, "protocols")); err != nil {
		t.Skip("symlinks not available:", err)
	}
	ps, skipped := LoadAll(protos)
	if _, ok := Find(ps, "secret"); ok || len(skipped) != 1 || !strings.Contains(skipped[0].Message, "symbolic links") {
		t.Fatalf("a linked protocols folder should be skipped: %+v %+v", ps, skipped)
	}
	if problems, _ := LintDir(pack, nil); !Errors(problems) {
		t.Fatalf("lint should refuse a linked protocols folder: %+v", problems)
	}
}

func TestSSHCommand(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		configured string
		want       string
	}{
		{"nothing set", nil, "", "ssh -o BatchMode=yes"},
		{"GIT_SSH_COMMAND", map[string]string{"GIT_SSH_COMMAND": "ssh -i key"}, "", ""},
		{"GIT_SSH", map[string]string{"GIT_SSH": "/usr/bin/plink"}, "", ""},
		{"core.sshCommand", nil, "ssh -i ~/.ssh/work", ""},
	}
	for _, c := range cases {
		got := sshCommand(func(k string) string { return c.env[k] }, func() string { return c.configured })
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGitErrorSpeaksPlainly(t *testing.T) {
	for stderr, want := range map[string]string{
		"Host key verification failed.\nfatal: Could not read from remote repository.":                    "ssh host key",
		"git@github.com: Permission denied (publickey).\r\nfatal: Could not read from remote repository.": "ssh key",
		"remote: Repository not found.\nfatal: repository 'https://github.com/x/y/' not found":            "doesn't exist or isn't public",
		"fatal: couldn't find remote ref v9":                                                              "isn't there",
	} {
		if got := gitError(errors.New("exit status 128"), stderr).Error(); !strings.Contains(got, want) {
			t.Errorf("%q: got %q, want it to mention %q", stderr, got, want)
		}
	}
}

// The default index's address from before the rename (MavrkAI/AntBot) still
// means the default: the signed index, with the built-in copy behind it,
// never an unsigned fetch of whatever that address serves.
func TestTheOldDefaultRegistryIsTheDefault(t *testing.T) {
	for url, want := range map[string]bool{
		"":                 true,
		DefaultRegistryURL: true,
		"https://raw.githubusercontent.com/MavrkAI/AntBot/main/registry/index.json": true,
		"https://raw.githubusercontent.com/MavrkAI/AntBot/main/registry/other.json": false,
		"https://example.com/index.json":                                            false,
	} {
		if got := IsDefaultRegistry(url); got != want {
			t.Errorf("IsDefaultRegistry(%q) = %v", url, got)
		}
	}

	body := `{"packs":[{"name":"live","description":"d","repo":"https://github.com/x/live"}]}`
	signer := newTestSigner(t)
	useRegistryKey(t, signer)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".minisig") {
			_, _ = w.Write([]byte(signer.sign([]byte(body), false)))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	oldURL, oldClient := registryURL, registryClient
	registryURL = srv.URL + "/index.json"
	// Nothing but the test's own server is ever asked.
	registryClient = &http.Client{Transport: onlyHost{strings.TrimPrefix(srv.URL, "http://")}}
	defer func() { registryURL, registryClient = oldURL, oldClient }()

	r, err := FetchRegistry(context.Background(), "https://raw.githubusercontent.com/MavrkAI/AntBot/main/registry/index.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Find("live"); !ok {
		t.Fatalf("the old default address wasn't read as the signed default index: %+v", r)
	}
}

// onlyHost is a transport that reaches one host and refuses every other.
type onlyHost struct{ host string }

func (o onlyHost) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != o.host {
		return nil, errors.New("the test asked " + r.URL.Host)
	}
	return http.DefaultTransport.RoundTrip(r)
}
