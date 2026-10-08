package events

// ScreenOpen counts a presence screen open on this computer (not the orb)
// until the returned func is called.
func (b *Bus) ScreenOpen() (closed func()) {
	b.mu.Lock()
	b.screens++
	b.mu.Unlock()
	done := false
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !done {
			done = true
			b.screens--
		}
	}
}

// ScreensOpen is how many presence screens are open on this computer.
func (b *Bus) ScreensOpen() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.screens
}
