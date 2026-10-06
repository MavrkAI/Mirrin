package tray

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// The menu's decisions, kept apart from the native systray handles so tests
// can make them without a desktop session.

// pendingTitle is the approvals line at the top of the menu.
func pendingTitle(n int) string {
	if n == 0 {
		return "No approvals waiting"
	}
	return fmt.Sprintf("%d approval(s) waiting — open the screen", n)
}

// providerSwitcher is what switching the model provider needs.
type providerSwitcher interface {
	SetProvider(provider string) error
	AccountsURL() string
}

// switchProvider changes the model provider. A provider without its API key
// yet opens the page where the key goes, rather than only saying so.
func switchProvider(b providerSwitcher, provider string, open func(string), tell func(string)) {
	err := b.SetProvider(provider)
	if err == nil {
		return
	}
	var km *llm.KeyMissingError
	if errors.As(err, &km) {
		if u := b.AccountsURL(); u != "" {
			open(u + "#model")
			return
		}
	}
	tell("Could not switch: " + err.Error())
}

// openVoiceSetup opens the voice setup page, or runs `mirrin voice setup` in
// a terminal when there is no page (the local API is off, or this build
// doesn't serve it).
func openVoiceSetup(pages pagesBackend, open func(string), terminal func(...string)) {
	if pages != nil {
		if u := pages.PageURL("/voice/setup"); u != "" && pageServed(u) {
			open(u)
			return
		}
	}
	terminal("voice", "setup")
}

// pageServed asks the twin's local API whether it serves a page, so the
// menu never opens a dead link. A variable so tests stay off the network.
var pageServed = func(u string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Head(u)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound
}

// configEditor reads and changes the twin's settings.
type configEditor interface {
	Config() config.Config
	UpdateConfig(mutate func(c *config.Config)) error
}

// zoneTitle is the time zone entry: following this computer's zone as it
// travels, with the offer to pin it, or pinned, with the offer to follow.
func zoneTitle(setting, local string) string {
	if config.FollowsSystem(setting) {
		if local == "" {
			return "Time zone: follows this computer"
		}
		return "Pin time zone to " + local
	}
	return "Time zone: " + strings.TrimSpace(setting) + " · follow this computer instead"
}

// toggleZone pins the time zone to this computer's zone now, or lets it
// follow the computer again. The twin reads User.Timezone as it changes
// (ZoneSetting), so no restart is needed.
func toggleZone(b configEditor, local string) error {
	if config.FollowsSystem(b.Config().User.Timezone) {
		if local == "" {
			return errors.New("Couldn't tell this computer's time zone. Open the settings file and set it under user, as timezone (for example Europe/London).")
		}
		return b.UpdateConfig(func(c *config.Config) { c.User.Timezone = local })
	}
	return b.UpdateConfig(func(c *config.Config) { c.User.Timezone = "Local" })
}

// timedPauser is a backend that can pause for a while.
type timedPauser interface {
	PauseUntil(until time.Time)
	PausedUntil() time.Time
}

// pauseEnd is when "Pause for 1 hour" or "Pause until tomorrow" ends:
// tomorrow is 8 in the morning (this morning, clicked before 4).
func pauseEnd(kind string, now time.Time) time.Time {
	if kind == "hour" {
		return now.Add(time.Hour)
	}
	y, m, d := now.Date()
	if now.Hour() < 4 {
		d-- // past midnight, "tomorrow" is still the coming morning
	}
	return time.Date(y, m, d+1, 8, 0, 0, 0, now.Location())
}

// pausedState is the status line's word for a pause: until when, for a
// pause for a while.
func pausedState(until, now time.Time) string {
	if until.IsZero() {
		return "paused"
	}
	until = until.In(now.Location())
	if y, m, d := until.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return "paused until " + until.Format("3:04 PM")
	}
	return "paused until " + until.Format("Mon 3:04 PM")
}
