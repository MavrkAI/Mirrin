//go:build mirrin_devkeys

package entitle

import (
	"crypto/ed25519"
	"testing"
	"time"
)

// With -tags mirrin_devkeys, the compiled-in sets are exactly the documented
// dev keys, SHA-256("mirrin dev key " + kid), and tokens signed with them
// verify.
func TestDevKeysAreTheDocumentedOnes(t *testing.T) {
	for kid, keys := range map[string]map[string]ed25519.PublicKey{
		"ent-dev-a": EntitlementKeys, "ent-dev-b": EntitlementKeys,
		"dl-dev-a": DenyListKeys, "dl-dev-b": DenyListKeys,
	} {
		if pub, ok := keys[kid]; !ok || !pub.Equal(devPub(kid)) {
			t.Errorf("%s is not SHA-256(\"mirrin dev key %s\")", kid, kid)
		}
	}
	if len(EntitlementKeys) != 2 || len(DenyListKeys) != 2 {
		t.Fatalf("%d and %d keys, want the two dev keys each", len(EntitlementKeys), len(DenyListKeys))
	}
	tok, err := Sign(claims(t), "ent-dev-a", devPriv("ent-dev-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(tok, EntitlementKeys, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	dl, err := SignDenyList(denyList(), "dl-dev-b", devPriv("dl-dev-b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDenyList(dl, DenyListKeys, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}
