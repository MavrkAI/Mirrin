package daemon

import (
	"context"
	"errors"

	"github.com/MavrkAI/Mirrin/internal/skills/browser"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// open_screen brings the presence screen up on this computer when the owner
// asks for it ("show me the screen", "I can't see it"). Before it, the twin
// could only insist the page was already on their screen.

// atThisDesk reports whether chatKey is a chat at this computer with no
// screen of its own to look at: by voice or in the terminal, never a call,
// whose caller is someone else.
func atThisDesk(chatKey string) bool {
	if isCall(chatKey) {
		return false
	}
	switch channelOf(homeKey(chatKey)) {
	case "voice", "cli":
		return true
	}
	return false
}

// openScreenTool opens the screen, at the twin's browser while the owner
// has a page there, otherwise at the top. Owner only (answers.go).
func (d *Daemon) openScreenTool() *tools.Func {
	return tools.New("open_screen",
		"Bring the presence screen up in front of the user on this computer when they ask to see it (\"show me the screen\", \"open the screen\", \"I can't see it\"). It opens at a page you handed them, if there is one. Only from a chat at this computer, by voice or in the terminal.",
		tools.Schema(map[string]tools.Prop{}), tools.RiskRead,
		func(ctx context.Context, call tools.Call) (string, error) {
			if !atThisDesk(call.ChatKey) {
				if channelOf(homeKey(call.ChatKey)) == "screen" {
					return "", errors.New("they're on a presence screen already: tell them a page you handed them is under Browser there, and is used on the computer you run on")
				}
				return "", errors.New("the screen opens only from a chat at this computer, by voice or in the terminal; tell them it's on the computer you run on")
			}
			hash, at := "", "the presence screen is open in front of them now"
			if d.browser != nil && d.browser.Live(ctx).Held {
				hash, at = "#browser", "the presence screen is open in front of them now, at the page you handed them"
			}
			if err := d.openScreenAt(hash); err != nil {
				d.log.Warn("open the screen on request", "err", err)
				return "", errors.New("the screen couldn't be opened, so don't say it's on their screen; tell them plainly, and that they can open it themselves (" + browser.OpenItYourself(d.screenAddress(call.ChatKey)) + ")")
			}
			return "Done: " + at + ". Tell them in a few words.", nil
		})
}
