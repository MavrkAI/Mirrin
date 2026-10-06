// Package registry carries the community pack index, index.json, inside the
// binary. Search falls back to it when the published copy can't be reached
// (offline, or before the repository is public) or its signature doesn't
// check out, so finding packs works on first run.
package registry

import _ "embed"

// Index is index.json as of this build.
//
//go:embed index.json
var Index []byte

// PublicKey is the minisign public key the published index.json.minisig is
// checked against. It holds only comments until a maintainer adds the key.
//
//go:embed minisign.pub
var PublicKey []byte
