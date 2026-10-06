package notices

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embedded copies must be exactly the files at the root of the
// repository; `make notices` writes both.
func TestCopiesMatchTheRepository(t *testing.T) {
	for name, embedded := range map[string]string{"LICENSE": License, "THIRD_PARTY_NOTICES": ThirdParty} {
		root, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(root) != embedded {
			t.Errorf("internal/notices/%s differs from %s at the root of the repository. Run `make notices` and commit the result", name, name)
		}
	}
	if !strings.Contains(License, "MIT License") || !strings.Contains(ThirdParty, "Go standard library") {
		t.Error("the notices are missing a licence text")
	}
}
