package daemon

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// A launchd job AntBot installed still counts as the service manager (it
// waits for the lock rather than quitting), and a restart asks that job.
func TestServiceManagerSeesAntBotsJob(t *testing.T) {
	for _, k := range []string{config.ServiceEnv, "ANTBOT_SERVICE", "OPENHUMAN_SERVICE"} {
		t.Setenv(k, "")
	}
	for label, want := range map[string]bool{"mirrin": true, "antbot": true, "": false, "application.com.apple.Terminal.1": false} {
		t.Setenv("XPC_SERVICE_NAME", label)
		if got := underServiceManager(); got != want {
			t.Errorf("XPC_SERVICE_NAME=%q: underServiceManager = %v", label, got)
		}
		wantLabel := label
		if !want {
			wantLabel = "mirrin"
		}
		if got := launchdLabel(); got != wantLabel {
			t.Errorf("XPC_SERVICE_NAME=%q: launchdLabel = %q, want %q", label, got, wantLabel)
		}
	}
	t.Setenv("XPC_SERVICE_NAME", "")
	t.Setenv("ANTBOT_SERVICE", "1")
	if !underServiceManager() {
		t.Error("AntBot's service marker isn't the service manager")
	}
	if got := passwordEnv(""); got != "MIRRIN_EMAIL_PASSWORD" {
		t.Errorf("passwordEnv(\"\") = %q", got)
	}
	if got := passwordEnv("ANTBOT_EMAIL_PASSWORD"); got != "ANTBOT_EMAIL_PASSWORD" {
		t.Errorf("the configured name isn't the hint: %q", got)
	}
}
