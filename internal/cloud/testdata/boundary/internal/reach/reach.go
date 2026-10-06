// Package reach may import cloud: Cloud is one of its modes.
package reach

import "github.com/MavrkAI/Mirrin/internal/cloud"

// Linked is allowed here.
func Linked() bool { return cloud.Linked() }
