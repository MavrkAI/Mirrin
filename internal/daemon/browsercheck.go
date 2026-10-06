package daemon

import (
	"context"
	"encoding/json"

	"github.com/MavrkAI/Mirrin/internal/memory"
)

// elementCheck is the browser's own check of an approved action
// (browser.Session.CheckApproved).
type elementCheck func(ctx context.Context, chatKey string, input json.RawMessage) error

// checkElements runs after the address check for an approved browser action:
// the browser compares each element it aims at with what it was when the
// owner was asked, so a button that now says something else is never
// pressed, and the approval ends unrun.
func (d *Daemon) checkElements(ctx context.Context, ap memory.Approval) error {
	if d.pageCheck == nil {
		return nil
	}
	return d.pageCheck(ctx, ap.ChatKey, ap.Input)
}
