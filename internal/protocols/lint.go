package protocols

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/MavrkAI/Mirrin/internal/persona"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Problem is one lint finding.
type Problem struct {
	File    string
	Level   string // error, warn, or info (what was found, for a clean file)
	Message string
}

func (p Problem) String() string { return fmt.Sprintf("%s: %s: %s", p.File, p.Level, p.Message) }

// KnownSkills are the skill names accepted in `requires` besides tool names.
var KnownSkills = slices.Sorted(maps.Keys(skillTools))

// credentials are shapes of real secrets. Lint looks for these rather than
// for words, so "Secret Santa" or "remind me to change my password" is fine.
var credentials = []struct {
	what string
	re   *regexp.Regexp
}{
	{"an API key (sk-…)", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)},
	{"a GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"an AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
}

// reAssigned finds "password: hunter2" and the like; the value is checked separately.
var reAssigned = regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|api[_ -]?key|secret|token)\s*[:=]\s*(\S+)`)

// credentialIn names the kind of credential s seems to contain, or "".
func credentialIn(s string) string {
	for _, c := range credentials {
		if c.re.MatchString(s) {
			return c.what
		}
	}
	// A written-out value is secret-shaped: six or more characters that are not
	// all letters ("password: stop" is prose; "password: hunter2" is not) and not a {{var}}.
	for _, m := range reAssigned.FindAllStringSubmatch(s, -1) {
		v := strings.TrimRight(m[1], `.,;:!?)"'`)
		if len(v) >= 6 && !strings.HasPrefix(v, "{{") && strings.IndexFunc(v, func(r rune) bool { return !unicode.IsLetter(r) }) >= 0 {
			return "a password or key written out"
		}
	}
	return ""
}

// LintFile checks one protocol file for the things that break sharing.
func LintFile(path string, knownTools []string) []Problem {
	name := filepath.Base(path)
	b, err := os.ReadFile(path)
	if err != nil {
		return []Problem{{name, "error", err.Error()}}
	}
	var p Protocol
	if err := yaml.Unmarshal(b, &p); err != nil {
		return []Problem{{name, "error", "not valid YAML: " + err.Error()}}
	}
	var out []Problem
	add := func(level, msg string) { out = append(out, Problem{name, level, msg}) }

	if strings.TrimSpace(p.Name) == "" {
		add("error", "name is required")
	} else if strings.ToLower(p.Name) != p.Name {
		add("warn", "name should be lowercase; users say it out loud")
	}
	if strings.TrimSpace(p.Prompt) == "" {
		add("error", "prompt is required")
	}
	if strings.TrimSpace(p.Description) == "" {
		add("warn", "description is empty; it is what people see when they search")
	}
	if p.Version == "" {
		add("warn", "version is empty (use semver, e.g. 0.1.0)")
	}
	if p.Author == "" {
		add("warn", "author is empty")
	}
	if p.Schedule != "" {
		if _, err := cron.ParseStandard(p.Schedule); err != nil {
			add("error", "schedule is not a valid 5-field cron expression: "+err.Error())
		} else if !strings.Contains(p.Prompt, "NOTHING_TO_REPORT") {
			add("warn", "scheduled protocols should mention NOTHING_TO_REPORT so quiet runs stay quiet")
		}
	}
	known := map[string]bool{}
	for _, k := range KnownSkills {
		known[k] = true
	}
	for _, t := range knownTools {
		known[t] = true
	}
	for _, r := range p.Requires {
		if len(knownTools) > 0 && !known[r] {
			add("warn", fmt.Sprintf("requires %q is not a known tool or skill", r))
		}
	}
	// vars used vs declared
	used := map[string]bool{}
	for _, m := range reVar.FindAllStringSubmatch(p.Prompt, -1) {
		used[m[1]] = true
	}
	for v := range used {
		if _, ok := p.Vars[v]; !ok {
			add("error", fmt.Sprintf("prompt uses {{%s}} but vars does not declare it", v))
		}
	}
	for v := range p.Vars {
		if !used[v] {
			add("warn", fmt.Sprintf("var %q is declared but never used in the prompt", v))
		}
	}
	for k, v := range p.Vars {
		if v.Required && v.Default != "" {
			add("warn", fmt.Sprintf("var %q is required but has a default; drop one", k))
		}
		if what := credentialIn(v.Default); what != "" {
			add("error", fmt.Sprintf("var %q defaults to what looks like %s; never put credentials in a protocol", k, what))
		}
	}
	if len(p.Prompt) > 4000 {
		add("warn", "prompt is very long; protocols work best as a brief, not an essay")
	}
	if what := credentialIn(p.Prompt); what != "" {
		add("error", "prompt contains what looks like "+what+"; never put credentials in a protocol")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Level < out[j].Level })
	return out
}

// lintPersona checks one persona file in the pack that installs as pack.
func lintPersona(path, pack string) []Problem {
	name := "personas/" + filepath.Base(path)
	b, err := os.ReadFile(path)
	if err != nil {
		return []Problem{{name, "error", err.Error()}}
	}
	var p persona.Persona
	if err := yaml.Unmarshal(b, &p); err != nil {
		return []Problem{{name, "error", "not valid YAML: " + err.Error()}}
	}
	var out []Problem
	add := func(level, msg string) { out = append(out, Problem{name, level, msg}) }
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Character) == "" {
		add("error", "name and character are required")
	}
	if strings.TrimSpace(p.Tagline) == "" {
		add("warn", "tagline is empty; it is what people see in the Persona menu")
	}
	if p.Version == "" {
		add("warn", "version is empty (use semver, e.g. 0.1.0)")
	}
	if p.Author == "" {
		add("warn", "author is empty")
	}
	id := p.ID
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	for _, b := range persona.Bundled() {
		if strings.EqualFold(id, b.ID) {
			add("warn", fmt.Sprintf("id %q is a built-in persona's; yours will show up as <pack>/%s next to it, so a distinct id reads better", id, id))
		}
	}
	if !Errors(out) {
		// Say what the pack offers, so a clean persona isn't silent.
		add("info", fmt.Sprintf("persona %s shows up as %s/%s in the Persona menu", strings.TrimSpace(p.Name), pack, id))
	}
	return out
}

// lintPackName is the folder a pack at dir installs as: from its repo in
// pack.yaml, else the folder's own name.
func lintPackName(dir string) string {
	var p Pack
	if b, err := os.ReadFile(filepath.Join(dir, "pack.yaml")); err == nil && yaml.Unmarshal(b, &p) == nil && p.Repo != "" {
		return packName(p.Repo)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return packName(dir)
}

// LintDir lints every protocol file in dir, or in dir/protocols for a pack,
// plus a pack's pack.yaml and personas/.
func LintDir(dir string, knownTools []string) ([]Problem, int) {
	var out []Problem
	files := 0
	yamlFiles := func(d string) []string {
		entries, err := os.ReadDir(d)
		if err != nil {
			return nil
		}
		var paths []string
		for _, e := range entries {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || (ext != ".yaml" && ext != ".yml") || e.Name() == "pack.yaml" || e.Name() == "vars.yaml" {
				continue
			}
			paths = append(paths, filepath.Join(d, e.Name()))
		}
		return paths
	}
	if pdir, err := packProtocolsDir(dir); err != nil {
		out = append(out, Problem{"protocols", "error", err.Error()})
	} else {
		for _, path := range yamlFiles(pdir) {
			files++
			out = append(out, LintFile(path, knownTools)...)
		}
	}
	if st, err := os.Lstat(filepath.Join(dir, "personas")); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		out = append(out, Problem{"personas", "error", errSymlinkInPack.Error()})
	} else {
		for _, path := range yamlFiles(filepath.Join(dir, "personas")) {
			files++
			out = append(out, lintPersona(path, lintPackName(dir))...)
		}
	}
	if pk := filepath.Join(dir, "pack.yaml"); files > 0 {
		if b, err := os.ReadFile(pk); err == nil {
			var p Pack
			if yaml.Unmarshal(b, &p) != nil || p.Name == "" || p.Description == "" || p.Repo == "" {
				out = append(out, Problem{"pack.yaml", "error", "pack.yaml needs name, description and repo"})
			}
		}
	}
	return out, files
}

// Errors reports whether any problem is an error.
func Errors(ps []Problem) bool {
	for _, p := range ps {
		if p.Level == "error" {
			return true
		}
	}
	return false
}
