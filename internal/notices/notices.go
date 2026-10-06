// Package notices carries the licence texts that go with every mirrin
// program, so a copy installed on its own (by install.sh, install.ps1,
// Homebrew or a container) has them too: Mirrin's own licence, and the
// notices and full licence texts of the third-party code built into it.
// `mirrin licenses` prints them.
//
// The files here are copies of LICENSE and THIRD_PARTY_NOTICES at the root of
// the repository (go:embed can't reach outside this folder). `make notices`
// writes both, and `make lint`, CI and this package's tests fail when they
// differ.
package notices

import _ "embed"

// License is Mirrin's own licence (MIT), which covers its source code.
//
//go:embed LICENSE
var License string

// ThirdParty lists every third-party module linked into the mirrin program,
// with its licence text, and says which only the WhatsApp channel needs.
//
//go:embed THIRD_PARTY_NOTICES
var ThirdParty string
