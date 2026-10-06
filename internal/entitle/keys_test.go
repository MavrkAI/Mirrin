package entitle

import (
	"crypto/ed25519"
	"testing"
)

// Each purpose has at most a current and a next key, under its own prefix,
// and no key serves two purposes.
func TestCompiledInKeys(t *testing.T) {
	seen := map[string]string{}
	for prefix, set := range map[string]map[string]ed25519.PublicKey{
		entitlementPrefix: EntitlementKeys,
		denyListPrefix:    DenyListKeys,
	} {
		if len(set) > 2 {
			t.Errorf("%s*: %d keys, want at most current and next", prefix, len(set))
		}
		for kid, pub := range set {
			if err := checkKid(kid, prefix); err != nil {
				t.Errorf("%s: %v", kid, err)
			}
			if len(pub) != ed25519.PublicKeySize {
				t.Errorf("%s: %d-byte key", kid, len(pub))
			}
			if other, ok := seen[string(pub)]; ok {
				t.Errorf("%s and %s share a key", kid, other)
			}
			seen[string(pub)] = kid
		}
	}
}
