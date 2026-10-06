package daemon

import (
	"context"
	"testing"
)

// Close kept the claim on the home: the lock file stayed open, so on
// Windows the home couldn't be deleted or moved (a restore, an uninstall)
// until the whole program quit, and nothing else could claim it.
func TestCloseGivesUpTheHome(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	if err := d.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !HomeInUse(d.cfg.DataDir) {
		t.Fatal("a claimed home reads as free")
	}
	d.Close()
	if HomeInUse(d.cfg.DataDir) {
		t.Fatal("the home is still claimed after Close")
	}
}
