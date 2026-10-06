package voice

// ListenNow opens a listening window as if the wake word had been said: the
// owner clicked the orb. It reports false when this channel isn't running
// its always-on wake loop (voice off, or push-to-talk).
func (c *Channel) ListenNow() bool {
	if !c.wakeLive.Load() {
		return false
	}
	select {
	case c.listenNow <- struct{}{}:
	default: // one is already on its way
	}
	return true
}
