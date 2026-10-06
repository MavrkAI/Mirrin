package persona

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// Every bundled persona file parses: Bundled skips one that doesn't, which
// would quietly drop a character from the menu.
func TestEveryBundledPersonaParses(t *testing.T) {
	for name, b := range map[string][]byte{"mirrin": mirrinYAML, "pickoo": pickooYAML, "nyra": nyraYAML} {
		var p Persona
		if err := yaml.Unmarshal(b, &p); err != nil {
			t.Errorf("%s.yaml: %v", name, err)
		}
	}
}
