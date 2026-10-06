//go:build darwin && cgo

package browser

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>

// keychainState is the default keychain's lock state without any prompt:
// 1 locked, 0 unlocked, -1 unknown.
static int keychainState(void) {
	SecKeychainStatus status = 0;
	if (SecKeychainGetStatus(NULL, &status) != errSecSuccess) {
		return -1;
	}
	return (status & kSecUnlockStateStatus) ? 0 : 1;
}
*/
import "C"

// quietKeychainLocked asks the Security framework whether the login
// keychain is locked. Reading its status never shows a prompt, so it is
// safe with someone at the screen, or nobody watching it (a Mac that logs in
// automatically keeps its keychain locked). ok is false when it can't tell.
func quietKeychainLocked() (locked, ok bool) {
	switch C.keychainState() {
	case 1:
		return true, true
	case 0:
		return false, true
	}
	return false, false
}
