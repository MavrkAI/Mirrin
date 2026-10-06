package cloud

import "crypto/ed25519"

// TunnelKey is the device key, for signing relay hellos: each entitlement
// is bound to it (cnf), and a hosted relay carries a handle only for the
// key its entitlement names. It fails with ErrNotLinked before StartLink
// has made the key. The key never leaves this machine; only its signatures
// do.
func (c *Client) TunnelKey() (ed25519.PrivateKey, error) {
	return loadKey(c.state.dir)
}
