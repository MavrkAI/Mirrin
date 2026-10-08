package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// A long answer said at this computer goes on the presence screen: the
// voice says the gist and "It's all on your screen" (voice spoken.go), and
// the screen shows the whole answer as a card (ui.html #show). It is only
// for a chat at this computer, by voice or in the terminal: never a call,
// whose caller is someone else, nor a messaging chat, whose owner may be
// anywhere. A screen already open here shows the card; otherwise one is
// opened at it. "Just read it to me" has long answers read out instead,
// kept in the kv store under readLongAloudKey.

// readLongAloudKey, set to "1", has long answers read out, not shown.
const readLongAloudKey = "voice.read_long_aloud"

// shownFor is how long the screen still offers an answer put on it.
const shownFor = 10 * time.Minute

// shownAnswer is the answer last put on the screen.
type shownAnswer struct {
	mu  sync.Mutex
	ans api.ShownAnswer
}

// screenAnswers gives the voice channel the screen to put answers on.
func (d *Daemon) screenAnswers() voice.ScreenHooks {
	return voice.ScreenHooks{
		For: func(chatID string) func(string) bool {
			key := "voice:" + chatID
			if !d.mayShowAnswer(key) {
				return nil
			}
			return func(text string) bool { return d.showAnswer(key, text) }
		},
		ReadAloud: d.setReadLongAloud,
	}
}

// mayShowAnswer reports whether a long answer in chatKey may go on the
// screen: a chat at this computer, with the screen there to open, and long
// answers not asked to be read out.
func (d *Daemon) mayShowAnswer(chatKey string) bool {
	if isCall(chatKey) {
		return false
	}
	switch channelOf(homeKey(chatKey)) {
	case "voice", "cli":
	default:
		return false
	}
	return d.UIURL() != "" && !d.readLongAloud()
}

// showAnswer puts text on the screen, opening one here if none is, and
// reports whether it is there to see.
func (d *Daemon) showAnswer(chatKey, text string) bool {
	if !d.mayShowAnswer(chatKey) {
		return false
	}
	now := time.Now()
	d.shown.mu.Lock()
	d.shown.ans = api.ShownAnswer{Text: text, At: now}
	d.shown.mu.Unlock()
	d.bus.Publish(events.Event{Kind: "show", Text: text, At: now})
	if d.bus.ScreensOpen() > 0 {
		return true
	}
	if err := d.openScreenAt("#show"); err != nil {
		d.log.Warn("show: open the screen for a long answer", "err", err)
		return false
	}
	return true
}

// ShownAnswer is the answer last put on the screen, while recent (api show.go).
func (d *Daemon) ShownAnswer(context.Context) (api.ShownAnswer, bool) {
	d.shown.mu.Lock()
	defer d.shown.mu.Unlock()
	a := d.shown.ans
	if a.Text == "" || time.Since(a.At) > shownFor {
		return api.ShownAnswer{}, false
	}
	return a, true
}

// readLongAloud reports whether the owner asked for long answers read out.
func (d *Daemon) readLongAloud() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, err := d.store.Get(ctx, readLongAloudKey)
	return err == nil && v == "1"
}

// onScreenOnly is a turn's line's data (from) for a voice reply that may
// have gone on the screen rather than been said: marked "shown", so a
// screen paired only to look, which shows what was said in the room, never
// shows a draft that was only on the owner's screen.
func onScreenOnly(kind, text string, from map[string]string) map[string]string {
	if kind != "said" || from["channel"] != "voice" || !voice.ForScreen(text) {
		return from
	}
	return map[string]string{"channel": from["channel"], "shown": "screen"}
}

// setReadLongAloud has long answers read out (on) or shown (off).
func (d *Daemon) setReadLongAloud(on bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v := ""
	if on {
		v = "1"
	}
	if err := d.store.Set(ctx, readLongAloudKey, v); err != nil {
		d.log.Warn("show: remember how long answers are given", "err", err)
	}
}
