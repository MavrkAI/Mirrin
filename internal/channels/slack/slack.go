// Package slack connects the twin to a Slack workspace over Socket Mode, so
// no public URL is needed. Create an app at https://api.slack.com/apps,
// enable Socket Mode (app-level token with connections:write → SLACK_APP_TOKEN),
// add bot scopes chat:write, im:history, im:read, users:read, files:read, files:write,
// app_mentions:read, channels:history, subscribe to message.im and
// app_mention events, install it (bot token → SLACK_BOT_TOKEN), then DM the app.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/coder/websocket"
)

const maxLen = 3900

// Channel is the Slack transport.
type Channel struct {
	channels.Tracker
	backlog            channels.Backlog // tells the replay at connect from live messages
	botToken, appToken string
	owner              string
	replyToOthers      bool
	log                *slog.Logger
	http               *http.Client
	api                string // base URL, overridable in tests

	mu      sync.Mutex
	selfID  string
	teamID  string
	ownerID string
	// ownerLookedUp is set once users.list was read through: from then on
	// the owner is whoever it named, never adopted later from a handle.
	ownerLookedUp bool
	ownerChat     string
	names         map[string]string // user id → handle
	seen          map[string]time.Time
}

// New builds the channel. owner is a member id (U…) or @handle.
func New(botToken, appToken, owner string, replyToOthers bool, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	c := &Channel{botToken: botToken, appToken: appToken, owner: strings.TrimSpace(owner), replyToOthers: replyToOthers,
		log: log, http: &http.Client{Timeout: 30 * time.Second}, api: "https://slack.com/api/",
		names: map[string]string{}, seen: map[string]time.Time{}}
	if strings.HasPrefix(c.owner, "U") && len(c.owner) >= 9 && strings.ToUpper(c.owner) == c.owner {
		c.ownerID = c.owner
	}
	return c
}

func (c *Channel) Name() string { return "slack" }

// ChatLink opens the DM with the app in Slack.
func (c *Channel) ChatLink() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ownerChat == "" || c.teamID == "" {
		return "", ""
	}
	return "Open the DM in Slack", "https://app.slack.com/client/" + c.teamID + "/" + c.ownerChat
}

// OwnerChatID is the owner's DM channel once opened.
func (c *Channel) OwnerChatID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ownerChat
}

func (c *Channel) call(ctx context.Context, token, method string, params any, out any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	if !env.OK {
		switch env.Error {
		case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired", "not_allowed_token_type":
			which := "bot token (xoxb-…)"
			if token == c.appToken {
				which = "app-level token (xapp-…)"
			}
			return channels.Fatal(fmt.Errorf("Slack doesn't accept the %s: %s. Copy it again from your app's settings and reconnect", which, env.Error))
		}
		return fmt.Errorf("slack %s: %s", method, env.Error)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Start connects over Socket Mode and delivers messages until ctx ends.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if c.botToken == "" || c.appToken == "" {
		return channels.Fatal(errors.New("slack tokens missing (set SLACK_BOT_TOKEN and SLACK_APP_TOKEN)"))
	}
	var auth struct {
		UserID string `json:"user_id"`
		Team   string `json:"team"`
		TeamID string `json:"team_id"`
	}
	if err := c.call(ctx, c.botToken, "auth.test", map[string]any{}, &auth); err != nil {
		c.Retrying(err)
		return err
	}
	c.mu.Lock()
	c.selfID, c.teamID = auth.UserID, auth.TeamID
	c.mu.Unlock()
	c.log.Info("slack connected", "team", auth.Team)
	if c.ownerID == "" {
		c.resolveOwner(ctx)
	}
	if c.ownerID != "" {
		c.openOwnerDM(ctx)
	}
	backoff := channels.Backoff{Min: time.Second, Max: 30 * time.Second}
	for ctx.Err() == nil {
		started := time.Now()
		err := c.socket(ctx, handler)
		if ctx.Err() != nil {
			return nil
		}
		if channels.IsFatal(err) {
			c.Down(err)
			return err
		}
		c.Retrying(err)
		c.log.Warn("slack socket", "err", err)
		// Slack refreshes Socket Mode connections every few hours: after a
		// healthy session the next attempt comes straight away.
		if !backoff.Wait(ctx, started) {
			return nil
		}
	}
	return nil
}

// resolveOwner turns an @handle into a member id by paging users.list.
// Only the member's unique handle ("name") counts: display names are free
// text anyone in the workspace can set, so two people can share one and a
// stranger could take the owner's. Once the list has been read through,
// the owner is settled: nobody is adopted later from a matching handle,
// even when no member (or more than one) matched.
func (c *Channel) resolveOwner(ctx context.Context) {
	want := strings.TrimPrefix(strings.ToLower(c.owner), "@")
	cursor, found, many := "", "", false
	for i := 0; ; i++ {
		var page struct {
			Members []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"members"`
			Meta struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, c.botToken, "users.list", map[string]any{"limit": 200, "cursor": cursor}, &page); err != nil {
			c.log.Warn("slack users.list", "err", err)
			return // not settled: tried again when a matching handle writes
		}
		for _, m := range page.Members {
			if strings.ToLower(m.Name) == want && m.ID != found {
				many = many || found != ""
				found = m.ID
			}
		}
		if page.Meta.Next == "" || i == 19 {
			break
		}
		cursor = page.Meta.Next
	}
	switch {
	case many:
		c.log.Warn("slack owner handle matches more than one member; set the owner's member id (U…) instead", "owner", c.owner)
		found = ""
	case found == "":
		c.log.Warn("no slack member has this handle; set the owner's member id (U…) or their @handle", "owner", c.owner)
	}
	c.mu.Lock()
	c.ownerID, c.ownerLookedUp = found, true
	c.mu.Unlock()
}

func (c *Channel) openOwnerDM(ctx context.Context) {
	var res struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := c.call(ctx, c.botToken, "conversations.open", map[string]any{"users": c.ownerID}, &res); err == nil {
		c.mu.Lock()
		c.ownerChat = res.Channel.ID
		c.mu.Unlock()
	}
}

type envelope struct {
	ID      string `json:"envelope_id"`
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Payload struct {
		Event struct {
			Type        string      `json:"type"`
			Subtype     string      `json:"subtype"`
			Channel     string      `json:"channel"`
			ChannelType string      `json:"channel_type"`
			User        string      `json:"user"`
			BotID       string      `json:"bot_id"`
			Text        string      `json:"text"`
			TS          string      `json:"ts"`
			ThreadTS    string      `json:"thread_ts"`
			Files       []slackFile `json:"files"`
		} `json:"event"`
	} `json:"payload"`
}

func (c *Channel) socket(ctx context.Context, handler channels.Handler) error {
	var open struct {
		URL string `json:"url"`
	}
	if err := c.call(ctx, c.appToken, "apps.connections.open", map[string]any{}, &open); err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, open.URL, nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(4 << 20)
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.ID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.ID})
			_ = conn.Write(ctx, websocket.MessageText, ack)
		}
		switch env.Type {
		case "hello":
			c.Up()
			c.backlog.Connected()
		case "disconnect":
			return fmt.Errorf("server disconnect: %s", env.Reason)
		case "events_api":
			c.onEvent(ctx, env, handler)
		}
	}
}

func (c *Channel) onEvent(ctx context.Context, env envelope, handler channels.Handler) {
	ev := env.Payload.Event
	kind := ""
	if len(ev.Files) > 0 {
		kind = channels.MediaKind(ev.Files[0].Mimetype)
	}
	if ev.BotID != "" || (ev.Subtype != "" && ev.Subtype != "file_share") || (strings.TrimSpace(ev.Text) == "" && kind == "") {
		return
	}
	c.mu.Lock()
	self := c.selfID
	if ev.User == self {
		c.mu.Unlock()
		return
	}
	// app_mention and message.channels both arrive for a mention; dedupe on ts.
	if t, ok := c.seen[ev.TS]; ok && time.Since(t) < time.Minute {
		c.mu.Unlock()
		return
	}
	c.seen[ev.TS] = time.Now()
	for k, t := range c.seen {
		if time.Since(t) > 2*time.Minute {
			delete(c.seen, k)
		}
	}
	c.mu.Unlock()
	if sec, err := strconv.ParseFloat(ev.TS, 64); err == nil && c.backlog.Stale(time.Unix(int64(sec), 0)) {
		return
	}
	text := ev.Text
	switch {
	case ev.Type == "message" && ev.ChannelType == "im":
	case ev.Type == "app_mention" || strings.Contains(text, "<@"+self+">"):
		text = strings.TrimSpace(strings.ReplaceAll(text, "<@"+self+">", ""))
		if text == "" && kind == "" {
			return
		}
	default:
		return
	}
	c.mu.Lock()
	ownerID, lookedUp := c.ownerID, c.ownerLookedUp
	c.mu.Unlock()
	if ownerID == "" && !lookedUp && channels.MatchOwner(c.owner, c.handle(ctx, ev.User)) {
		// users.list failed at connect: look again rather than adopting
		// whoever has the handle (a Slack Connect guest can share it).
		c.resolveOwner(ctx)
		c.mu.Lock()
		ownerID = c.ownerID
		c.mu.Unlock()
	}
	isOwner := ownerID != "" && ev.User == ownerID
	if isOwner && ev.ChannelType == "im" {
		c.mu.Lock()
		c.ownerChat = ev.Channel
		c.mu.Unlock()
	}
	if !isOwner && !c.replyToOthers {
		return
	}
	text = strings.TrimSpace(unescape(text))
	handler(ctx, channels.Inbound{Channel: "slack", ChatID: ev.Channel, Sender: "@" + c.handle(ctx, ev.User), Text: text, Media: kind, Attachment: c.attachment(ev.Files), IsOwner: isOwner})
}

func unescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

func (c *Channel) handle(ctx context.Context, userID string) string {
	c.mu.Lock()
	if n, ok := c.names[userID]; ok {
		c.mu.Unlock()
		return n
	}
	c.mu.Unlock()
	var res struct {
		User struct {
			Name string `json:"name"`
		} `json:"user"`
	}
	name := userID
	if err := c.call(ctx, c.botToken, "users.info", map[string]any{"user": userID}, &res); err == nil && res.User.Name != "" {
		name = res.User.Name
	}
	c.mu.Lock()
	c.names[userID] = name
	c.mu.Unlock()
	return name
}

// Send posts text to a channel or DM.
func (c *Channel) Send(ctx context.Context, chatID, text string) error {
	return channels.SendParts(text, maxLen, func(part string) error {
		return c.call(ctx, c.botToken, "chat.postMessage", map[string]any{"channel": chatID, "text": part, "mrkdwn": true}, nil)
	})
}

// SendImage uploads a file to the conversation (files.getUploadURLExternal flow).
func (c *Channel) SendImage(ctx context.Context, chatID, path, caption string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var up struct {
		URL    string `json:"upload_url"`
		FileID string `json:"file_id"`
	}
	q := url.Values{"filename": {filepath.Base(path)}, "length": {strconv.Itoa(len(data))}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.api+"files.getUploadURLExternal?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+c.botToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(body, &up); err != nil || up.URL == "" {
		return fmt.Errorf("slack getUploadURLExternal: %s", strings.TrimSpace(string(body)))
	}
	preq, _ := http.NewRequestWithContext(ctx, http.MethodPost, up.URL, bytes.NewReader(data))
	preq.Header.Set("Content-Type", "application/octet-stream")
	presp, err := c.http.Do(preq)
	if err != nil {
		return err
	}
	presp.Body.Close()
	return c.call(ctx, c.botToken, "files.completeUploadExternal", map[string]any{
		"files":           []map[string]string{{"id": up.FileID, "title": filepath.Base(path)}},
		"channel_id":      chatID,
		"initial_comment": caption,
	}, nil)
}

// ManifestURL is a link that opens Slack's "create app" page with everything
// this channel needs already filled in: Socket Mode, scopes, events, App Home.
func ManifestURL(botName string) string {
	if botName == "" {
		botName = "Mirrin"
	}
	m := map[string]any{
		"display_information": map[string]any{"name": botName, "description": "Your Mirrin twin"},
		"features": map[string]any{
			"bot_user": map[string]any{"display_name": botName, "always_online": true},
			"app_home": map[string]any{"messages_tab_enabled": true, "messages_tab_read_only_enabled": false},
		},
		"oauth_config": map[string]any{"scopes": map[string]any{"bot": []string{
			"chat:write", "im:history", "im:read", "im:write", "users:read", "files:read", "files:write", "app_mentions:read", "channels:history",
		}}},
		"settings": map[string]any{
			"socket_mode_enabled": true,
			"event_subscriptions": map[string]any{"bot_events": []string{"message.im", "app_mention"}},
		},
	}
	b, _ := json.Marshal(m)
	return "https://api.slack.com/apps?new_app=1&manifest_json=" + url.QueryEscape(string(b))
}
