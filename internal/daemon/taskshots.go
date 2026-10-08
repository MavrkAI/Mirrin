package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// A background task that browsed used to finish with only its words: the
// chat it reported to never learned there was a screenshot, and the twin
// there said, asked to send it, that the task hadn't used the browser.
// Now a finished task keeps its last screenshots (tasks/shots.go, found
// here by taskShots), and send_to_owner can pass one on to the owner's
// own chat on another app, say WhatsApp from a voice turn.

// reShotLine is a screenshot a tool result showed: the path the agent left
// in place of a picture it lifted ("screenshot: /…"), or a marker it didn't
// lift ("[[image:/…]]").
var reShotLine = regexp.MustCompile(`(?m)(?:^screenshot: (.+?)\s*$|\[\[image:([^\]]+)\]\])`)

// taskShots lists the twin's own screenshots that tool results in the
// conversation key showed and that are still on disk, oldest first, each
// once. Only tool results count, and each path is held to
// agent.OwnScreenshot: a page or a message naming some other file names
// nothing that will be sent.
func (d *Daemon) taskShots(ctx context.Context, key string) []string {
	hist, err := d.store.History(ctx, key, 200)
	if err != nil {
		return nil
	}
	dataDir := d.Config().DataDir
	var out []string
	for _, m := range hist {
		for _, b := range m.Blocks {
			if b.Type != llm.BlockToolResult {
				continue
			}
			for _, sm := range reShotLine.FindAllStringSubmatch(b.Text, -1) {
				p, ok := ownPicture(strings.TrimSpace(sm[1]+sm[2]), dataDir)
				if !ok {
					continue
				}
				out = slices.DeleteFunc(out, func(s string) bool { return s == p })
				out = append(out, p) // seen again: it moves to the end
			}
		}
	}
	return out
}

// ownPicture is agent.OwnScreenshot, and the file really is a PNG or JPEG:
// a file that only borrows a screenshot's name is never kept or sent.
func ownPicture(path, dataDir string) (string, bool) {
	p, ok := agent.OwnScreenshot(path, dataDir)
	if !ok {
		return "", false
	}
	f, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	switch http.DetectContentType(head[:n]) {
	case "image/png", "image/jpeg":
		return p, true
	}
	return "", false
}

// shotsNote is a finished task's screenshots for the task state every turn
// sees, "" for none.
func shotsNote(shots []string) string {
	if len(shots) == 0 {
		return ""
	}
	return " (screenshot: " + strings.Join(shots, ", ") + ")"
}

// sendToOwnerTool sends the owner a message, and perhaps one of the twin's
// own screenshots, in their own chat on one of their messaging apps. It
// reaches no one else: the chat is the channel's owner chat, and the
// picture must be a screenshot the twin took itself. Like a reminder, a
// message to the owner alone needs no approval; only the owner's own turn
// may use it (ownersOnly, answers.go).
func (d *Daemon) sendToOwnerTool() *tools.Func {
	return tools.New("send_to_owner",
		"Send the owner a message in their own chat on one of their messaging apps (say WhatsApp, when they ask by voice), with one of your own screenshots if they want it: a path shown as \"screenshot: …\" in a tool result, a finished task's result or list_tasks. It only ever reaches the owner. Don't put the path in the text.",
		tools.Schema(map[string]tools.Prop{
			"text":       {Type: "string", Description: "What to say, a line or two", Required: true},
			"channel":    {Type: "string", Description: "The app, e.g. whatsapp or telegram; leave empty for the first one that's connected"},
			"screenshot": {Type: "string", Description: "The full path of one of your own screenshots to send with it"},
		}), tools.RiskRead, d.sendToOwnerRun)
}

func (d *Daemon) sendToOwnerRun(ctx context.Context, call tools.Call) (string, error) {
	var in struct {
		Text       string `json:"text"`
		Channel    string `json:"channel"`
		Screenshot string `json:"screenshot"`
	}
	if err := tools.Decode(call, &in); err != nil {
		return "", err
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return "", errors.New("say what goes with it in text")
	}
	cfg := d.Config()
	shot := ""
	if p := strings.TrimSpace(in.Screenshot); p != "" {
		var ok bool
		if shot, ok = ownPicture(p, cfg.DataDir); !ok {
			return "", errors.New("that isn't one of my own screenshots (or it has been cleared), so I can't send it; list_tasks shows a finished task's screenshots")
		}
	}
	name, ch, err := d.ownerChannel(strings.ToLower(strings.TrimSpace(in.Channel)))
	if err != nil {
		return "", err
	}
	img, pictures := ch.(channels.ImageSender)
	if shot != "" && !pictures {
		return "", fmt.Errorf("%s can't take pictures from me; try another app", channelLabel(name))
	}
	to := ch.OwnerChatID()
	key := name + ":" + to
	d.store.Audit(ctx, "message.out", key, truncate(text, 300))
	if err := ch.Send(ctx, to, text); err != nil {
		return "", fmt.Errorf("%s didn't take it: %w", channelLabel(name), err)
	}
	record := text
	if shot != "" {
		if err := img.SendImage(ctx, to, shot, ""); err != nil {
			d.noticed(ctx, key, text, record)
			return "", fmt.Errorf("the message reached %s but the screenshot didn't: %w", channelLabel(name), err)
		}
		d.store.Audit(ctx, "message.out.image", key, shot)
		record += "\nscreenshot: " + shot
	}
	d.noticed(ctx, key, text, record) // the chat it reached knows what was sent
	if shot != "" {
		return "sent the message and the screenshot to the owner on " + channelLabel(name), nil
	}
	return "sent to the owner on " + channelLabel(name), nil
}

// ownerChannel is the messaging app send_to_owner uses: the one named, or
// the first connected one in MessagingOrder with an owner chat. Voice and
// the terminal are not apps to send to.
func (d *Daemon) ownerChannel(name string) (string, channels.Channel, error) {
	usable := func(n string) (channels.Channel, string) {
		if n == "voice" || n == "cli" || !slices.Contains(channels.MessagingOrder, n) {
			return nil, "isn't a messaging app I can send to"
		}
		ch, ok := d.channel(n)
		if !ok {
			return nil, "isn't connected"
		}
		if ch.OwnerChatID() == "" {
			return nil, "doesn't know your own chat yet; message me there once"
		}
		if up, why := d.channelHealth(n); !up {
			return nil, why
		}
		return ch, ""
	}
	if name != "" {
		ch, why := usable(name)
		if ch == nil {
			return "", nil, fmt.Errorf("%s %s", channelLabel(name), why)
		}
		return name, ch, nil
	}
	for _, n := range channels.MessagingOrder {
		if ch, _ := usable(n); ch != nil {
			return n, ch, nil
		}
	}
	return "", nil, errors.New("none of the owner's messaging apps is connected")
}
