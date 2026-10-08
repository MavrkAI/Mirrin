package browser

import "runtime"

// ScreenShow is how bringing the presence screen up for a hand-over went.
type ScreenShow int

const (
	// ScreenNotTried: the chat isn't one at this computer's desk (a
	// messaging chat, a call, or the screen the owner is typing on).
	ScreenNotTried ScreenShow = iota
	// ScreenInSight: a screen was opened here, or one here is in sight.
	ScreenInSight
	// ScreenNotShown: the owner is at this computer, but no screen could
	// be put in front of them.
	ScreenNotShown
)

// notShown is the twin's word when the screen couldn't be brought up: it
// must not tell the owner "it's on your screen", which left them looking
// for a page that wasn't there. Nor send them to ask for it again, which
// would fail the same way, or read an address out loud by voice.
func notShown(here, screen string) string {
	return "The page " + here + " is handed to the user, but the presence screen could not be put in front of them, so don't say it's on their screen. Tell them plainly in one short message that you couldn't bring the screen up, that they can open it themselves (" + OpenItYourself(screen) + "), what to do there, and to say when it's done."
}

// OpenItYourself is how the owner opens the presence screen at this
// computer when the twin can't: from the menu, or by its address, which is
// not one to read out loud.
func OpenItYourself(screen string) string {
	menu := "Open screen… in the tray menu"
	if runtime.GOOS == "darwin" {
		menu = "Open screen… in the menu bar"
	}
	if screen == "" {
		return menu
	}
	return menu + ", or " + screen + " in their browser; by voice, don't read the address out"
}
