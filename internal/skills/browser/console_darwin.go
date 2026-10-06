package browser

import (
	"os"
	"syscall"
)

// atConsole reports whether this user is the one logged in at the Mac's
// screen: loginwindow hands /dev/console to that user (root while the login
// window shows).
func atConsole() bool {
	fi, err := os.Stat("/dev/console")
	if err != nil {
		return true // can't tell: leave the keychain alone
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Getuid()
}
