package browser

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// The live view: the twin's browser, streamed to the presence screen while
// someone watches, and taken over by the owner (clicks, typing, scrolling)
// while the twin waits.

// Frame is one picture of the page: a JPEG, base64 as Chrome sends it, and
// the size of the page it shows, in CSS pixels, for mapping clicks back.
type Frame struct {
	Data string  `json:"d"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
}

// LiveState says whether there is a page to watch, and who is driving.
type LiveState struct {
	Open  bool   `json:"open"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
	Held  bool   `json:"held"` // the owner has taken over
	// Handover: the twin handed this page to the owner, and Ask is what it
	// needs them to do there.
	Handover bool   `json:"handover,omitempty"`
	Ask      string `json:"ask,omitempty"`
	// Active: the twin is using its browser now (a tool call under way, or
	// one within activeFor) and the owner hasn't taken it over. The screen
	// offers to watch, or opens to watch, when it turns on.
	Active bool `json:"active"`
}

// errNoPage is Watch and Input with no browser open.
var errNoPage = errors.New("the browser isn't open")

// errNotHeld is Input while the twin, not the owner, is driving.
var errNotHeld = errors.New("take over the browser first")

// liveHub shares one screencast among every watcher of the current tab.
type liveHub struct {
	mu    sync.Mutex
	subs  map[chan Frame]struct{}
	tab   context.Context // the tab being cast, or nil
	gen   int             // bumped on each start, so a stopped cast's frames are dropped
	last  Frame           // the latest frame, for a watcher who joins late
	held  bool
	freed chan struct{} // closed when the owner hands the browser back
	// soft: the twin handed the page over (browser_signin). The owner's
	// "done" in chat releases it: the twin's next browser tool takes it
	// back. ask is what the twin asked them to do there.
	soft bool
	ask  string
	// pressed: the owner's mouse button is down on the page, so moves are
	// drags.
	pressed bool
	// chat is the conversation that last used the browser, and when: the
	// one to carry on in when the owner hands the browser back.
	chat   string
	chatAt time.Time
	// lastURL and lastTitle are where the tab was when last asked, for
	// while it's too busy to say.
	lastURL, lastTitle string
	// inFlight counts the twin's browser tool calls under way, and twinAt is
	// when it last started or finished one: together, whether it is using
	// the browser now (activeAt). The owner's own input never counts.
	inFlight int
	twinAt   time.Time
	// now is the clock (nil: time.Now); tests set it.
	now func() time.Time
}

func (h *liveHub) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// activeFor is how long the twin counts as using its browser after its last
// browser tool call: the pause between one step and the next.
const activeFor = 40 * time.Second

// use marks one browser tool call by the twin ("" key: a task or routine,
// which leaves the chat to carry on in as it was). It returns done, to call
// when the call ends, and whether the browser had been idle (a new run).
func (h *liveHub) use(key string) (done func(), fresh bool) {
	h.mu.Lock()
	now := h.clock()
	fresh = h.inFlight == 0 && now.Sub(h.twinAt) >= activeFor
	h.inFlight++
	h.twinAt = now
	if key != "" {
		h.chat, h.chatAt = key, now
	}
	h.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			h.inFlight--
			h.twinAt = h.clock()
			h.mu.Unlock()
		})
	}, fresh
}

// activeAt says whether the twin is using the browser at t, never while the
// owner has it. The caller holds h.mu.
func (h *liveHub) activeAt(t time.Time) bool {
	return !h.held && (h.inFlight > 0 || t.Sub(h.twinAt) < activeFor)
}

// handBackTo is how recently a chat must have used the browser for a hand
// back to carry on in it.
const handBackTo = 30 * time.Minute

func (h *liveHub) isHeld() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.held }

func (h *liveHub) setPressed(on bool) { h.mu.Lock(); h.pressed = on; h.mu.Unlock() }

// button is the button a move carries: the left one while pressed.
func (h *liveHub) button() input.MouseButton {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pressed {
		return input.Left
	}
	return input.None
}

// Live reports the browser's state for the screen.
func (s *Session) Live(ctx context.Context) LiveState {
	s.mu.Lock()
	tab := s.ctx
	s.mu.Unlock()
	s.live.mu.Lock()
	held, soft, ask := s.live.held, s.live.soft, s.live.ask
	active := s.live.activeAt(s.live.clock())
	s.live.mu.Unlock()
	if tab == nil || tab.Err() != nil {
		// Chrome still starting for a run: active, with no page yet.
		return LiveState{Held: held, Active: active}
	}
	var url, title string
	tctx, cancel := context.WithTimeout(tab, 2*time.Second)
	defer cancel()
	if err := chromedp.Run(tctx, chromedp.Location(&url), chromedp.Title(&title)); err != nil {
		// Busy (a page loading), not gone: the tab is still there, so the
		// screen keeps showing it, with where it was last.
		s.live.mu.Lock()
		url, title = s.live.lastURL, s.live.lastTitle
		s.live.mu.Unlock()
		return LiveState{Open: true, URL: url, Title: title, Held: held, Handover: held && soft, Ask: ask, Active: active}
	}
	s.live.mu.Lock()
	s.live.lastURL, s.live.lastTitle = url, title
	s.live.mu.Unlock()
	return LiveState{Open: true, URL: url, Title: title, Held: held, Handover: held && soft, Ask: ask, Active: active}
}

// Watch streams the page until ctx ends or the tab closes. Watchers share
// one screencast, started for the first and stopped after the last.
func (s *Session) Watch(ctx context.Context) (<-chan Frame, error) {
	s.mu.Lock()
	tab := s.ctx
	s.mu.Unlock()
	if tab == nil || tab.Err() != nil {
		return nil, errNoPage
	}
	h := &s.live
	ch := make(chan Frame, 2)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan Frame]struct{}{}
	}
	h.subs[ch] = struct{}{}
	first := h.tab != tab
	if first {
		h.tab, h.gen, h.last = tab, h.gen+1, Frame{}
	}
	gen, last := h.gen, h.last
	h.mu.Unlock()
	if last.Data != "" {
		ch <- last
	}
	if first {
		if err := s.startCast(tab, gen); err != nil {
			s.unwatch(ch)
			return nil, err
		}
	}
	out := make(chan Frame, 2)
	go func() {
		defer close(out)
		defer s.unwatch(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tab.Done():
				return
			case f := <-ch:
				select {
				case out <- f:
				default: // a slow watcher skips frames rather than holding the cast up
				}
			}
		}
	}()
	return out, nil
}

// startCast begins Chrome's screencast on tab and fans its frames out.
func (s *Session) startCast(tab context.Context, gen int) error {
	chromedp.ListenTarget(tab, func(ev any) {
		e, ok := ev.(*page.EventScreencastFrame)
		if !ok {
			return
		}
		go func() { // acknowledge off the event loop, or Chrome stops sending
			c := chromedp.FromContext(tab)
			if c == nil || c.Target == nil {
				return
			}
			_ = page.ScreencastFrameAck(e.SessionID).Do(cdp.WithExecutor(tab, c.Target))
		}()
		f := Frame{Data: e.Data}
		if e.Metadata != nil {
			f.W, f.H = e.Metadata.DeviceWidth, e.Metadata.DeviceHeight
		}
		h := &s.live
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.gen != gen || h.tab != tab {
			return // from a cast that has since stopped
		}
		h.last = f
		for sub := range h.subs {
			select {
			case sub <- f:
			default:
			}
		}
	})
	ctx, cancel := context.WithTimeout(tab, 5*time.Second)
	defer cancel()
	return chromedp.Run(ctx, page.StartScreencast().WithFormat(page.ScreencastFormatJpeg).WithQuality(60).WithMaxWidth(1280).WithMaxHeight(900))
}

// unwatch drops a watcher and stops the cast after the last one.
func (s *Session) unwatch(ch chan Frame) {
	h := &s.live
	h.mu.Lock()
	delete(h.subs, ch)
	tab := h.tab
	stop := len(h.subs) == 0 && tab != nil
	if stop {
		h.tab, h.gen = nil, h.gen+1
	}
	h.mu.Unlock()
	if stop && tab.Err() == nil {
		ctx, cancel := context.WithTimeout(tab, 3*time.Second)
		defer cancel()
		_ = chromedp.Run(ctx, page.StopScreencast())
	}
}

// TakeOver hands the browser to the owner (on) or back to the twin (off).
// While the owner has it, the twin's browser tools wait.
func (s *Session) TakeOver(on bool) { s.live.take(on) }

// HandBack gives the browser back to the twin from the screen, and says
// which conversation to carry on in: the one that last used the browser,
// if lately ("" for none, or when the owner didn't have it).
func (s *Session) HandBack() string {
	h := &s.live
	h.mu.Lock()
	held, chat, at := h.held, h.chat, h.chatAt
	h.mu.Unlock()
	h.take(false)
	if !held || chat == "" || time.Since(at) > handBackTo {
		return ""
	}
	return chat
}

func (h *liveHub) take(on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if on == h.held {
		return
	}
	h.held, h.soft, h.ask, h.pressed = on, false, "", false
	if on {
		h.freed = make(chan struct{})
	} else if h.freed != nil {
		close(h.freed)
		h.freed = nil
	}
}

// handTo gives the page to the owner because the twin asked for help. It is
// released by the twin's next browser action (after the owner says done),
// or by Hand back.
func (s *Session) handTo(ask string) {
	h := &s.live
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.held {
		h.freed = make(chan struct{})
	}
	h.held, h.soft, h.ask = true, true, ask
}

// waitHandBack blocks the twin while the owner is driving, up to limit. A
// page the twin handed over comes back as soon as the twin acts again: it
// only does once the owner has answered.
func (s *Session) waitHandBack(limit time.Duration) error {
	h := &s.live
	h.mu.Lock()
	freed := h.freed
	held, soft := h.held, h.soft
	if held && soft {
		h.held, h.soft, h.ask = false, false, ""
		if h.freed != nil {
			close(h.freed)
			h.freed = nil
		}
	}
	h.mu.Unlock()
	if !held || soft {
		return nil
	}
	select {
	case <-freed:
		return nil
	case <-time.After(limit):
		return errOwnerDriving
	}
}

// errOwnerDriving is what the twin's browser tools say while the owner has
// taken over and hasn't handed back.
var errOwnerDriving = errors.New("the owner is using the browser right now; once they say they're finished, take it back with browser_take_back")

// handBackWait is how long a browser tool waits for the owner to hand back.
var handBackWait = 2 * time.Minute

// InputEvent is something the owner did on the live view, in page pixels.
type InputEvent struct {
	Type string  `json:"type"` // click, down, up, move, wheel, key, text
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	DX   float64 `json:"dx,omitempty"`
	DY   float64 `json:"dy,omitempty"`
	Key  string  `json:"key,omitempty"`  // a named key (Enter, Backspace, ArrowDown…)
	Text string  `json:"text,omitempty"` // typed characters
}

// namedKeys are the keys the live view passes on by name.
var namedKeys = map[string]string{
	"Enter": kb.Enter, "Backspace": kb.Backspace, "Tab": kb.Tab, "Escape": kb.Escape, "Delete": kb.Delete,
	"ArrowUp": kb.ArrowUp, "ArrowDown": kb.ArrowDown, "ArrowLeft": kb.ArrowLeft, "ArrowRight": kb.ArrowRight,
	"Home": kb.Home, "End": kb.End, "PageUp": kb.PageUp, "PageDown": kb.PageDown,
}

// Input plays one of the owner's actions on the page. It works only while
// the owner has taken over, so the twin and the owner never both drive.
func (s *Session) Input(ctx context.Context, ev InputEvent) error {
	s.live.mu.Lock()
	held := s.live.held
	s.live.mu.Unlock()
	if !held {
		return errNotHeld
	}
	s.mu.Lock()
	tab := s.ctx
	s.mu.Unlock()
	if tab == nil || tab.Err() != nil {
		return errNoPage
	}
	s.mu.Lock()
	s.lastUse = time.Now()
	s.mu.Unlock()
	rctx, cancel := context.WithTimeout(tab, 5*time.Second)
	defer cancel()
	var act chromedp.Action
	switch ev.Type {
	case "click":
		act = chromedp.ActionFunc(func(ctx context.Context) error {
			if err := input.DispatchMouseEvent(input.MouseMoved, ev.X, ev.Y).Do(ctx); err != nil {
				return err
			}
			if err := input.DispatchMouseEvent(input.MousePressed, ev.X, ev.Y).WithButton(input.Left).WithClickCount(1).Do(ctx); err != nil {
				return err
			}
			return input.DispatchMouseEvent(input.MouseReleased, ev.X, ev.Y).WithButton(input.Left).WithClickCount(1).Do(ctx)
		})
	case "down", "up":
		// Pressed and released apart, so a press can be held ("press and
		// hold" checks) or dragged.
		kind := input.MousePressed
		if ev.Type == "up" {
			kind = input.MouseReleased
		}
		act = chromedp.ActionFunc(func(ctx context.Context) error {
			if err := input.DispatchMouseEvent(input.MouseMoved, ev.X, ev.Y).WithButton(s.live.button()).Do(ctx); err != nil {
				return err
			}
			if err := input.DispatchMouseEvent(kind, ev.X, ev.Y).WithButton(input.Left).WithClickCount(1).Do(ctx); err != nil {
				return err
			}
			s.live.setPressed(ev.Type == "down")
			return nil
		})
	case "move":
		act = input.DispatchMouseEvent(input.MouseMoved, ev.X, ev.Y).WithButton(s.live.button())
	case "wheel":
		act = input.DispatchMouseEvent(input.MouseWheel, ev.X, ev.Y).WithDeltaX(ev.DX).WithDeltaY(ev.DY)
	case "key":
		k, ok := namedKeys[ev.Key]
		if !ok {
			return errors.New("unknown key")
		}
		act = chromedp.KeyEvent(k)
	case "text":
		if ev.Text == "" {
			return nil
		}
		act = input.InsertText(ev.Text)
	default:
		return errors.New("unknown input")
	}
	return chromedp.Run(rctx, act)
}

// Brief says, in a line for the twin's prompt, what the browser has open and
// who is driving it, or "" when no page is open.
func (s *Session) Brief(ctx context.Context) string {
	st := s.Live(ctx)
	if !st.Open || st.URL == "about:blank" {
		return ""
	}
	if st.URL == "" {
		return "open, busy loading a page. Read it with browse_page and no url once it has loaded."
	}
	line := "open at " + st.URL
	if st.Title != "" {
		line += " (\"" + st.Title + "\")"
	}
	line += "."
	switch {
	case st.Handover:
		line += " Handed to the user on the presence screen"
		if st.Ask != "" {
			line += " to: " + st.Ask
		}
		line += "; when they say done, your next browser tool takes it back."
	case st.Held:
		line += " The user has taken it over; browser tools wait until they hand it back."
	}
	return line
}
