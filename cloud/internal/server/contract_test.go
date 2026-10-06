package server

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/cloud/cloudtest"
)

// The daemon's contract suite (docs/cloud-api.md §7), the same one the fake
// in internal/cloud/cloudtest passes, run against this server in --dev mode
// with a real SQLite store.
func TestContract(t *testing.T) {
	e := newEnv(t, false, nil)
	cloudtest.Contract(t, e.url)
}
