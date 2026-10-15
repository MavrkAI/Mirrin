package config

import "testing"

// A new twin listens for its name on this computer as soon as voice is set
// up, with no trip to the menu; push-to-talk is a choice, not the default.
func TestDefaultListensForItsName(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	c := Default()
	if !c.Channels.Voice.Enabled || c.Channels.Voice.Mode != "wake" {
		t.Fatalf("default voice: enabled %v, mode %q", c.Channels.Voice.Enabled, c.Channels.Voice.Mode)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
