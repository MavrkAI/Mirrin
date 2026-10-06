package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/registry"
)

// TestRegistryIndexIsValid keeps registry/index.json honest: every pack needs a
// name, description and a GitHub repo URL, and names must be unique. This is
// the check that runs on pull requests that add a pack. It reads the copy
// built into the binary, which is the same file.
func TestRegistryIndexIsValid(t *testing.T) {
	var r Registry
	if err := json.Unmarshal(registry.Index, &r); err != nil {
		t.Fatalf("index.json is not valid JSON: %v", err)
	}
	if len(r.Packs) == 0 {
		t.Fatal("index.json lists no packs")
	}
	seen := map[string]bool{}
	for i, p := range r.Packs {
		if p.Name == "" || p.Description == "" || p.Repo == "" {
			t.Errorf("pack %d: name, description and repo are required: %+v", i, p)
		}
		if strings.ToLower(p.Name) != p.Name || strings.ContainsAny(p.Name, " /#") {
			t.Errorf("pack %q: name must be lowercase with no spaces, slashes or #", p.Name)
		}
		if !strings.HasPrefix(p.Repo, "https://github.com/") && !strings.HasPrefix(p.Repo, "https://gitlab.com/") && !strings.HasPrefix(p.Repo, "https://codeberg.org/") {
			t.Errorf("pack %q: repo must be an https URL on a known forge: %s", p.Name, p.Repo)
		}
		if strings.Contains(p.Repo, "#") {
			t.Errorf("pack %q: pin with \"commit\", not a # in the repo URL", p.Name)
		}
		if p.Commit != "" && !reCommit.MatchString(p.Commit) {
			t.Errorf("pack %q: commit must be a full commit hash: %s", p.Name, p.Commit)
		}
		if seen[p.Name] {
			t.Errorf("pack %q listed twice", p.Name)
		}
		seen[p.Name] = true
		if len(p.Description) > 200 {
			t.Errorf("pack %q: description over 200 characters", p.Name)
		}
	}
}
