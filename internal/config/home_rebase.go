package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// homePrefixes are the ways a config can point into a home that moved from
// old to dir: by its full path, and as ~/.antbot (or ~\.antbot) when it
// sits in the user's home.
func homePrefixes(old, dir string) [][2]string {
	pairs := [][2]string{{old, dir}}
	user, _ := userHomeDir()
	if user != "" && samePath(filepath.Dir(old), user) && samePath(filepath.Dir(dir), user) {
		for _, sep := range []string{"/", `\`} {
			pairs = append(pairs, [2]string{"~" + sep + filepath.Base(old), "~" + sep + filepath.Base(dir)})
		}
	}
	return pairs
}

// rebasePlan is the config.yaml a home has once it moves: the file to write
// (the one config.yaml is, or links to), and what to write in it. No data
// means the file needs no change.
type rebasePlan struct {
	file   string // inHome: the rest of its path inside the home
	inHome bool   // the file moves with the home
	data   []byte
	mode   os.FileMode
}

// planRebase works out the config.yaml at path once the home old it is in
// moves: its paths moved from each pair's first prefix to its second
// (RebasePaths), comments and mode kept. It fails when the moved home
// would still find its data, protocols or tools in old, which won't be
// there: a path the prefixes don't catch, or a config.yaml that links into
// old by its full path. Nothing is written.
func planRebase(path, old string, pairs [][2]string) (rebasePlan, error) {
	if t, err := os.Readlink(path); err == nil && filepath.IsAbs(t) {
		if _, inside := underPath(t, old); inside {
			return rebasePlan{}, fmt.Errorf("config.yaml links to %s by its full path; make it a plain file or a relative link", t)
		}
	}
	file := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		file = real // a link is written through, as Save does
	}
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return rebasePlan{}, nil
	}
	if err != nil {
		return rebasePlan{}, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return rebasePlan{}, fmt.Errorf("config.yaml doesn't read: %v", err)
	}
	changed := false
	for _, p := range pairs {
		if RebasePaths(&doc, p[0], p[1]) {
			changed = true
		}
	}
	if err := stillInside(&doc, old); err != nil {
		return rebasePlan{}, err
	}
	if !changed {
		return rebasePlan{}, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return rebasePlan{}, err
	}
	if err := enc.Close(); err != nil {
		return rebasePlan{}, err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(file); err == nil {
		mode = st.Mode().Perm()
	}
	plan := rebasePlan{file: file, data: buf.Bytes(), mode: mode}
	if real, err := filepath.EvalSymlinks(old); err == nil {
		plan.file, plan.inHome = underPath(file, real)
		if !plan.inHome {
			plan.file = file
		}
	}
	return plan, nil
}

// stillInside fails when a folder the twin keeps its own files in
// (data_dir, protocols_dir, tools_dir) is in old.
func stillInside(doc *yaml.Node, old string) error {
	var c struct {
		DataDir      string `yaml:"data_dir"`
		ProtocolsDir string `yaml:"protocols_dir"`
		ToolsDir     string `yaml:"tools_dir"`
	}
	if doc.Kind != 0 {
		_ = doc.Decode(&c) // a setting that doesn't read is left for Load to report
	}
	for _, s := range [][2]string{{"data_dir", c.DataDir}, {"protocols_dir", c.ProtocolsDir}, {"tools_dir", c.ToolsDir}} {
		v := strings.TrimSpace(s[1])
		if v == "" {
			continue
		}
		if _, inside := underPath(expandHome(v), old); inside {
			return fmt.Errorf("%s in config.yaml is %s, which Mirrin can't point at the new folder; change it to a full path, then run Mirrin again", s[0], v)
		}
	}
	return nil
}

// write writes the planned config.yaml once the home has moved to dir.
func (p rebasePlan) write(dir string) error {
	if p.data == nil {
		return nil
	}
	file := p.file
	if p.inHome {
		file = dir + p.file
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, p.data, p.mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, p.mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// RebasePaths changes every string value under n that is the path from, or
// a path inside it (from followed by / or \), to the same path under to. A
// value that only starts with the same letters (~/.antbot-old) is left
// alone, and so are mapping keys. Paths compare as underPath says. It
// reports whether anything changed.
func RebasePaths(n *yaml.Node, from, to string) bool {
	if n == nil || from == "" || from == to {
		return false
	}
	changed := false
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			changed = RebasePaths(c, from, to) || changed
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			changed = RebasePaths(n.Content[i], from, to) || changed
		}
	case yaml.ScalarNode:
		if n.Tag != "" && n.Tag != "!!str" {
			return false
		}
		if rest, ok := underPath(n.Value, from); ok {
			n.Value, changed = rebasedPath(foldSlashes, n.Value, to, rest), true
		}
	}
	return changed
}

// underPath reports whether p is the path from or a path inside it, and
// gives the rest after from ("", or starting with / or \). Case counts
// only where the disk's does, and on Windows / and \ are alike
// (pathsEqual).
func underPath(p, from string) (string, bool) {
	if from == "" || len(p) < len(from) {
		return "", false
	}
	head, rest := p[:len(from)], p[len(from):]
	if !pathsEqual(head, from) {
		return "", false
	}
	if rest != "" && rest[0] != '/' && rest[0] != '\\' {
		return "", false
	}
	return rest, true
}

// rebasedPath is to+rest, written with one separator. Where / and \ are
// alike (foldSlashes: Windows), "~\.old\tts" moved as "~/.mirrin\tts": a
// full path now takes Windows' own \, and a ~ path keeps the one it was
// written with. Elsewhere \ is part of a name, so nothing changes.
func rebasedPath(fold bool, orig, to, rest string) string {
	if !fold {
		return to + rest
	}
	sep := `\`
	if !(len(to) > 1 && to[1] == ':') && !strings.HasPrefix(to, `\\`) {
		if i := strings.IndexAny(orig, `/\`); i >= 0 {
			sep = orig[i : i+1]
		}
	}
	return strings.NewReplacer("/", sep, `\`, sep).Replace(to + rest)
}
