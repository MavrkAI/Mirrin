package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	memskill "github.com/MavrkAI/Mirrin/internal/skills/memory"
)

// "Remember this page": said in a chat it is the remember_page tool, pressed
// on the browser sheet it is BrowserRemember. Both keep the page through
// memskill.SavePage, so a page saved either way is saved once.

// currentPage is the page open in the twin's browser, or none.
func (d *Daemon) currentPage(ctx context.Context) memskill.Page {
	if d.browser == nil {
		return memskill.Page{}
	}
	st := d.browser.Live(ctx)
	if !st.Open {
		return memskill.Page{}
	}
	return memskill.Page{URL: st.URL, Title: st.Title}
}

// BrowserRemember keeps the browser's page in memory for the screen's
// button: the title and address, filed by what the page is, with no note
// (the owner pressed it; nothing read the page).
func (d *Daemon) BrowserRemember(ctx context.Context) (bool, error) {
	return d.rememberPage(ctx, d.currentPage(ctx))
}

// rememberPage keeps p as pressed on this Mac's screen: shown there as
// noted, with Undo (remembered.go).
func (d *Daemon) rememberPage(ctx context.Context, p memskill.Page) (bool, error) {
	_, already, err := memskill.SavePage(ctx, d.store, "", p, "", "screen:local", time.Now())
	if errors.Is(err, memskill.ErrNoPage) {
		return false, api.ErrNoWebPage
	}
	return already, err
}

var _ api.BrowserRememberer = (*Daemon)(nil)
