// Package homelock is a home's claim: the lock file in its data folder that
// the one twin running from that home holds. Two twins never share the
// WhatsApp session, the bot tokens or the memory, and a home is never moved
// or replaced under a running one. It imports nothing of the project's, so
// config can look before it moves a home and daemon can take the claim.
package homelock

import (
	"errors"
	"os"
	"path/filepath"
)

// Name is the claim's file in a home's data folder. It keeps the name it had
// as AntBot: an AntBot twin and a Mirrin one on the same data folder must
// lock the same file to keep out of each other's way.
const Name = "antbot.lock" // rename:keep

// ErrLocked is what Lock returns when another process holds the lock.
var ErrLocked = errors.New("locked")

// Path is the claim's file for dataDir.
func Path(dataDir string) string { return filepath.Join(dataDir, Name) }

// InUse reports whether a twin holds the claim on dataDir right now. It
// never creates anything.
func InUse(dataDir string) bool {
	path := Path(dataDir)
	if _, err := os.Stat(path); err != nil {
		return false // never claimed
	}
	f, err := Lock(path)
	if err == nil {
		f.Close()
		return false
	}
	return errors.Is(err, ErrLocked)
}
