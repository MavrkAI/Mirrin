// Package zulip connects the twin to a Zulip organisation as a bot
// (Settings → Your bots → Add a new bot, type Generic). Set channels.zulip.site,
// the bot's email, ZULIP_API_KEY and your own email as owner. It answers
// direct messages and stream messages that @-mention it.
package zulip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Channel is the Zulip transport.
type Channel struct {
	channels.Tracker
	backlog          channels.Backlog // tells the replay at connect from live messages
	site, email, key string
	owner            string
	replyToOthers    bool
	log              *slog.Logger
	http             *http.Client
	selfID           int64

	mu  sync.Mutex
	ids map[string]int64 // sender email → user id, for typing notices
}

// New builds the channel.
func New(site, email, key, owner string, replyToOthers bool, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	return &Channel{site: strings.TrimRight(site, "/"), email: email, key: key, owner: strings.TrimSpace(owner),
		replyToOthers: replyToOthers, log: log, http: &http.Client{Timeout: 120 * time.Second}, ids: map[string]int64{}}
}

func (c *Channel) Name() string { return "zulip" }

// OwnerChatID is a direct-message conversation with the owner.
func (c *Channel) OwnerChatID() string { return "pm:" + c.owner }

func (c *Channel) call(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	u := c.site + "/api/v1" + path
	if method == http.MethodGet {
		if len(form) > 0 {
			u += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.email, c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var env struct {
		Result string `json:"result"`
		Msg    string `json:"msg"`
		Code   string `json:"code"`
	}
	_ = json.Unmarshal(data, &env)
	if env.Result != "success" {
		switch {
		case resp.StatusCode == http.StatusUnauthorized, env.Code == "INVALID_API_KEY", env.Code == "USER_DEACTIVATED", env.Code == "REALM_DEACTIVATED":
			return channels.Fatal(fmt.Errorf("Zulip doesn't accept the bot's email and API key (%s); check them and reconnect", env.Msg))
		case env.Result == "":
			return fmt.Errorf("zulip %s: %s", path, resp.Status)
		}
		return fmt.Errorf("zulip %s: %s (%s)", path, env.Msg, env.Code)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type zmessage struct {
	ID          int64           `json:"id"`
	SenderEmail string          `json:"sender_email"`
	SenderName  string          `json:"sender_full_name"`
	SenderID    int64           `json:"sender_id"`
	Type        string          `json:"type"`
	Content     string          `json:"content"`
	StreamID    int64           `json:"stream_id"`
	Subject     string          `json:"subject"`
	Timestamp   int64           `json:"timestamp"`
	Recipient   json.RawMessage `json:"display_recipient"`
}

// Start registers an event queue and long-polls it until ctx ends.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if c.key == "" || c.site == "" || c.email == "" {
		return channels.Fatal(errors.New("zulip site, email or api key missing (set ZULIP_API_KEY)"))
	}
	var me struct {
		ID int64 `json:"user_id"`
	}
	if err := c.call(ctx, http.MethodGet, "/users/me", nil, &me); err != nil {
		c.Retrying(err)
		return err
	}
	c.selfID = me.ID
	c.log.Info("zulip connected", "bot", c.email)
	backoff := channels.Backoff{Min: time.Second, Max: 30 * time.Second}
	for ctx.Err() == nil {
		started := time.Now()
		err := c.queue(ctx, handler)
		if ctx.Err() != nil {
			return nil
		}
		if channels.IsFatal(err) {
			c.Down(err)
			return err
		}
		c.Retrying(err)
		c.log.Warn("zulip events", "err", err)
		if !backoff.Wait(ctx, started) {
			return nil
		}
	}
	return nil
}

func (c *Channel) queue(ctx context.Context, handler channels.Handler) error {
	var reg struct {
		QueueID string `json:"queue_id"`
		LastID  int64  `json:"last_event_id"`
	}
	if err := c.call(ctx, http.MethodPost, "/register", url.Values{"event_types": {`["message"]`}, "apply_markdown": {"false"}}, &reg); err != nil {
		return err
	}
	c.Up()
	c.backlog.Connected()
	last := reg.LastID
	for ctx.Err() == nil {
		var res struct {
			Events []struct {
				ID      int64    `json:"id"`
				Type    string   `json:"type"`
				Message zmessage `json:"message"`
				Flags   []string `json:"flags"`
			} `json:"events"`
		}
		err := c.call(ctx, http.MethodGet, "/events", url.Values{"queue_id": {reg.QueueID}, "last_event_id": {strconv.FormatInt(last, 10)}}, &res)
		if err != nil {
			return err // includes BAD_EVENT_QUEUE_ID; Start re-registers
		}
		for _, ev := range res.Events {
			last = ev.ID
			if ev.Type == "message" {
				mentioned := false
				for _, f := range ev.Flags {
					if f == "mentioned" || f == "wildcard_mentioned" {
						mentioned = true
					}
				}
				c.onMessage(ctx, ev.Message, mentioned, handler)
			}
		}
	}
	return nil
}

func (c *Channel) onMessage(ctx context.Context, m zmessage, mentioned bool, handler channels.Handler) {
	if m.SenderID == c.selfID || strings.TrimSpace(m.Content) == "" {
		return
	}
	if m.Timestamp > 0 && c.backlog.Stale(time.Unix(m.Timestamp, 0)) {
		return
	}
	var chatID string
	text := m.Content
	switch m.Type {
	case "private":
		chatID = "pm:" + m.SenderEmail
		c.mu.Lock()
		c.ids[strings.ToLower(m.SenderEmail)] = m.SenderID
		c.mu.Unlock()
	case "stream":
		if !mentioned {
			return
		}
		chatID = fmt.Sprintf("stream:%d:%s", m.StreamID, m.Subject)
		if i := strings.Index(text, "**"); strings.HasPrefix(text, "@**") && i == 1 {
			if j := strings.Index(text[3:], "**"); j >= 0 {
				text = strings.TrimSpace(text[3+j+2:])
			}
		}
	default:
		return
	}
	isOwner := channels.MatchOwner(c.owner, m.SenderEmail)
	if !isOwner && !c.replyToOthers {
		return
	}
	handler(ctx, channels.Inbound{Channel: "zulip", ChatID: chatID, Sender: m.SenderName, Text: text, IsOwner: isOwner})
}

// Typing shows "typing…" in a direct conversation; clients clear it after
// fifteen seconds without a refresh.
func (c *Channel) Typing(ctx context.Context, chatID string) (time.Duration, error) {
	form := c.typingForm(chatID, "start")
	if form == nil {
		return 0, nil // stream topics, or someone not heard from yet
	}
	return 10 * time.Second, c.call(ctx, http.MethodPost, "/typing", form, nil)
}

// EndTyping clears the indicator once the reply is sent.
func (c *Channel) EndTyping(ctx context.Context, chatID string) error {
	if form := c.typingForm(chatID, "stop"); form != nil {
		return c.call(ctx, http.MethodPost, "/typing", form, nil)
	}
	return nil
}

// typingForm is the /typing request for a direct message, nil otherwise.
func (c *Channel) typingForm(chatID, op string) url.Values {
	c.mu.Lock()
	id, ok := c.ids[strings.ToLower(strings.TrimPrefix(chatID, "pm:"))]
	c.mu.Unlock()
	if !strings.HasPrefix(chatID, "pm:") || !ok {
		return nil
	}
	return url.Values{"op": {op}, "type": {"private"}, "to": {"[" + strconv.FormatInt(id, 10) + "]"}}
}

// Send delivers text to "pm:email" or "stream:id:topic".
func (c *Channel) Send(ctx context.Context, chatID, text string) error {
	to := url.Values{}
	switch {
	case strings.HasPrefix(chatID, "pm:"):
		to.Set("type", "private")
		to.Set("to", `["`+strings.TrimPrefix(chatID, "pm:")+`"]`)
	case strings.HasPrefix(chatID, "stream:"):
		parts := strings.SplitN(chatID, ":", 3)
		if len(parts) < 3 {
			return fmt.Errorf("bad zulip chat id %q", chatID)
		}
		to.Set("type", "stream")
		to.Set("to", parts[1])
		to.Set("topic", parts[2])
	default:
		return fmt.Errorf("bad zulip chat id %q", chatID)
	}
	return channels.SendParts(text, 9000, func(part string) error {
		form := url.Values{"content": {part}}
		for k, v := range to {
			form[k] = v
		}
		return c.call(ctx, http.MethodPost, "/messages", form, nil)
	})
}
