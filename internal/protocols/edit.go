package protocols

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Edit changes when a protocol runs (schedule) or whether it runs at all
// (enabled); nil leaves either as it is. The rest of the file stays as it
// was written, comments included. A protocol from a pack isn't changed in
// the pack, which an update would overwrite: it is copied into dir, the
// owner's own folder, with the change, and that copy shadows the pack's
// (LoadAll), so it no longer follows the pack. Edit returns the file it
// wrote.
func Edit(dir string, p Protocol, schedule *string, enabled *bool) (string, error) {
	if p.Source == "" {
		return "", fmt.Errorf("%q has no file to change", p.Name)
	}
	b, err := os.ReadFile(p.Source)
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("%s isn't a protocol I can change; edit it by hand", p.Source)
	}
	m := doc.Content[0]
	if p.Pack != "" {
		// A pack's file may take its name from the file; the copy says it.
		setKey(m, "name", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: p.Name}, true)
	}
	if schedule != nil {
		v := strings.TrimSpace(*schedule)
		setKey(m, "schedule", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}, false)
	}
	if enabled != nil {
		setKey(m, "enabled", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(*enabled)}, false)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	_ = enc.Close()
	// What was written must still load as this protocol.
	var check Protocol
	if err := yaml.Unmarshal(out.Bytes(), &check); err != nil {
		return "", err
	}
	if check.Name == "" {
		check.Name = p.Name
	}
	if err := Check(check); err != nil {
		return "", err
	}
	if p.Pack == "" {
		path := p.Source
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real // a file kept elsewhere stays linked
		}
		return p.Source, replaceFile(path, out.Bytes())
	}
	return writeNew(dir, Slug(p.Name), out.Bytes())
}

// setKey sets key in a mapping to v, adding it (at the top when first)
// if it isn't there.
func setKey(m *yaml.Node, key string, v *yaml.Node, first bool) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v.HeadComment, v.LineComment = m.Content[i+1].HeadComment, m.Content[i+1].LineComment
			m.Content[i+1] = v
			return
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	if first {
		m.Content = append([]*yaml.Node{k, v}, m.Content...)
		return
	}
	m.Content = append(m.Content, k, v)
}

// replaceFile writes b over path all at once, so a crash never leaves half a protocol.
func replaceFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".edit-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// writeNew writes b to a new file in dir named after slug, never replacing
// one already there (another protocol whose name has the same letters).
func writeNew(dir, slug string, b []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	for i := 1; i <= 9; i++ {
		name := slug + ".yaml"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.yaml", slug, i)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(b); err != nil {
			f.Close()
			os.Remove(path)
			return "", err
		}
		return path, f.Close()
	}
	return "", fmt.Errorf("couldn't find a free file name for %q in %s", slug, dir)
}
