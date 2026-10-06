// Package discord connects the twin to Discord as a bot. Create an application
// at https://discord.com/developers, add a bot, enable the Message Content
// intent, invite it to your server (or just DM it), and put the token in
// DISCORD_BOT_TOKEN. Uses the gateway websocket directly; no public endpoint.
package discord

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/coder/websocket"
)

const (
	apiBase    = "https://discord.com/api/v10"
	gatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"
	// intents: GUILDS | GUILD_MESSAGES | DIRECT_MESSAGES | MESSAGE_CONTENT
	intents = 1<<0 | 1<<9 | 1<<12 | 1<<15
	maxLen  = 1900
)

// Channel is the Discord transport.
type Channel struct {
	channels.Tracker
	backlog       channels.Backlog // tells the replay at connect from live messages
	token         string
	owner         string // user id or username
	replyToOthers bool
	allowed       map[string]bool // guild channel ids to answer in
	log           *slog.Logger
	http          *http.Client
	api           string // REST base, overridable in tests

	mu        sync.Mutex
	selfID    string
	ownerID   string
	ownerChat string
	seq       int64
	sessionID string
	resumeURL string
}

// New builds the channel. owner is a user id or username; extra is guild channel ids to answer in.
func New(token, owner string, replyToOthers bool, extra []string, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	c := &Channel{token: token, owner: strings.TrimSpace(owner), replyToOthers: replyToOthers, log: log,
		http: &http.Client{Timeout: 30 * time.Second}, api: apiBase, allowed: map[string]bool{}}
	for _, id := range extra {
		c.allowed[strings.TrimSpace(id)] = true
	}
	if isSnowflake(c.owner) {
		c.ownerID = c.owner
	}
	return c
}

func isSnowflake(s string) bool {
	if len(s) < 15 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (c *Channel) Name() string { return "discord" }

// ChatLink opens the DM with the bot in Discord.
func (c *Channel) ChatLink() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ownerChat == "" {
		return "", ""
	}
	return "Open the DM in Discord", "https://discord.com/channels/@me/" + c.ownerChat
}

// OwnerChatID is the owner's DM channel once known.
func (c *Channel) OwnerChatID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ownerChat
}

func (c *Channel) rest(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/MavrkAI/Mirrin, 1.0)")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 429 {
		var rl struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(data, &rl)
		time.Sleep(time.Duration(rl.RetryAfter*1000) * time.Millisecond)
		return c.rest(ctx, method, path, body, out)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errBadToken
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("discord %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

var errBadToken = channels.Fatal(errors.New("Discord doesn't accept this bot token; reset it under Bot in the Developer Portal and reconnect"))

// closeError turns the gateway's close codes into what the owner must do.
// Codes not listed here are worth a reconnect.
func closeError(err error) error {
	switch websocket.CloseStatus(err) {
	case 4004:
		return errBadToken
	case 4013, 4014:
		return channels.Fatal(errors.New("Discord refused the Message Content intent; turn it on under Bot → Privileged Gateway Intents in the Developer Portal, then reconnect"))
	case 4010, 4011, 4012:
		return channels.Fatal(fmt.Errorf("Discord closed the connection for good (%d); reconnect it from Channels", websocket.CloseStatus(err)))
	case 4007, 4009:
		return errSessionGone
	}
	return err
}

var errSessionGone = errors.New("session expired")

type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

type message struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Author    struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Global   string `json:"global_name"`
		Bot      bool   `json:"bot"`
	} `json:"author"`
	Mentions []struct {
		ID string `json:"id"`
	} `json:"mentions"`
	Attachments []attachment `json:"attachments"` // media.go
}

// Start connects to the gateway and delivers messages until ctx ends.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if c.token == "" {
		return channels.Fatal(errors.New("discord token missing (set DISCORD_BOT_TOKEN)"))
	}
	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := c.rest(ctx, http.MethodGet, "/users/@me", nil, &me); err != nil {
		c.Retrying(err)
		return err
	}
	c.mu.Lock()
	c.selfID = me.ID
	c.mu.Unlock()
	c.log.Info("discord connected", "bot", me.Username)
	if c.ownerID != "" {
		c.openOwnerDM(ctx)
	}
	backoff := channels.Backoff{Min: time.Second, Max: 30 * time.Second}
	for ctx.Err() == nil {
		started := time.Now()
		err := closeError(c.session(ctx, handler))
		if ctx.Err() != nil {
			return nil
		}
		if channels.IsFatal(err) {
			c.Down(err)
			return err
		}
		if errors.Is(err, errSessionGone) {
			c.mu.Lock()
			c.sessionID, c.resumeURL = "", ""
			c.mu.Unlock()
		}
		c.Retrying(err)
		c.log.Warn("discord gateway", "err", err)
		if !backoff.Wait(ctx, started) {
			return nil
		}
	}
	return nil
}

func (c *Channel) openOwnerDM(ctx context.Context) {
	var dm struct {
		ID string `json:"id"`
	}
	if err := c.rest(ctx, http.MethodPost, "/users/@me/channels", map[string]string{"recipient_id": c.ownerID}, &dm); err == nil && dm.ID != "" {
		c.mu.Lock()
		c.ownerChat = dm.ID
		c.mu.Unlock()
	}
}

// session runs one gateway connection (identify or resume) until it drops.
func (c *Channel) session(ctx context.Context, handler channels.Handler) error {
	c.mu.Lock()
	url, sid := c.resumeURL, c.sessionID
	c.mu.Unlock()
	if url == "" {
		url = gatewayURL
	} else if !strings.Contains(url, "?") {
		url += "/?v=10&encoding=json"
	}
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		c.mu.Lock()
		c.resumeURL = ""
		c.mu.Unlock()
		return err
	}
	conn.SetReadLimit(8 << 20)
	c.backlog.ConnectedUntilDone() // a resume replays what was missed, then says RESUMED
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	send := func(op int, d any) error {
		b, _ := json.Marshal(d)
		p, _ := json.Marshal(payload{Op: op, D: b})
		return conn.Write(sctx, websocket.MessageText, p)
	}
	for {
		_, data, err := conn.Read(sctx)
		if err != nil {
			return err
		}
		var p payload
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}
		if p.S != nil {
			c.mu.Lock()
			c.seq = *p.S
			c.mu.Unlock()
		}
		switch p.Op {
		case 10: // hello
			var h struct {
				Interval int `json:"heartbeat_interval"`
			}
			_ = json.Unmarshal(p.D, &h)
			go c.heartbeat(sctx, send, time.Duration(h.Interval)*time.Millisecond)
			if sid != "" {
				c.mu.Lock()
				seq := c.seq
				c.mu.Unlock()
				err = send(6, map[string]any{"token": c.token, "session_id": sid, "seq": seq})
			} else {
				err = send(2, map[string]any{
					"token":      c.token,
					"intents":    intents,
					"properties": map[string]string{"os": "mirrin", "browser": "mirrin", "device": "mirrin"},
				})
			}
			if err != nil {
				return err
			}
		case 1:
			c.mu.Lock()
			seq := c.seq
			c.mu.Unlock()
			_ = send(1, seq)
		case 7: // reconnect
			return errors.New("gateway asked to reconnect")
		case 9: // invalid session
			var resumable bool
			_ = json.Unmarshal(p.D, &resumable)
			if !resumable {
				c.mu.Lock()
				c.sessionID, c.resumeURL = "", ""
				c.mu.Unlock()
			}
			time.Sleep(2 * time.Second)
			return errors.New("invalid session")
		case 0:
			c.dispatch(ctx, p, handler)
		}
	}
}

func (c *Channel) heartbeat(ctx context.Context, send func(int, any) error, every time.Duration) {
	if every <= 0 {
		every = 41 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.mu.Lock()
			seq := c.seq
			c.mu.Unlock()
			if err := send(1, seq); err != nil {
				return
			}
		}
	}
}

func (c *Channel) dispatch(ctx context.Context, p payload, handler channels.Handler) {
	switch p.T {
	case "READY":
		var r struct {
			SessionID string `json:"session_id"`
			ResumeURL string `json:"resume_gateway_url"`
		}
		_ = json.Unmarshal(p.D, &r)
		c.mu.Lock()
		c.sessionID, c.resumeURL = r.SessionID, r.ResumeURL
		c.mu.Unlock()
		c.Up()
		c.backlog.Done()
	case "RESUMED":
		c.Up()
		c.backlog.Done()
	case "MESSAGE_CREATE":
		var m message
		if err := json.Unmarshal(p.D, &m); err != nil {
			return
		}
		c.onMessage(ctx, m, handler)
	}
}

func (c *Channel) onMessage(ctx context.Context, m message, handler channels.Handler) {
	c.mu.Lock()
	self := c.selfID
	c.mu.Unlock()
	kind := ""
	if len(m.Attachments) > 0 {
		kind = channels.MediaKind(m.Attachments[0].ContentType)
	}
	if m.Author.Bot || m.Author.ID == self || (strings.TrimSpace(m.Content) == "" && kind == "") {
		return
	}
	if ts, err := time.Parse(time.RFC3339, m.Timestamp); err == nil && c.backlog.Stale(ts) {
		return
	}
	isOwner := (c.ownerID != "" && m.Author.ID == c.ownerID) || (c.ownerID == "" && channels.MatchOwner(c.owner, m.Author.Username))
	text := m.Content
	if m.GuildID != "" {
		mentioned := false
		for _, u := range m.Mentions {
			if u.ID == self {
				mentioned = true
			}
		}
		if !c.allowed[m.ChannelID] && !mentioned {
			return
		}
		text = strings.TrimSpace(strings.NewReplacer("<@"+self+">", "", "<@!"+self+">", "").Replace(text))
		if text == "" && kind == "" {
			return
		}
	} else if isOwner {
		c.mu.Lock()
		c.ownerChat = m.ChannelID
		if c.ownerID == "" {
			c.ownerID = m.Author.ID
		}
		c.mu.Unlock()
	}
	if !isOwner && !c.replyToOthers {
		return
	}
	sender := m.Author.Username
	if m.Author.Global != "" {
		sender = m.Author.Global
	}
	handler(ctx, channels.Inbound{Channel: "discord", ChatID: m.ChannelID, Sender: sender, Text: strings.TrimSpace(text), Media: kind,
		Attachment: c.attachment(m), IsOwner: isOwner}) // media.go
}

// Typing shows "typing…" in the channel; Discord clears it after ten seconds.
func (c *Channel) Typing(ctx context.Context, chatID string) (time.Duration, error) {
	return 8 * time.Second, c.rest(ctx, http.MethodPost, "/channels/"+chatID+"/typing", nil, nil)
}

// Send posts text to a channel, split under Discord's 2000-character limit.
func (c *Channel) Send(ctx context.Context, chatID, text string) error {
	return channels.SendParts(text, maxLen, func(part string) error {
		return c.rest(ctx, http.MethodPost, "/channels/"+chatID+"/messages", map[string]any{"content": part}, nil)
	})
}

// SendImage uploads a file with a caption.
func (c *Channel) SendImage(ctx context.Context, chatID, path, caption string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	pj, _ := json.Marshal(map[string]any{"content": caption, "attachments": []map[string]any{{"id": 0, "filename": filepath.Base(path)}}})
	_ = mw.WriteField("payload_json", string(pj))
	fw, err := mw.CreateFormFile("files[0]", filepath.Base(path))
	if err != nil {
		return err
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/channels/"+chatID+"/messages", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord upload: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// InviteURL derives the bot's application id from its token and returns the
// OAuth link that adds it to a server with the permissions it needs.
func InviteURL(token string) string {
	seg := token
	if i := strings.Index(seg, "."); i > 0 {
		seg = seg[:i]
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil || len(raw) == 0 {
		return ""
	}
	if !isSnowflake(string(raw)) {
		return ""
	}
	// View Channel 1024 + Send Messages 2048 + Attach Files 32768 + Read Message History 65536
	return "https://discord.com/oauth2/authorize?client_id=" + string(raw) + "&scope=bot&permissions=101376"
}
