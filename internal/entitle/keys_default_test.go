//go:build !mirrin_devkeys

package entitle

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

// A default build, which is what every release is, trusts no dev key: the
// dev seeds are public, so tokens signed with them are refused.
func TestDefaultBuildRefusesDevKeys(t *testing.T) {
	devKids := []string{"ent-dev-a", "ent-dev-b", "dl-dev-a", "dl-dev-b"}
	for _, set := range []map[string]ed25519.PublicKey{EntitlementKeys, DenyListKeys} {
		for kid, pub := range set {
			if strings.Contains(kid, "dev") {
				t.Errorf("trusts dev kid %q", kid)
			}
			// The dev keys, and the dev recipe applied to this kid.
			for _, dev := range append(devKids, kid) {
				if pub.Equal(devPub(dev)) {
					t.Errorf("%s is the dev key %s", kid, dev)
				}
			}
		}
	}
	for _, kid := range devKids[:2] {
		tok, err := Sign(claims(t), kid, devPriv(kid))
		if err != nil {
			t.Fatal(err)
		}
		_, err = Verify(tok, EntitlementKeys, t0.Add(time.Hour))
		wantErr(t, kid, err, ErrSignature, "unknown key")
	}
	for _, kid := range devKids[2:] {
		tok, err := SignDenyList(denyList(), kid, devPriv(kid))
		if err != nil {
			t.Fatal(err)
		}
		_, err = VerifyDenyList(tok, DenyListKeys, t0.Add(time.Hour))
		wantErr(t, kid, err, ErrSignature, "unknown key")
	}
}
