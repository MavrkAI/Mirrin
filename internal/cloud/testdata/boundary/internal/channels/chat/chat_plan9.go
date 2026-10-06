//go:build plan9

// On plan9 only, chat imports cloud: go list never sees it from the
// platforms tests run on, so the source scan must.
package chat

import _ "github.com/MavrkAI/Mirrin/internal/cloud"
