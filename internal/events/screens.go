package events

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// The presence screens open on this computer (not the orb, nor a phone or a
// wall screen), and whether each is in sight: a tab left in the background
// or a minimised window is open but shows the owner nothing, so a page
// handed over by voice must still bring a screen up.

// Screen is one presence screen open on this computer.
type Screen struct {
	b    *Bus
	id   string
	once sync.Once
}

// ScreenHere counts a presence screen open on this computer until Close,
// in sight or not. Its ID is what the page sends back when that changes
// (SeenScreen); it is random, so no other page can speak for it.
func (b *Bus) ScreenHere(visible bool) *Screen {
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	s := &Screen{b: b, id: hex.EncodeToString(raw[:])}
	b.mu.Lock()
	if b.screens == nil {
		b.screens = map[string]bool{}
	}
	b.screens[s.id] = visible
	b.mu.Unlock()
	return s
}

// ID names the screen to the page showing it.
func (s *Screen) ID() string { return s.id }

// Close stops counting the screen; twice is once.
func (s *Screen) Close() {
	s.once.Do(func() {
		s.b.mu.Lock()
		delete(s.b.screens, s.id)
		s.b.mu.Unlock()
	})
}

// SeenScreen notes whether the screen with id is in sight now, and reports
// whether there is such a screen.
func (b *Bus) SeenScreen(id string, visible bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.screens[id]; !ok {
		return false
	}
	b.screens[id] = visible
	return true
}

// ScreenOpen counts a presence screen open and in sight on this computer
// until the returned func is called.
func (b *Bus) ScreenOpen() (closed func()) { return b.ScreenHere(true).Close }

// ScreensOpen is how many presence screens are open on this computer, in
// sight or not.
func (b *Bus) ScreensOpen() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.screens)
}

// ScreensInSight is how many presence screens on this computer the owner
// can see: open, and not a background tab or a minimised window.
func (b *Bus) ScreensInSight() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, v := range b.screens {
		if v {
			n++
		}
	}
	return n
}
