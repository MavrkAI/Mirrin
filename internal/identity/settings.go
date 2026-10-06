package identity

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// homeToken stands for the twin's home (~/.mirrin) in an exported config, so
// paths follow the twin to wherever its new home is. It keeps AntBot's
// spelling: archives are read by older builds too, which know only this one.
const homeToken = "$ANTBOT_HOME" // rename:keep: older builds read archives with this token

// homeTokens are the tokens an import reads: the one written, and the one a
// later version may write.
var homeTokens = []string{homeToken, "$MIRRIN_HOME"}

// machineLocal are settings that describe this machine rather than the twin:
// where things are kept, what is installed, which address the API binds. An
// import keeps the destination's values, or its defaults on a fresh machine.
var machineLocal = [][]string{
	{"data_dir"}, {"protocols_dir"}, {"tools_dir"},
	{"api", "listen"}, {"api", "remote"},
	{"channels", "voice", "whisper_bin"}, {"channels", "voice", "whisper_model"}, {"channels", "voice", "kokoro_dir"},
	{"skills", "calendar", "credentials_file"}, {"skills", "calendar", "token_file"},
	{"backup"}, // this machine's backups (internal/backup), never another's
}

// makePortable rewrites paths in an exported config: under the twin's home to
// $ANTBOT_HOME/…, under the user's home to ~/….
func makePortable(cfg map[string]any, home string) {
	homes := []string{filepath.Clean(home)}
	if abs, err := filepath.Abs(home); err == nil && abs != homes[0] {
		homes = append(homes, abs)
	}
	user, _ := os.UserHomeDir()
	mapStrings(cfg, func(s string) string {
		for _, h := range homes {
			if rel, ok := under(s, h); ok {
				return joinToken(homeToken, rel)
			}
		}
		if rel, ok := under(s, user); ok && user != "" {
			return joinToken("~", rel)
		}
		return s
	})
}

// localize turns $ANTBOT_HOME/… (or $MIRRIN_HOME/…) and ~/… in an imported
// config into paths on this machine.
func localize(cfg map[string]any, home string) {
	user, _ := os.UserHomeDir()
	mapStrings(cfg, func(s string) string {
		if rel, ok := homeRel(s); ok {
			return filepath.Join(home, filepath.FromSlash(rel))
		}
		if rel, ok := tokenRel(s, "~"); ok && user != "" {
			return filepath.Join(user, filepath.FromSlash(rel))
		}
		return s
	})
}

// repairMaxTokens drops the blank max_tokens the format 1 exporter wrote (and
// its importer copied into configs), which stopped the config loading.
// Without it the default applies.
func repairMaxTokens(cfg map[string]any) {
	if llm, ok := cfg["llm"].(map[string]any); ok {
		if s, isString := llm["max_tokens"].(string); isString && strings.TrimSpace(s) == "" {
			delete(llm, "max_tokens")
		}
	}
}

// upgradeLegacy turns the source machine's absolute paths in a format 1
// config back into portable ones.
func upgradeLegacy(cfg map[string]any) {
	src := ""
	for key, base := range map[string]string{"data_dir": "data", "protocols_dir": "protocols", "tools_dir": "tools"} {
		if v, _ := cfg[key].(string); v != "" && src == "" {
			if dir, b := splitAnySep(v); b == base && dir != "" {
				src = dir
			}
		}
	}
	if src == "" {
		return
	}
	user := ""
	if dir, b := splitAnySep(src); b == brand.HomeDirName || slices.Contains(brand.LegacyHomeDirs, b) {
		user = dir
	}
	mapStrings(cfg, func(s string) string {
		if rel, ok := underAnySep(s, src); ok {
			return joinToken(homeToken, rel)
		}
		if rel, ok := underAnySep(s, user); ok && user != "" {
			return joinToken("~", rel)
		}
		return s
	})
}

// mergeSettings builds the config an import writes. The archive's settings
// win; anything it does not mention stays as it was here. Secrets the export
// left out keep this machine's values, and machine-local settings stay this
// machine's. It notes in r what is still to set and what to check.
func mergeSettings(cur, in map[string]any, m Manifest, home string, r *Result) map[string]any {
	if m.Format < 2 {
		upgradeLegacy(in)
	}
	localize(in, home)
	r.Warnings = keyWarnings(cur, in, nil)
	merged, _ := mergeValue(cur, in, nil).(map[string]any)
	if merged == nil {
		merged = map[string]any{}
	}
	repairMaxTokens(merged)
	if fullDump(cur) || fullDump(in) {
		// Merged with a full dump from an earlier release, the settings are a
		// full dump too, stale defaults and all. Kept as layered, they would
		// skip the upgrade that retires those defaults (the old wake
		// threshold, the zone frozen at install), and every default in them
		// would count as the owner's choice for good.
		delete(merged, "config_version")
	}
	for _, p := range machineLocal {
		if v, ok := lookup(cur, p); ok {
			set(merged, p, v)
		} else {
			del(merged, p)
		}
	}
	r.Missing, r.MCPEnv = finishSecrets(merged, m.Secrets)
	if w := allowHostsWarning(cur, merged); w != "" {
		r.Warnings = append(r.Warnings, w)
	}
	return merged
}

// fullDump reports whether settings are a full dump from a release before
// config.yaml was saved in layers: they have no config_version, or an older
// one. No settings at all (a fresh machine) are nothing of the kind.
func fullDump(m map[string]any) bool {
	if len(m) == 0 {
		return false
	}
	v, _ := m["config_version"].(int)
	return v < config.LayeredVersion
}

// allowHostsWarning notes hosts on the local network the archive lets
// fetch_url reach (skills.web.allow_hosts): 192.168.1.0/24 was the old
// machine's network, and here may be someone else's.
func allowHostsWarning(cur, merged map[string]any) string {
	list := func(m map[string]any) []string {
		v, _ := lookup(m, []string{"skills", "web", "allow_hosts"})
		xs, _ := v.([]any)
		var out []string
		for _, x := range xs {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	now, before := list(merged), list(cur)
	var added []string
	for _, h := range now {
		if !contains(before, h) {
			added = append(added, h)
		}
	}
	if len(added) == 0 {
		return ""
	}
	return fmt.Sprintf("skills.web.allow_hosts, from the archive, lets the twin fetch pages from %s on this machine's network; if that isn't a network you trust here, change it in config.yaml", strings.Join(added, ", "))
}

// endpoints are settings that say where a key next to them is sent.
var endpoints = map[string]bool{
	"base_url": true, "url": true, "homeserver": true, "site": true,
	"server": true, "http": true, "imap_host": true, "smtp_host": true,
}

// keyWarnings finds the endpoints an archive changes where this machine keeps
// its own key for them, which would send that key somewhere new.
// (llm.base_url also serves the keys under llm.providers, so a key anywhere
// under a setting's parent counts.)
func keyWarnings(cur, in map[string]any, path []string) []string {
	var out []string
	keeps := keepsKey(cur, in, path)
	for k, v := range in {
		p := extend(path, k)
		switch t := v.(type) {
		case map[string]any:
			if c, ok := cur[k].(map[string]any); ok {
				out = append(out, keyWarnings(c, t, p)...)
			}
		case string:
			old, _ := cur[k].(string)
			if keeps && endpoints[k] && t != "" && scrubLeaf(p, old) != t {
				out = append(out, fmt.Sprintf("%s is now %s, from the archive, and your key here will be sent there; if you don't recognise it, change it in config.yaml", label(p), t))
			}
		}
	}
	sort.Strings(out)
	return out
}

// keepsKey reports whether cur holds a key, at its level or below, that the
// merge keeps because the archive left it out.
func keepsKey(cur, in map[string]any, path []string) bool {
	for k, v := range cur {
		p := extend(path, k)
		switch t := v.(type) {
		case map[string]any:
			sub, _ := in[k].(map[string]any)
			if keepsKey(t, sub, p) {
				return true
			}
		case string:
			o, given := in[k].(string)
			if t != "" && isSecretKey(k) && (!given || scrubLeaf(p, t) == o) {
				return true
			}
		}
	}
	return false
}

// mergeValue lays over on top of base. A string the export scrubbed (blank
// secret, URL without credentials) keeps base's real value; so does an MCP
// env value the export left blank where this machine names a variable
// ("$GITHUB_TOKEN", which scrubbing keeps).
func mergeValue(base, over any, path []string) any {
	switch o := over.(type) {
	case map[string]any:
		b, ok := base.(map[string]any)
		if !ok {
			return o
		}
		out := make(map[string]any, len(b)+len(o))
		for k, v := range b {
			out[k] = v
		}
		for k, v := range o {
			if bv, ok := b[k]; ok {
				out[k] = mergeValue(bv, v, extend(path, k))
			} else {
				out[k] = v
			}
		}
		return out
	case []any:
		b, ok := base.([]any)
		if !ok {
			return o
		}
		switch {
		case isMCPArgs(path):
			return mergeArgs(b, o)
		case isMCPServers(path):
			return mergeByName(b, o, path)
		}
		return o
	case string:
		if b, ok := base.(string); ok && b != o && (scrubLeaf(path, b) == o || o == "" && isMCPEnv(path)) {
			return b
		}
		return o
	}
	return over
}

// mergeArgs keeps this machine's value for each argument the export scrubbed.
func mergeArgs(base, over []any) []any {
	scrubbed := append([]any(nil), base...)
	scrubArgs(scrubbed)
	out := append([]any(nil), over...)
	for i := range out {
		if i >= len(base) {
			break
		}
		o, ok1 := out[i].(string)
		b, ok2 := base[i].(string)
		s, _ := scrubbed[i].(string)
		if ok1 && ok2 && o == s && b != s {
			out[i] = b
		}
	}
	return out
}

// mergeByName merges MCP servers by name, keeping servers only this machine has.
func mergeByName(base, over []any, path []string) []any {
	byName := map[string]any{}
	for i, x := range base {
		byName[itemLabel(x, i)] = x
	}
	used := map[string]bool{}
	var out []any
	for i, x := range over {
		l := itemLabel(x, i)
		if bx, ok := byName[l]; ok {
			out = append(out, mergeValue(bx, x, extend(path, l)))
			used[l] = true
		} else {
			out = append(out, x)
		}
	}
	for i, x := range base {
		if !used[itemLabel(x, i)] {
			out = append(out, x)
		}
	}
	return out
}

// checkSettings parses a config the way `mirrin` will and validates it.
func checkSettings(b []byte) (*config.Config, error) {
	cfg := config.Default()
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

// readSettings reads a config file as a map; nil when it is missing or unreadable.
func readSettings(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]any
	if yaml.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// LocalAPI returns the address a twin in home serves its local API on (empty
// when it has none) and its data folder, which holds the API token. It reads
// config.yaml directly, so a config that no longer loads still points at the
// twin that is running from it.
func LocalAPI(home string) (listen, dataDir string) {
	cfg := readSettings(filepath.Join(home, "config.yaml"))
	listen = config.Default().API.Listen
	if v, ok := lookup(cfg, []string{"api", "listen"}); ok {
		if s, ok := v.(string); ok {
			listen = s
		}
	}
	return listen, dirSetting(cfg, "data_dir", home, "data")
}

// dirSetting resolves a directory setting the way config.Load does.
func dirSetting(cfg map[string]any, key, home, def string) string {
	v, _ := cfg[key].(string)
	if v == "" {
		return filepath.Join(home, def)
	}
	if rel, ok := homeRel(v); ok {
		return filepath.Join(home, filepath.FromSlash(rel))
	}
	return expandTilde(v)
}

// mapStrings replaces every string in v (maps and lists, recursively).
func mapStrings(v any, fn func(string) string) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok {
				t[k] = fn(s)
			} else {
				mapStrings(x, fn)
			}
		}
	case []any:
		for i, x := range t {
			if s, ok := x.(string); ok {
				t[i] = fn(s)
			} else {
				mapStrings(x, fn)
			}
		}
	}
}

// under reports whether p is dir or inside it, and the relative rest.
func under(p, dir string) (string, bool) {
	if dir == "" || dir == "." {
		return "", false
	}
	if p == dir {
		return "", true
	}
	if rest, ok := strings.CutPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator)); ok {
		return filepath.ToSlash(rest), true
	}
	return "", false
}

// underAnySep is under for paths written on another OS.
func underAnySep(p, dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	if p == dir {
		return "", true
	}
	for _, sep := range []string{"/", `\`} {
		if rest, ok := strings.CutPrefix(p, dir+sep); ok {
			return strings.ReplaceAll(rest, `\`, "/"), true
		}
	}
	return "", false
}

// splitAnySep splits a path from any OS into its directory and last element.
func splitAnySep(p string) (dir, base string) {
	p = strings.TrimRight(p, `/\`)
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

func joinToken(token, rel string) string {
	if rel == "" {
		return token
	}
	return token + "/" + rel
}

// homeRel reports whether s is one of homeTokens, or one followed by /…,
// and the rest.
func homeRel(s string) (string, bool) {
	for _, t := range homeTokens {
		if rel, ok := tokenRel(s, t); ok {
			return rel, true
		}
	}
	return "", false
}

// tokenRel reports whether s is token or token/…, and the rest.
func tokenRel(s, token string) (string, bool) {
	if s == token {
		return "", true
	}
	for _, sep := range []string{"/", `\`} {
		if rest, ok := strings.CutPrefix(s, token+sep); ok {
			return rest, true
		}
	}
	return "", false
}

func lookup(m map[string]any, path []string) (any, bool) {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func set(m map[string]any, path []string, v any) {
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[path[len(path)-1]] = v
}

func del(m map[string]any, path []string) {
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
	delete(m, path[len(path)-1])
}
