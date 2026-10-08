package daemon

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// A launchd job counts as the service manager (it waits for the lock rather
// than quitting), and a restart asks that job.
func TestServiceManagerSeesItsJob(t *testing.T) {
	t.Setenv(config.ServiceEnv, "")
	for label, want := range map[string]bool{"mirrin": true, "": false, "application.com.apple.Terminal.1": false} {
		t.Setenv("XPC_SERVICE_NAME", label)
		if got := underServiceManager(); got != want {
			t.Errorf("XPC_SERVICE_NAME=%q: underServiceManager = %v", label, got)
		}
		if got := launchdLabel(); got != "mirrin" {
			t.Errorf("XPC_SERVICE_NAME=%q: launchdLabel = %q", label, got)
		}
	}
	t.Setenv("XPC_SERVICE_NAME", "")
	t.Setenv(config.ServiceEnv, "1")
	if !underServiceManager() {
		t.Error("the service marker isn't the service manager")
	}
	if got := passwordEnv(""); got != "MIRRIN_EMAIL_PASSWORD" {
		t.Errorf("passwordEnv(\"\") = %q", got)
	}
	if got := passwordEnv("WORK_EMAIL_PASSWORD"); got != "WORK_EMAIL_PASSWORD" {
		t.Errorf("the configured name isn't the hint: %q", got)
	}
}
