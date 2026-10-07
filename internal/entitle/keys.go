package entitle

import "crypto/ed25519"

// The public keys every build trusts: a current and a next key per purpose.
// The control plane signs with the current key; the next one ships a release
// ahead, so a yearly rotation needs no flag day. /v1/keys publishes the same
// set.
//
// No production key exists yet, so both sets are empty and a default build
// verifies nothing: every entitlement and deny list fails with ErrSignature.
// At launch the real public keys go here under real kids (ent-2026a,
// dl-2026a and so on). Development builds that talk to the fake control
// plane add the public dev keys with -tags mirrin_devkeys (keys_dev.go);
// release builds never set that tag.

// EntitlementKeys verify entitlements. Only ent-* kids belong here.
var EntitlementKeys = map[string]ed25519.PublicKey{}

// DenyListKeys verify deny lists. Only dl-* kids belong here, and never a key
// that is also in EntitlementKeys.
var DenyListKeys = map[string]ed25519.PublicKey{}
