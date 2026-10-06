//go:build !windows

package browser

// windowsProxy only means something on Windows.
func windowsProxy() *sysProxy { return nil }
