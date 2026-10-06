// Package daemon may import cloud: it wires the twin together.
package daemon

import "github.com/MavrkAI/Mirrin/internal/cloud"

// Linked is allowed here.
func Linked() bool { return cloud.Linked() }
