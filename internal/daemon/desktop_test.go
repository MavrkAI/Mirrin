package daemon

import "fmt"

// Tests never show real notifications on the machine running them. As on a
// server with no desktop, a message nothing else could deliver fails.
func init() {
	desktopNotify = func(_, body string) error {
		return fmt.Errorf("no channel available to deliver: %s", truncate(body, 80))
	}
}
