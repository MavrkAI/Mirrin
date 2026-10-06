package server

import (
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/cloud/internal/dns"
)

// Nightly CI runs each of these for 60 s (.github/workflows/fuzz.yml).

// Whatever allowedHandle accepts is one DNS label within the rules, and
// never a reserved name or a brand however spelled.
func FuzzAllowedHandle(f *testing.F) {
	for _, s := range []string{"ember-otter-42", "www", "adm1n", "g00gle", "a--b", "-x-", "xn--80ak6aa92e", "rnavrk", strings.Repeat("a", 33), "ünï"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, h string) {
		if allowedHandle(h) != nil {
			return
		}
		if dns.Label(h) != nil || len(h) < 3 || len(h) > 32 || strings.Contains(h, "--") {
			t.Fatalf("%q allowed", h)
		}
		sk := skeleton(h)
		for _, b := range brands {
			if strings.Contains(sk, skeleton(b)) {
				t.Fatalf("%q allowed but holds %s", h, b)
			}
		}
	})
}
