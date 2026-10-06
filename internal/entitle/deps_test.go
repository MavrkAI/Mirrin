package entitle

import (
	"os/exec"
	"strings"
	"testing"
)

// The package stays standard-library only: the daemon, the relay and the
// cloud module all import it.
func TestStdlibOnly(t *testing.T) {
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	out, err := exec.Command(gotool, "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range strings.Fields(string(out)) {
		if p != "github.com/MavrkAI/Mirrin/internal/entitle" {
			t.Errorf("non-stdlib dependency: %s", p)
		}
	}
}
