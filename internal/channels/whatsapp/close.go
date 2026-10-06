//go:build !nowhatsapp

package whatsapp

import "go.mau.fi/whatsmeow"

// Close releases the device store Start opened. The daemon calls it once it
// is done with an instance, including after a failed connect, so retrying
// while offline doesn't leave a database handle open per attempt.
func (c *Channel) Close() error {
	cl := c.cl()
	if cl == nil {
		return nil
	}
	cl.Disconnect()
	return closeStore(cl)
}

// closeStore closes the database behind a client's device store.
func closeStore(cl *whatsmeow.Client) error {
	if cl == nil || cl.Store == nil {
		return nil
	}
	if s, ok := cl.Store.Container.(interface{ Close() error }); ok {
		return s.Close()
	}
	return nil
}
