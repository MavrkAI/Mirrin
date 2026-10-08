package brand

import "testing"

func TestIsServiceLabel(t *testing.T) {
	for label, want := range map[string]bool{
		"mirrin": true,
		"":       false, "0": false, "application.com.apple.Terminal.123": false, "Mirrin": false,
	} {
		if got := IsServiceLabel(label); got != want {
			t.Errorf("IsServiceLabel(%q) = %v", label, got)
		}
	}
}

// The names everything else is built from: the command and service label,
// the name people read and the home folder.
func TestNames(t *testing.T) {
	if Name != "mirrin" || DisplayName != "Mirrin" || HomeDirName != ".mirrin" {
		t.Fatalf("names: %q %q %q", Name, DisplayName, HomeDirName)
	}
}
