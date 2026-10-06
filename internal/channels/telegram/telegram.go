// Package telegram connects Mirrin to a Telegram bot. Create one with
// @BotFather, put the token in TELEGRAM_BOT_TOKEN, set your user id as owner,
// and message the bot from your phone. Long polling, no public endpoint.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Channel is the Telegram transport.
type Channel struct {
	channels.Tracker
	backlog       channels.Backlog // tells the replay at connect from live messages
	token         string
	ownerID       int64
	ownerUsername string
	replyToOthers bool
	log           *slog.Logger
	http          *http.Client
	api           string // Bot API base, overridable in tests

	mu          sync.Mutex
	ownerChat   int64 // learned from the first owner message if ownerID unset
	botUsername string
}

// ChatLink opens the bot's chat in Telegram.
func (c *Channel) ChatLink() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.botUsername == "" {
		return "", ""
	}
	return "Open the chat in Telegram", "https://t.me/" + c.botUsername
}

// New builds the channel. owner may be a numeric user id or an @username.
func New(token, owner string, replyToOthers bool, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	c := &Channel{token: token, replyToOthers: replyToOthers, log: log, http: &http.Client{Timeout: 70 * time.Second}, api: "https://api.telegram.org"}
	owner = strings.TrimSpace(owner)
	if id, err := strconv.ParseInt(owner, 10, 64); err == nil {
		c.ownerID = id
		c.ownerChat = id
	} else {
		c.ownerUsername = strings.TrimPrefix(strings.ToLower(owner), "@")
	}
	return c
}

func (c *Channel) Name() string { return "telegram" }

// OwnerChatID is the owner's private chat (same as their user id).
func (c *Channel) OwnerChatID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ownerChat == 0 {
		return ""
	}
	return strconv.FormatInt(c.ownerChat, 10)
}

type message struct {
	ID      int64  `json:"message_id"`
	Date    int64  `json:"date"`
	Text    string `json:"text"`
	Caption string `json:"caption"`
	From    struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		First    string `json:"first_name"`
	} `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	// Attachments, only checked for presence.
	Voice     json.RawMessage `json:"voice"`
	Audio     json.RawMessage `json:"audio"`
	VideoNote json.RawMessage `json:"video_note"`
	Video     json.RawMessage `json:"video"`
	Photo     json.RawMessage `json:"photo"`
	Document  json.RawMessage `json:"document"`
}

// media names what came with the message, "" if nothing the twin can't read.
func (m *message) media() string {
	switch {
	case m.Voice != nil, m.Audio != nil:
		return channels.Voice
	case m.Photo != nil:
		return channels.Photo
	case m.Video != nil, m.VideoNote != nil:
		return channels.Video
	case m.Document != nil:
		return channels.File
	}
	return ""
}

type update struct {
	ID      int64    `json:"update_id"`
	Message *message `json:"message"`
}

func (c *Channel) call(ctx context.Context, method string, params any, out any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL carries the bot token; keep it out of logs and the page
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: %s", method, resp.Status)
	}
	if !env.OK {
		switch env.Code {
		case http.StatusUnauthorized, http.StatusNotFound:
			return channels.Fatal(errors.New("Telegram doesn't accept this bot token; copy it again from @BotFather and reconnect"))
		case http.StatusConflict:
			if strings.Contains(strings.ToLower(env.Description), "webhook") {
				return channels.Fatal(errors.New("this bot sends its messages to a webhook, so the twin can't read them; remove the webhook (Telegram's deleteWebhook) and reconnect"))
			}
			return errors.New("another program is reading this bot's messages (is the twin running twice?)")
		}
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// Start long-polls for updates until ctx ends.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if c.token == "" {
		return channels.Fatal(errors.New("telegram token missing (set TELEGRAM_BOT_TOKEN)"))
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := c.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		c.Retrying(err)
		return err
	}
	c.mu.Lock()
	c.botUsername = me.Username
	c.mu.Unlock()
	c.Up()
	c.backlog.ConnectedUntilDone()
	c.log.Info("telegram connected", "bot", "@"+me.Username)
	var offset int64
	backoff := channels.Backoff{Min: time.Second, Max: 30 * time.Second}
	for ctx.Err() == nil {
		var updates []update
		err := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "limit": updatesLimit, "timeout": 50, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if channels.IsFatal(err) {
				c.Down(err)
				return err
			}
			c.Retrying(err)
			c.log.Warn("telegram poll", "err", err)
			if !backoff.Wait(ctx, time.Time{}) {
				return nil
			}
			continue
		}
		c.Up()
		backoff.Reset()
		for _, u := range updates {
			offset = u.ID + 1
			c.onUpdate(ctx, u, handler)
		}
		if len(updates) < updatesLimit {
			c.backlog.Done() // a short batch: nothing more was queued
		}
	}
	return nil
}

// updatesLimit is the most updates one getUpdates returns (Telegram's
// maximum). A full batch means more of the queue is still waiting.
const updatesLimit = 100

func (c *Channel) onUpdate(ctx context.Context, u update, handler channels.Handler) {
	m := u.Message
	if m == nil || m.Chat.Type != "private" {
		return
	}
	text, kind := m.Text, m.media()
	if text == "" {
		text = m.Caption
	}
	if text == "" && kind == "" {
		return // a sticker, a location, a service message
	}
	if c.backlog.Stale(time.Unix(m.Date, 0)) {
		return // don't replay a backlog after a restart
	}
	isOwner := (c.ownerID != 0 && m.From.ID == c.ownerID) ||
		(c.ownerUsername != "" && strings.ToLower(m.From.Username) == c.ownerUsername)
	if isOwner {
		c.mu.Lock()
		if c.ownerChat == 0 {
			c.ownerChat = m.Chat.ID
		}
		c.mu.Unlock()
	}
	if !isOwner && !c.replyToOthers {
		return
	}
	sender := m.From.First
	if m.From.Username != "" {
		sender = "@" + m.From.Username
	}
	handler(ctx, channels.Inbound{Channel: "telegram", ChatID: strconv.FormatInt(m.Chat.ID, 10), Sender: sender, Text: text, Media: kind,
		Attachment: c.attachment(m), IsOwner: isOwner}) // media.go
}

// Typing shows "typing…" in the chat; Telegram clears it after five seconds.
func (c *Channel) Typing(ctx context.Context, chatID string) (time.Duration, error) {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return 0, err
	}
	return 4 * time.Second, c.call(ctx, "sendChatAction", map[string]any{"chat_id": id, "action": "typing"}, nil)
}

// Send delivers text, splitting under Telegram's 4096-character limit.
func (c *Channel) Send(ctx context.Context, chatID, text string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("bad telegram chat id %q", chatID)
	}
	return channels.SendParts(text, 4000, func(part string) error {
		return c.call(ctx, "sendMessage", map[string]any{"chat_id": id, "text": part}, nil)
	})
}

// SendImage posts a photo with a caption.
func (c *Channel) SendImage(ctx context.Context, chatID, path, caption string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", chatID)
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	fw, err := mw.CreateFormFile("photo", filepath.Base(path))
	if err != nil {
		return err
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/bot"+c.token+"/sendPhoto", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("telegram sendPhoto: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram sendPhoto: %s", strings.TrimSpace(string(b)))
	}
	return nil
}
