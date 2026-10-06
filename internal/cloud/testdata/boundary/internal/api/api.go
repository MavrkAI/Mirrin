// Package api is a core package that adds the import: a violation.
package api

import "github.com/MavrkAI/Mirrin/internal/cloud"

// Paid is exactly what the boundary forbids: a core feature asking whether
// the user pays.
func Paid() bool { return cloud.Linked() }
