package sigv4

import (
	"os/exec"
	"strings"
	"testing"
)

// The package stays standard-library only: the daemon's S3 backup target and
// the cloud module's Route 53 and R2 adapters all import it.
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
		if p != "github.com/MavrkAI/Mirrin/internal/sigv4" {
			t.Errorf("non-stdlib dependency: %s", p)
		}
	}
}
