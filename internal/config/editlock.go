package config

import (
	"errors"
	"os"
	"time"
)

// Edit runs change while holding config.yaml's edit lock (config.yaml.lock
// beside it), so two programs that each read the file, change their part
// and write it back (the running twin saving a setting from the menu or a
// chat, `mirrin backup init` saving the backup section) never both start
// from the same file and lose each other's change. It waits up to ten
// seconds for another edit to finish; after that it goes ahead regardless,
// as before the lock (an edit is never refused over it).
func Edit(change func() error) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	return EditFile(Path(), change)
}

// EditFile is Edit for the settings file at path, whose lock is beside it
// (path.lock). Nothing is made in the home: editing a config elsewhere
// leaves the home alone.
func EditFile(path string, change func() error) error {
	if f, err := lockWait(path+".lock", editWait); err == nil {
		defer f.Close()
	} // else no lock to be had: edit anyway, as before there was one
	return change()
}

// editWait is how long Edit waits for another program's edit.
var editWait = 10 * time.Second

var errLocked = errors.New("locked")
