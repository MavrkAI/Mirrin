package daemon

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"runtime"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/skills/browser"
)

// The live view of the twin's browser, for the presence screen (api
// browser_live.go, browser live.go).

var errNoBrowser = errors.New("the browser skill is off")

// BrowserState reports whether the twin's browser has a page open.
func (d *Daemon) BrowserState(ctx context.Context) api.BrowserState {
	if d.browser == nil {
		return api.BrowserState{}
	}
	return screenBrowserState(d.browser.Live(ctx))
}

// screenBrowserState is what the screen is told of the browser: the page,
// who drives it, a hand-over with what it asks, and whether the twin is
// using it now.
func screenBrowserState(st browser.LiveState) api.BrowserState {
	return api.BrowserState{Open: st.Open, URL: st.URL, Title: st.Title, Held: st.Held, Handover: st.Handover, Ask: st.Ask, Active: st.Active}
}

// browserActive tells the screen the twin started using its browser after a
// while idle, so it looks at once (and may open to watch). No data: where it
// went is the screen's to ask (/browser/state), not the feed's to say.
func (d *Daemon) browserActive() {
	d.bus.Publish(events.Event{Kind: "browser", Text: "active"})
}

// BrowserWatch streams the page while ctx lasts.
func (d *Daemon) BrowserWatch(ctx context.Context) (<-chan api.BrowserFrame, error) {
	if d.browser == nil {
		return nil, errNoBrowser
	}
	in, err := d.browser.Watch(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan api.BrowserFrame, 2)
	go func() {
		defer close(out)
		for f := range in {
			select {
			case out <- api.BrowserFrame{Data: f.Data, W: f.W, H: f.H}:
			case <-ctx.Done(): // the viewer left; don't hold a frame for nobody
				return
			}
		}
	}()
	return out, nil
}

// BrowserInput plays the owner's click, key or text on the page.
func (d *Daemon) BrowserInput(ctx context.Context, in api.BrowserInput) error {
	if d.browser == nil {
		return errNoBrowser
	}
	return d.browser.Input(ctx, browser.InputEvent{Type: in.Type, X: in.X, Y: in.Y, DX: in.DX, DY: in.DY, Key: in.Key, Text: in.Text})
}

// BrowserTakeOver gives the browser to the owner, or back to the twin.
func (d *Daemon) BrowserTakeOver(_ context.Context, on bool) error {
	if d.browser == nil {
		return errNoBrowser
	}
	if on {
		d.browser.TakeOver(true)
		return nil
	}
	if key := d.browser.HandBack(); key != "" {
		go d.carryOnAfterHandBack(key)
	}
	return nil
}

// handBackNote is what the twin hears when the owner hands the browser back
// from the screen: framed as the screen's, and never as "not from the user",
// which the twin rightly reads as an instruction not to follow.
const handBackNote = "[Screen event: the owner clicked Hand back on the presence screen, so the browser is yours again. Carry on with what you were doing in it, and tell them briefly how it's going.]"

// carryOnAfterHandBack takes up the conversation that was using the browser
// once the owner hands it back, so a voice chat that has since gone quiet
// carries on without them having to say "done".
func (d *Daemon) carryOnAfterHandBack(key string) {
	name, chatID := channels.SplitKey(key)
	if name == "" {
		return
	}
	ctx := d.base(context.Background())
	in := channels.Inbound{Channel: name, ChatID: chatID, Text: handBackNote, IsOwner: true}
	p := d.bus.Begin("thinking")
	defer p.End()
	reply, err := d.message(ctx, in, agent.Events{OnTool: func(_, caption string) {
		if caption != "" {
			d.bus.Publish(events.Event{Kind: "note", Text: caption})
		}
	}})
	p.End() // settled before the reply goes out
	if err != nil {
		d.log.Warn("browser: carrying on after the hand back", "chat", key, "err", err)
		return
	}
	if reply == "" {
		return
	}
	d.bus.Publish(events.Event{Kind: "said", Text: reply})
	if err := d.Send(ctx, key, reply); err != nil {
		d.log.Warn("browser: send after the hand back", "chat", key, "err", err)
	}
}

// browserHandedOver tells the screen and the orb that the twin handed a page
// to the owner: the orb shows, and the screen's browser panel leads. Their
// phone hears of it too (push.go), and it says whether one did.
func (d *Daemon) browserHandedOver(url, ask string) bool {
	d.bus.Publish(events.Event{Kind: "browser", Text: "handover", Data: map[string]string{"url": url, "ask": ask}})
	return d.pushHandOver()
}

// screenAddress is the presence screen's address on this computer, without
// its key: said in chat, it must not carry a credential. Only a chat at this
// computer gets it; from a messaging chat or a call the owner may be anywhere,
// and 127.0.0.1 would be a dead link on their phone.
func (d *Daemon) screenAddress(chatKey string) string {
	if isCall(chatKey) {
		return ""
	}
	switch channelOf(homeKey(chatKey)) {
	case "", "screen", "voice", "cli", "api":
	default:
		return ""
	}
	_, port, err := net.SplitHostPort(d.Config().API.Listen)
	if err != nil || port == "" {
		return ""
	}
	return "http://127.0.0.1:" + port + "/ui"
}

// OpenScreen opens the presence screen in this computer's own browser, at
// the twin's browser when a page is waiting there (a click on the orb).
func (d *Daemon) OpenScreen(context.Context) error {
	u := d.UIURL()
	if u == "" {
		return errors.New("the screen isn't available")
	}
	if !strings.Contains(u, "#") {
		u += "#browser"
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return openScreenCmd(cmd)
}

// openScreenCmd starts the opener; replaced in tests.
var openScreenCmd = func(c *exec.Cmd) error { return c.Start() }
