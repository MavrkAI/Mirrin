package config

import (
	"bytes"
	"os"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// configVersion marks a config.yaml saved in layers: only what the owner
// set, over the built-in defaults, so an update that improves a default
// reaches them. A file without it is a full dump from an earlier release,
// where every default was written out and so stayed frozen.
const configVersion = 2

// LayeredVersion is the config_version of a config.yaml saved in layers.
// One without it, or with a lower one, is a full dump from an earlier
// release, which Load brings up to date.
const LayeredVersion = configVersion

// layeredHeader opens a saved config.yaml.
const layeredHeader = `# Mirrin settings. Only what you've changed is here; everything else uses
# the built-in defaults, which get better with updates. The settings you're
# likely to change, with notes, are in config.example.yaml.`

// sticky settings are saved even when they match today's default (unless
// empty): they are the owner's own choices (who the twin is, the model it
// thinks with), or defaults that follow the country the system says it's in,
// which shouldn't change under the owner when they travel.
var sticky = map[string]bool{
	"name": true, "persona": true, "user.name": true, "user.honorific": true,
	"llm.provider": true, "llm.model": true,
	"spending.currency": true, "phone.voice": true, "phone.language": true,
}

// isSticky reports whether a setting is saved even at its default. A
// channel someone turned on is theirs: it stays on whatever the default.
func isSticky(path string) bool {
	if sticky[path] {
		return true
	}
	parts := strings.Split(path, ".")
	return len(parts) == 3 && parts[0] == "channels" && parts[2] == "enabled"
}

// isGuarded reports whether a setting marks where the twin's trust ends:
// what it may do without asking, spend, run, read or reach, who can reach
// it, how long it keeps what it saw, and what leaves the machine in a
// backup. These are saved as they are, even empty or at today's default,
// so a later release that widened a default could never widen it under
// someone already running the twin. (Tightening one for existing owners
// takes a migration in upgrade.) Settings that only inform (the model
// budget, a notice threshold) or whose zero means "today's default" (the
// browser's screenshot limits) aren't guarded: saving them would freeze
// nothing.
func isGuarded(path string) bool {
	if strings.HasPrefix(path, "autonomy.") || strings.HasPrefix(path, "retention.") {
		return true
	}
	switch path {
	case "spending.per_action_limit", "spending.monthly_limit",
		"skills.system.allow_shell", "skills.system.allowed_dirs", "skills.web.allow_hosts",
		"api.listen", "api.remote", "backup.sessions":
		return true
	}
	return false
}

// staleDefaults are values earlier releases wrote into every config as the
// default, since improved. In a full dump they were never the owner's
// choice, so they give way to today's default.
var staleDefaults = map[string][]any{
	"channels.voice.wake_threshold": {0.5, 0.35}, // retuned for the bundled wake model
	// the default persona, before he was Mirrin (persona.Find knows him by both)
	"name":                     {"MAVRK"},
	"persona":                  {"mavrk"},
	"channels.voice.wake_word": {"maverick"},
}

// layeredVersion reads config_version from a config file's YAML.
func layeredVersion(raw []byte) int {
	var v struct {
		Version int `yaml:"config_version"`
	}
	_ = yaml.Unmarshal(raw, &v)
	return v.Version
}

// upgrade brings a config read from raw up to date. A full dump from an
// earlier release keeps every value the owner could have chosen, but a
// default that has since improved gives way to today's, and the time zone
// setup wrote in at install (the system's zone then) becomes "Local" again,
// so the twin follows the system as the laptop travels. A zone other than
// the system's is kept: the owner may have pinned it.
func (c *Config) upgrade(raw []byte) {
	if layeredVersion(raw) >= configVersion {
		return
	}
	def := Default()
	for path, olds := range staleDefaults {
		v, dv := field(reflect.ValueOf(c).Elem(), path), field(reflect.ValueOf(def).Elem(), path)
		if !v.IsValid() || !dv.IsValid() || !v.CanSet() {
			continue
		}
		for _, old := range olds {
			if reflect.DeepEqual(v.Interface(), reflect.ValueOf(old).Convert(v.Type()).Interface()) {
				v.Set(dv)
				break
			}
		}
	}
	if tz := strings.TrimSpace(c.User.Timezone); tz != "" && tz == LocalTimezone() {
		c.User.Timezone = "Local"
	}
}

// field finds a setting by its dotted YAML path, or the zero Value.
func field(v reflect.Value, path string) reflect.Value {
	for _, name := range strings.Split(path, ".") {
		if v.Kind() != reflect.Struct {
			return reflect.Value{}
		}
		t, found := v.Type(), false
		for i := 0; i < t.NumField(); i++ {
			if yamlName(t.Field(i)) == name {
				v, found = v.Field(i), true
				break
			}
		}
		if !found {
			return reflect.Value{}
		}
	}
	return v
}

// yamlName is the key a struct field is saved under.
func yamlName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if name == "" {
		name = strings.ToLower(f.Name)
	}
	return name
}

// onDisk is the config file as it is before a save: its settings over the
// defaults, and, for a file saved in layers, which settings it names.
type onDisk struct {
	cfg     *Config
	named   map[string]bool
	layered bool
}

func readDisk() onDisk {
	d := onDisk{cfg: Default(), named: map[string]bool{}}
	defer func() { d.cfg.expand() }() // whichever config d ends up with
	raw, err := os.ReadFile(Path())
	if err != nil {
		return d
	}
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) != nil {
		return d
	}
	if yaml.Unmarshal(raw, d.cfg) != nil {
		d.cfg = Default()
		return d
	}
	d.cfg.upgrade(raw)
	if d.layered = layeredVersion(raw) >= configVersion; d.layered {
		namedPaths(&root, "", d.named)
	}
	return d
}

// namedPaths notes the dotted path of every key in a YAML tree.
func namedPaths(n *yaml.Node, prefix string, out map[string]bool) {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		namedPaths(n.Content[0], prefix, out)
		return
	}
	if n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		p := join(prefix, n.Content[i].Value)
		out[p] = true
		namedPaths(n.Content[i+1], p, out)
	}
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// layered is the YAML Save writes: config_version, then each setting that
// differs from its default, that this save changes, that the file already
// named (the owner wrote it, even if it matches the default), or that is
// sticky. Everything else is left to the defaults.
func (c *Config) layered() ([]byte, error) {
	cur := c.Clone()
	cur.expand()
	def := Default()
	def.expand()
	l := layering{disk: readDisk()}
	root := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content, scalarKey("config_version"), encoded(reflect.ValueOf(configVersion)))
	l.walk(reflect.ValueOf(cur).Elem(), reflect.ValueOf(def).Elem(), reflect.ValueOf(l.disk.cfg).Elem(), "", root)
	doc := &yaml.Node{Kind: yaml.DocumentNode, HeadComment: layeredHeader, Content: []*yaml.Node{root}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type layering struct{ disk onDisk }

func (l layering) walk(cur, def, disk reflect.Value, path string, out *yaml.Node) {
	t := cur.Type()
	for i := 0; i < t.NumField(); i++ {
		name := yamlName(t.Field(i))
		if name == "" || name == "-" {
			continue
		}
		p := join(path, name)
		cv, dv, kv := cur.Field(i), def.Field(i), disk.Field(i)
		var node *yaml.Node
		switch ft := t.Field(i).Type; {
		case ft.Kind() == reflect.Struct:
			child := &yaml.Node{Kind: yaml.MappingNode}
			l.walk(cv, dv, kv, p, child)
			// A section that is empty, and empty by default (backup before it
			// is set up), isn't written just for its guarded settings.
			if len(child.Content) > 0 && !(cv.IsZero() && dv.IsZero() && onlyGuarded(child, p)) {
				node = child
			}
		case ft.Kind() == reflect.Map && ft.Key().Kind() == reflect.String && ft.Elem().Kind() == reflect.Struct:
			node = l.entries(cv, dv, kv, p)
		case l.keep(p, cv, dv, kv):
			node = encoded(cv)
		}
		if node != nil {
			out.Content = append(out.Content, scalarKey(name), node)
		}
	}
}

// onlyGuarded reports whether every setting in a section's node is guarded.
func onlyGuarded(n *yaml.Node, path string) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i+1].Kind == yaml.MappingNode || !isGuarded(join(path, n.Content[i].Value)) {
			return false
		}
	}
	return true
}

// keep decides one setting.
func (l layering) keep(path string, cur, def, disk reflect.Value) bool {
	switch {
	case isGuarded(path), isSticky(path) && !cur.IsZero():
		return true
	case !same(cur, def), !same(cur, disk):
		return true
	}
	return l.disk.layered && l.disk.named[path]
}

// entries saves the entries of a map of settings (llm.providers) that differ
// from the default entry. Loading replaces an entry whole, so a saved entry
// is written whole (its empty fields left out, as they load empty anyway).
func (l layering) entries(cur, def, disk reflect.Value, path string) *yaml.Node {
	keys := cur.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	out := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range keys {
		p := join(path, k.String())
		v := cur.MapIndex(k)
		var dv, kv reflect.Value
		if def.Len() > 0 {
			dv = def.MapIndex(k)
		}
		if disk.Len() > 0 {
			kv = disk.MapIndex(k)
		}
		if !dv.IsValid() || !same(v, dv) || !kv.IsValid() || !same(v, kv) || (l.disk.layered && l.disk.named[p]) {
			out.Content = append(out.Content, scalarKey(k.String()), nonEmpty(v))
		}
	}
	if len(out.Content) == 0 {
		return nil
	}
	return out
}

// same compares two settings, an empty list or map matching a missing one.
func same(a, b reflect.Value) bool {
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	if k := a.Kind(); (k == reflect.Slice || k == reflect.Map) && a.Len() == 0 && b.Len() == 0 {
		return true
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

func scalarKey(name string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
}

func encoded(v reflect.Value) *yaml.Node {
	n := &yaml.Node{}
	if err := n.Encode(v.Interface()); err != nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	}
	return n
}

// nonEmpty encodes a struct with only its non-empty fields.
func nonEmpty(v reflect.Value) *yaml.Node {
	if v.Kind() != reflect.Struct {
		return encoded(v)
	}
	out := &yaml.Node{Kind: yaml.MappingNode}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name := yamlName(t.Field(i))
		if name == "" || name == "-" || v.Field(i).IsZero() {
			continue
		}
		out.Content = append(out.Content, scalarKey(name), encoded(v.Field(i)))
	}
	return out
}
