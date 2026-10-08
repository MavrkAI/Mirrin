package events

import "testing"

// A screen here is counted as open whether or not it is in sight, and in
// sight only while its page says so: a background tab shows the owner
// nothing.
func TestScreensInSight(t *testing.T) {
	b := New()
	hidden := b.ScreenHere(false)
	shown := b.ScreenOpen()
	if b.ScreensOpen() != 2 || b.ScreensInSight() != 1 {
		t.Fatalf("open %d, in sight %d", b.ScreensOpen(), b.ScreensInSight())
	}
	if !b.SeenScreen(hidden.ID(), true) || b.ScreensInSight() != 2 {
		t.Fatalf("came into sight: %d", b.ScreensInSight())
	}
	if b.SeenScreen("nobody", true) || b.SeenScreen("", true) {
		t.Fatal("a screen nobody opened was seen")
	}
	shown()
	hidden.Close()
	hidden.Close()
	if b.ScreensOpen() != 0 || b.ScreensInSight() != 0 || b.SeenScreen(hidden.ID(), true) {
		t.Fatalf("after closing: open %d, in sight %d", b.ScreensOpen(), b.ScreensInSight())
	}
	if a, c := b.ScreenHere(true), b.ScreenHere(true); a.ID() == c.ID() || len(a.ID()) < 16 {
		t.Fatalf("ids %q %q", a.ID(), c.ID())
	}
}
