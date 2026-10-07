//go:build mirrin_devkeys

package entitle

import "crypto/ed25519"

// DEV KEYS, for development builds only (-tags mirrin_devkeys). They are
// derived from public seeds, SHA-256("antbot dev key " + kid), so anyone can
// sign with them: a build that trusts them trusts everyone. The fake control
// plane and --dev use them. Nothing that ships may be built with this tag.

func init() {
	EntitlementKeys["ent-dev-a"] = mustKey("uoPg6Pnuio7xFnssi_6zfZxcWyhF0OfhuMbac1OhHG0") // current
	EntitlementKeys["ent-dev-b"] = mustKey("H3zuHIMzeH0cBGna7A1QdFX4AlX-uDDEixnElxlRPVU") // next
	DenyListKeys["dl-dev-a"] = mustKey("omzILxuZcl2JLo1RV60NWGUESN358ZnHpCUJ9si0qms")     // current
	DenyListKeys["dl-dev-b"] = mustKey("ITlqhKTRYpvzati78SXZWG1A4TUhnwZ-t0NlmzMtfsM")     // next
}

func mustKey(s string) ed25519.PublicKey {
	k, err := ParseKey(s)
	if err != nil {
		panic("entitle: bad compiled-in key " + s)
	}
	return k
}
