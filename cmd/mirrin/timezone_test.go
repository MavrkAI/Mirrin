package main

import "testing"

// The regression: first run wrote the system's zone into the config, which
// pinned the twin to home time for every later trip.
func TestFirstRunFollowsTheSystemsTimeZone(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("TZ", "Australia/Melbourne")
	if tz := firstRunConfig().User.Timezone; tz != "Local" {
		t.Fatalf("a new twin should follow the system's zone, got %q", tz)
	}
}
