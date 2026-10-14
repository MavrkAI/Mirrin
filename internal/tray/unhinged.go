package tray

import (
	"context"
	"time"

	"fyne.io/systray"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// unhingedItem is the "Unhinged mode" tick under Persona. It saves the
// switch as a persona change would, and follows a switch made in chat
// ("go unhinged"), so the tick always says what the twin is doing.
func unhingedItem(ctx context.Context, parent *systray.MenuItem, backend Backend, name string) {
	item := parent.AddSubMenuItemCheckbox("Unhinged mode", "The same character, turned all the way up. Money, privacy and asking first stay as they are", backend.Config().Unhinged)
	show := func() {
		if backend.Config().Unhinged {
			item.Check()
		} else {
			item.Uncheck()
		}
	}
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-item.ClickedCh:
				on := !item.Checked()
				if err := backend.UpdateConfig(func(c *config.Config) { c.Unhinged = on }); err != nil {
					notify(name, "Could not save: "+err.Error())
				}
				show()
			case <-tick.C:
				show()
			}
		}
	}()
}
