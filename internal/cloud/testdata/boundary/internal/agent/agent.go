// Package agent reaches cloud through reach: a violation.
package agent

import "github.com/MavrkAI/Mirrin/internal/reach"

// Paid asks through a side door.
func Paid() bool { return reach.Linked() }
