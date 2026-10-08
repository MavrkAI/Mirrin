//go:build mirrin_devkeys

package entitle

import "crypto/ed25519"

// DEV KEYS, for development builds only (-tags mirrin_devkeys). They are
// derived from public seeds, SHA-256("mirrin dev key " + kid), so anyone can
// sign with them: a build that trusts them trusts everyone. The fake control
// plane and --dev use them. Nothing that ships may be built with this tag.

func init() {
	EntitlementKeys["ent-dev-a"] = mustKey("MchHdvUSGoiXqNFrZgt-HjeA2kPmvqX3rFCHUixw_68") // current
	EntitlementKeys["ent-dev-b"] = mustKey("k2Sczq9i4XqNbck7XF7IiZNg8tfLZYWraTXrEXsp0w4") // next
	DenyListKeys["dl-dev-a"] = mustKey("pvGQvDPrmN74XOO1ZHZamJrbTDkzxFqP6j1gUdLYmwk")     // current
	DenyListKeys["dl-dev-b"] = mustKey("ubDpWyTrJ7kPKi-K2JTrbZkG_r3cM-HlLVAYeT8rW_c")     // next
}

func mustKey(s string) ed25519.PublicKey {
	k, err := ParseKey(s)
	if err != nil {
		panic("entitle: bad compiled-in key " + s)
	}
	return k
}
