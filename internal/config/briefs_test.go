package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Meeting briefs are on unless the owner says watch.meeting_briefs: false,
// and a config that never mentions them is written back without the key.
func TestMeetingBriefsAreOnUnlessSwitchedOff(t *testing.T) {
	cfg := Default()
	if !cfg.Watch.Briefs() {
		t.Fatal("off by default")
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "meeting_briefs") {
		t.Fatalf("an untouched config gained the key:\n%s", out)
	}
	if err := yaml.Unmarshal([]byte("watch:\n  enabled: true\n  meeting_briefs: false\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Watch.Briefs() {
		t.Fatal("meeting_briefs: false left them on")
	}
}
