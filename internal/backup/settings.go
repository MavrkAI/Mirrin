package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// LoadSettings reads the backup section of the config file at path,
// without the rest of the config having to load. The daemon reads it before
// every run, so `mirrin backup init` takes effect without a restart.
func LoadSettings(path string) (config.Backup, error) {
	var doc struct {
		Backup config.Backup `yaml:"backup"`
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config.Backup{}, nil
	}
	if err != nil {
		return config.Backup{}, err
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return config.Backup{}, fmt.Errorf("config.yaml doesn't read (%v)", err)
	}
	return doc.Backup, nil
}

// SaveSettings replaces the backup section of the config file at path and
// leaves everything else in it as it was, comments included.
func SaveSettings(path string, s config.Backup) error {
	// One edit of config.yaml: the running twin saving a setting meanwhile
	// can't write back a copy without this section (config.EditFile).
	return config.EditFile(path, func() error { return saveSettings(path, s) })
}

func saveSettings(path string, s config.Backup) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("config.yaml doesn't read (%v); fix it, then try again", err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("config.yaml isn't a settings file Mirrin can change; fix it, then try again")
	}
	var val yaml.Node
	if err := val.Encode(s); err != nil {
		return err
	}
	root := doc.Content[0]
	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "backup" {
			if len(val.Content) == 0 {
				root.Content = append(root.Content[:i], root.Content[i+2:]...)
			} else {
				root.Content[i+1] = &val
			}
			replaced = true
			break
		}
	}
	if !replaced && len(val.Content) > 0 {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "backup"}, &val)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out)
}
