package channels

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Messages a transport hands to its handler go into the daemon's
// per-conversation mailbox (internal/daemon/conversations.go), so a long turn
// never stalls the receive loop. What a transport does while a reply is being
// worked on is below: the typing indicator.

// Typer is a Channel that can show "typing…" in a chat while a reply is being
// worked on. Typing returns how soon to repeat it: every platform lets the
// indicator fade after a few seconds unless it is refreshed.
type Typer interface {
	Typing(ctx context.Context, chatID string) (again time.Duration, err error)
}

// TypingEnder is a Typer whose indicator lingers for a while after the reply
// unless it is cleared (Matrix, Zulip).
type TypingEnder interface {
	EndTyping(ctx context.Context, chatID string) error
}

// typingKey names one indicator by channel and chat, so the instance that
// sends the reply can stop the one that started it (a channel may have been
// rebuilt in between) and any Typer, pointer or not, can be used.
type typingKey struct{ channel, chat string }

func keyOf(t Typer, chatID string) typingKey {
	if n, ok := t.(interface{ Name() string }); ok {
		return typingKey{n.Name(), chatID}
	}
	return typingKey{fmt.Sprintf("%T", t), chatID}
}

type typingRun struct {
	cancel context.CancelFunc
	t      Typer
}

var (
	typingMu  sync.Mutex
	typingNow = map[typingKey]*typingRun{}
)

// KeepTyping shows the typing indicator in chatID straight away and keeps it
// up until stop is called, ctx ends, or the reply is sent (StopTyping). It
// gives up quietly if the platform refuses: the indicator is a courtesy.
func KeepTyping(ctx context.Context, t Typer, chatID string) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	k, run := keyOf(t, chatID), &typingRun{cancel: cancel, t: t}
	typingMu.Lock()
	if prev := typingNow[k]; prev != nil {
		prev.cancel()
	}
	typingNow[k] = run
	typingMu.Unlock()
	go func() {
		for {
			again, err := t.Typing(ctx, chatID)
			if err != nil || again <= 0 {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(again):
			}
		}
	}()
	return func() {
		cancel()
		typingMu.Lock()
		mine := typingNow[k] == run
		if mine {
			delete(typingNow, k)
		}
		typingMu.Unlock()
		if mine {
			run.end(chatID)
		}
	}
}

// StopTyping ends the indicator in chatID, if one is running. Call it just
// before sending the reply, so a late refresh can't show "typing…" again
// after the answer has arrived.
func StopTyping(t Typer, chatID string) {
	k := keyOf(t, chatID)
	typingMu.Lock()
	run := typingNow[k]
	delete(typingNow, k)
	typingMu.Unlock()
	if run != nil {
		run.cancel()
		run.end(chatID)
	}
}

// end clears a lingering indicator in the background.
func (r *typingRun) end(chatID string) {
	e, ok := r.t.(TypingEnder)
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.EndTyping(ctx, chatID)
	}()
}
