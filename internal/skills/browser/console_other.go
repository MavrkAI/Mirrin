//go:build !darwin

package browser

// atConsole only matters on a Mac, where the login keychain can be locked.
func atConsole() bool { return true }
