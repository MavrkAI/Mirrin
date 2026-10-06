package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tasks"
)

// The character reacts, once, to something that really happened: a task
// done or an approved action carried out (pleased), a task or a reply that
// failed (sorry), a new request waiting for the owner (attentive). The
// screens play it only where someone can see it (ui.html). Each reaction
// plays at most once every 20 seconds, so a run of results reads as one
// nod, and someone else's chat never moves the owner's character.

// reactEvery is how often the same reaction may play.
const reactEvery = 20 * time.Second

// reactions remembers when each daemon last played each reaction.
var reactions sync.Map // *Daemon → *reactLog

type reactLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// react tells the screens to play kind (pleased, sorry or attentive); why
// says what happened, in a few words of the twin's own and never the
// owner's, since a wall screen that only looks hears it too.
func (d *Daemon) react(kind, why string) {
	v, _ := reactions.LoadOrStore(d, &reactLog{last: map[string]time.Time{}})
	l := v.(*reactLog)
	now := clock()
	l.mu.Lock()
	if at, ok := l.last[kind]; ok && now.Sub(at) < reactEvery && now.Sub(at) >= 0 {
		l.mu.Unlock()
		return
	}
	l.last[kind] = now
	l.mu.Unlock()
	// Only the screens open now: a reaction counts as it happens, and one
	// opened later mustn't play it.
	d.bus.Flash(events.Event{Kind: "react", Text: kind, Data: events.Reaction{Why: why}})
}

// taskOutcome hears how a background task ended (tasks.Deps.OnOutcome):
// done is pleased, failed is sorry. Only a task asked for in the owner's
// own chat moves the character.
func (d *Daemon) taskOutcome(t tasks.Task, status tasks.Status) {
	if !d.ownersOwnChat(t.Owner) {
		return
	}
	switch status {
	case tasks.Done:
		d.react("pleased", "a task finished")
	case tasks.Failed:
		d.react("sorry", "a task stopped after a problem")
	}
}

// turnFailed is sorry when the owner's own turn ended in an error they are
// told about. A turn cut short because the page or the twin went away
// isn't a failure anyone sees.
func (d *Daemon) turnFailed(in channels.Inbound, err error) {
	if err == nil || !in.IsOwner || errors.Is(err, context.Canceled) {
		return
	}
	d.react("sorry", "a reply failed")
}

// performed hears that an approved call ran (agent.Performed). One that
// worked is pleased; one that failed says so in the reply, and a request
// someone else asked for doesn't move the owner's character.
func (d *Daemon) performed(ctx context.Context, ap memory.Approval, ok bool) {
	if ok && !d.forSomeoneElse(ctx, ap.ID) {
		d.react("pleased", "an approved step was carried out")
	}
}

// raisedFor is attentive when a request the owner asked for starts waiting
// on them: the character glances towards Needs.
func (d *Daemon) raisedFor(ctx context.Context, ap memory.Approval) {
	if !d.forSomeoneElse(ctx, ap.ID) {
		d.react("attentive", "something needs you")
	}
}
