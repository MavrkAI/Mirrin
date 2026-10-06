//go:build !darwin || !cgo

package browser

// quietKeychainLocked needs the Security framework (cgo on a Mac); without
// it the status can't be read without risking a prompt.
func quietKeychainLocked() (locked, ok bool) { return false, false }
