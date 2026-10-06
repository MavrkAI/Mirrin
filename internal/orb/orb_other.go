//go:build !(darwin && cgo)

package orb

// Available reports native support.
func Available() bool { return false }

// Show is a no-op without native support.
func Show(url string, w, h, margin int) {}

// Preload is a no-op.
func Preload(url string, w, h, margin int) {}

// Hide is a no-op.
func Hide() {}

// SetInteractive is a no-op.
func SetInteractive(on bool) {}

// Reload is a no-op.
func Reload() {}
