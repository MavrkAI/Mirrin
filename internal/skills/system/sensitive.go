package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Places under the home directory that hold keys, passwords or live
// sessions. Touching them is always treated as dangerous, so it needs the
// owner's yes even when reading files is otherwise automatic.
var sensitiveDirs = []struct{ path, why string }{
	{".ssh", "SSH keys"},
	{".gnupg", "GPG keys"},
	{".aws", "cloud credentials"},
	{".azure", "cloud credentials"},
	{".config/gcloud", "cloud credentials"},
	{".kube", "cloud credentials"},
	{".docker", "cloud credentials"},
	{".oci", "cloud credentials"},
	{".terraform.d", "cloud credentials"},
	{".config/gh", "saved passwords and tokens"},
	{".password-store", "saved passwords and tokens"},
	{".local/share/keyrings", "the keychain"},
	{"Library/Keychains", "the keychain"},
	{"Library/Cookies", "browser cookies and saved logins"},
	{"Library/Safari", "browser cookies and saved logins"},
	{"Library/Application Support/Google/Chrome", "browser cookies and saved logins"},
	{"Library/Application Support/Chromium", "browser cookies and saved logins"},
	{"Library/Application Support/BraveSoftware", "browser cookies and saved logins"},
	{"Library/Application Support/Microsoft Edge", "browser cookies and saved logins"},
	{"Library/Application Support/Arc", "browser cookies and saved logins"},
	{"Library/Application Support/Firefox", "browser cookies and saved logins"},
	{".mozilla", "browser cookies and saved logins"},
	{".config/google-chrome", "browser cookies and saved logins"},
	{".config/chromium", "browser cookies and saved logins"},
	{".config/BraveSoftware", "browser cookies and saved logins"},
	{".config/microsoft-edge", "browser cookies and saved logins"},
	{"AppData/Local/Google/Chrome", "browser cookies and saved logins"},
	{"AppData/Roaming/Mozilla", "browser cookies and saved logins"},
}

// sensitiveName recognises secret files wherever they are.
func sensitiveName(base string) string {
	b := strings.ToLower(base)
	switch {
	case b == ".env" || b == ".envrc" || strings.HasPrefix(b, ".env."):
		return "a secrets file"
	case b == ".netrc" || b == ".pgpass" || b == ".git-credentials" || b == ".npmrc" || b == ".pypirc" || b == ".my.cnf":
		return "saved passwords and tokens"
	case strings.HasPrefix(b, "id_rsa") || strings.HasPrefix(b, "id_dsa") || strings.HasPrefix(b, "id_ecdsa") || strings.HasPrefix(b, "id_ed25519"):
		if strings.HasSuffix(b, ".pub") {
			return ""
		}
		return "a private key"
	}
	switch filepath.Ext(b) {
	case ".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".kdbx":
		return "a private key or password vault"
	case ".keychain", ".keychain-db":
		return "the keychain"
	}
	return ""
}

// guard decides which paths need the owner's yes, and says why.
type guard struct {
	dirs   []struct{ path, why string } // absolute
	own    []string                     // Mirrin's own home and data: off-limits, not just asked about
	copies homeCopies                   // copies of the home beside it: off-limits too
}

// whyOwn is why the twin's own files are sensitive.
const whyOwn = "Mirrin's own settings, memory and keys"

// errOwnFiles is the refusal for the twin's own settings, memory and keys.
// The twin once raised its own spending limit by editing config.yaml (an
// approval clicked through), and read the model key into the conversation
// on the way. Asking isn't enough: these change only on the settings pages.
var errOwnFiles = fmt.Errorf("that's Mirrin's own settings, memory and keys, which the twin never reads or changes itself. Settings change on the screen's pages (Spending, Trust, Channels, Accounts…) or by the owner in config.yaml; tell the user which one")

// ownFile reports whether p is inside Mirrin's own home or data.
func (g guard) ownFile(p string) bool {
	abs, err := filepath.Abs(expand(p))
	if err != nil {
		return false
	}
	for _, c := range []string{abs, resolve(abs)} {
		for _, d := range g.own {
			if c != "" && (within(d, c) || within(resolve(d), c)) {
				return true
			}
		}
		if c != "" && g.copies.holds(c) {
			return true
		}
	}
	return false
}

// mentionsOwn reports whether a shell command names Mirrin's own home, or
// one from before the rename (~/.antbot, ~/.openhuman) left beside it.
func (g guard) mentionsOwn(command string) bool {
	c := strings.ToLower(command)
	for _, name := range append([]string{brand.HomeDirName}, brand.LegacyHomeDirs...) {
		if strings.Contains(c, name) {
			return true
		}
	}
	for _, d := range g.own {
		if d != "" && strings.Contains(c, strings.ToLower(d)) {
			return true
		}
	}
	return false
}

// newGuard covers the usual secret places plus Mirrin's own home, the
// copies of it kept beside it (homeCopies), and any extra paths it keeps
// elsewhere (data directory, Google token files).
func newGuard(private []string) guard {
	var g guard
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range sensitiveDirs {
			g.dirs = append(g.dirs, struct{ path, why string }{filepath.Join(home, filepath.FromSlash(d.path)), d.why})
		}
	}
	home := config.Home()
	for _, p := range append([]string{home}, private...) {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(expand(p)); err == nil {
			g.dirs = append(g.dirs, struct{ path, why string }{abs, whyOwn})
			g.own = append(g.own, abs)
		}
	}
	g.copies = newHomeCopies(home)
	return g
}

// homeCopies are the folders beside a home that hold a copy of a twin's
// settings, memory and keys: what a restore set aside (<home>.before-
// restore-…) or left half done (.<home>.restore-…), and, beside the
// default home, the homes from before the rename (~/.antbot, ~/.openhuman)
// with their own copies.
type homeCopies struct {
	parent string
	names  []string // the home's own name first
}

func newHomeCopies(home string) homeCopies {
	abs, err := filepath.Abs(expand(home))
	if err != nil || home == "" {
		return homeCopies{}
	}
	c := homeCopies{parent: filepath.Dir(abs), names: []string{filepath.Base(abs)}}
	if config.IsDefaultHome(abs) {
		c.names = append(c.names, brand.HomeDirName)
		c.names = append(c.names, brand.LegacyHomeDirs...)
	}
	return c
}

// holds reports whether p is in one of those folders.
func (c homeCopies) holds(p string) bool {
	if c.parent == "" {
		return false
	}
	rel, err := filepath.Rel(c.parent, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	first := strings.ToLower(strings.SplitN(filepath.ToSlash(rel), "/", 2)[0])
	for i, name := range c.names {
		name = strings.ToLower(name)
		if (i > 0 && first == name) || strings.HasPrefix(first, name+".before-restore-") || strings.HasPrefix(first, "."+name+".restore-") {
			return true
		}
	}
	return false
}

// reason is why p is sensitive, or "" when it isn't. Both the path as
// written and where its symlinks lead are checked, so a link can't hide a key.
func (g guard) reason(p string) string {
	abs, err := filepath.Abs(expand(p))
	if err != nil {
		return ""
	}
	for _, c := range []string{abs, resolve(abs)} {
		if c == "" {
			continue
		}
		for _, d := range g.dirs {
			if within(d.path, c) || within(resolve(d.path), c) {
				return d.why
			}
		}
		if g.copies.holds(c) {
			return whyOwn
		}
		if why := sensitiveName(filepath.Base(c)); why != "" {
			return why
		}
	}
	return ""
}

// gate makes a file tool's call dangerous when its path is sensitive, and
// makes the approval say why (and, for a write, show what would be written).
func (g guard) gate(f *tools.Func) tools.Tool {
	name := f.Spec().Name
	f.WithRiskFor(func(_ context.Context, call tools.Call) tools.Risk {
		var in struct{ Path string }
		if tools.Decode(call, &in) == nil && g.reason(in.Path) != "" {
			return tools.RiskDangerous
		}
		return f.Risk()
	})
	return tools.WithSummaryAndCheck(f, func(call tools.Call) string {
		var in struct{ Path, Content string }
		_ = tools.Decode(call, &in)
		s := name + " " + in.Path
		if why := g.reason(in.Path); why != "" {
			s += " (sensitive: " + why + ")"
		}
		if name == "write_file" {
			content := []rune(in.Content)
			if len(content) > 10_000 { // a whole file of prose; past that it's a blob
				content = append(content[:10_000], []rune(fmt.Sprintf("\n…[%d more characters not shown]", len(content)-10_000))...)
			}
			s += fmt.Sprintf(", %d bytes:\n%s", len(in.Content), string(content))
		}
		return s
	}, func(_ context.Context, call tools.Call) error {
		var in struct{ Path string }
		if tools.Decode(call, &in) == nil && g.ownFile(in.Path) {
			return errOwnFiles
		}
		return nil
	})
}

// resolve follows symlinks in p, or in its parent when p doesn't exist yet
// (a file about to be written). It returns "" when neither resolves.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(r, filepath.Base(p))
	}
	return ""
}

// within reports whether p is dir or inside it. Case is ignored: macOS and
// Windows treat ~/.SSH as ~/.ssh, and a false alarm only means one more ask.
func within(dir, p string) bool {
	if dir == "" {
		return false
	}
	dir, p = strings.ToLower(filepath.Clean(dir)), strings.ToLower(filepath.Clean(p))
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}
