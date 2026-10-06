package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/memory"
	memskill "github.com/MavrkAI/Mirrin/internal/skills/memory"
)

// When the twin keeps something the owner told it in a chat on this Mac,
// the screens show it then and there, under the reply: "Noted: Akshay
// doesn't eat meat · Undo". Nothing private is shown (what remember_sensitive
// keeps), nor anything learned in a messaging app, a routine or a task, and a
// wall screen that only looks never hears of it (api's screen_private.go
// passes no "remembered" event).

// notedChats are this Mac's own chats: the screen's, the microphone's and the
// terminal's.
var notedChats = map[string]bool{"screen:local": true, "voice:local": true, "cli:terminal": true}

// undoChats are the chats whose facts a screen's Undo may take back: the
// ones a screen shows as they happen.
var undoChats = map[string]bool{"screen:local": true, "voice:local": true}

// undoFor is how long a fact can be undone from a screen. After that it is
// forgotten by asking, or on the memory page.
const undoFor = 10 * time.Minute

// watchRemembered has the screens told about what the twin keeps.
func (d *Daemon) watchRemembered() {
	d.store.OnRemembered(d.remembered)
}

// remembered tells the screens about a fact the twin just kept, when it is
// one they may show.
func (d *Daemon) remembered(f memory.Fact) {
	if memskill.Sensitive(f.Subject) || !notedChats[f.Source] {
		return
	}
	// Only the screens open now: none opened later, nor /screen's recent
	// lines, keeps the fact, so an Undo or a forget leaves no copy there.
	d.bus.Flash(events.Event{Kind: "remembered", Text: f.Content, Data: map[string]any{"id": f.ID, "subject": f.Subject}})
}

// UndoFact forgets a fact a screen showed as just noted (api.FactUndoer):
// one that isn't private, was learned in a chat a screen shows, and is less
// than ten minutes old. It forgets it everywhere, as "forget that" would, so
// the backups and the portrait follow (OnForgot). Anything else is
// api.ErrNotUndoable.
func (d *Daemon) UndoFact(ctx context.Context, id int64) error {
	f, err := d.store.FactByID(ctx, id)
	if errors.Is(err, memory.ErrNoFact) {
		return api.ErrNotUndoable
	}
	if err != nil {
		return err
	}
	if memskill.Sensitive(f.Subject) || !undoChats[f.Source] || f.CreatedAt.IsZero() || clock().Sub(f.CreatedAt) >= undoFor {
		return api.ErrNotUndoable
	}
	if _, err := d.store.ForgetFact(ctx, id); err != nil {
		if errors.Is(err, memory.ErrNoFact) {
			return api.ErrNotUndoable
		}
		return err
	}
	return nil
}

var _ api.FactUndoer = (*Daemon)(nil)
