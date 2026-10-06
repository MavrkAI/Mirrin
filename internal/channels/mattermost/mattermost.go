// Package mattermost connects the twin to a Mattermost server as a bot account
// (System Console → Integrations → Bot Accounts, or any personal access token).
// Set channels.mattermost.url, MATTERMOST_TOKEN and your username as owner,
// then DM the bot. In channels it answers when @mentioned.
package mattermost

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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/coder/websocket"
)

// Channel is the Mattermost transport.
type Channel struct {
	channels.Tracker
	backlog       channels.Backlog // tells the replay at connect from live messages
	url, token    string
	owner         string
	replyToOthers bool
	log           *slog.Logger
	http          *http.Client

	mu        sync.Mutex
	selfID    string
	selfName  string
	ownerID   string
	ownerChat string
}

// New builds the channel. owner is a username or user id.
func New(serverURL, token, owner string, replyToOthers bool, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	return &Channel{url: strings.TrimRight(serverURL, "/"), token: token, owner: strings.TrimSpace(owner),
		replyToOthers: replyToOthers, log: log, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Channel) Name() string { return "mattermost" }

// OwnerChatID is the DM channel with the owner once known.
func (c *Channel) OwnerChatID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ownerChat
}

func (c *Channel) api(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+"/api/v4"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return channels.Fatal(errors.New("the Mattermost server doesn't accept the bot's token; make a new one and reconnect"))
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("mattermost %s %s: %d %s", method, path, resp.StatusCode, e.Message)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type user struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Start connects the websocket and delivers posts until ctx ends.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if c.token == "" || c.url == "" {
		return channels.Fatal(errors.New("mattermost url or token missing (set MATTERMOST_TOKEN)"))
	}
	var me user
	if err := c.api(ctx, http.MethodGet, "/users/me", nil, &me); err != nil {
		c.Retrying(err)
		return err
	}
	c.mu.Lock()
	c.selfID, c.selfName = me.ID, me.Username
	c.mu.Unlock()
	c.log.Info("mattermost connected", "bot", me.Username)
	var owner user
	if err := c.api(ctx, http.MethodGet, "/users/username/"+strings.TrimPrefix(c.owner, "@"), nil, &owner); err != nil {
		if err2 := c.api(ctx, http.MethodGet, "/users/"+c.owner, nil, &owner); err2 != nil {
			c.log.Warn("mattermost owner lookup", "err", err)
		}
	}
	if owner.ID != "" {
		var dm struct {
			ID string `json:"id"`
		}
		if err := c.api(ctx, http.MethodPost, "/channels/direct", []string{me.ID, owner.ID}, &dm); err == nil {
			c.mu.Lock()
			c.ownerID, c.ownerChat = owner.ID, dm.ID
			c.mu.Unlock()
		}
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
		c.log.Warn("mattermost socket", "err", err)
		if !backoff.Wait(ctx, started) {
			return nil
		}
	}
	return nil
}

type wsEvent struct {
	Event  string `json:"event"`
	Status string `json:"status"` // "OK" answers the authentication challenge
	Data   struct {
		Post        string `json:"post"`
		ChannelType string `json:"channel_type"`
		SenderName  string `json:"sender_name"`
	} `json:"data"`
}

type post struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	Message   string `json:"message"`
	CreateAt  int64  `json:"create_at"`
	Props     struct {
		FromBot string `json:"from_bot"`
	} `json:"props"`
	FileIDs  []string `json:"file_ids"`
	Metadata struct {
		Files []fileInfo `json:"files"`
	} `json:"metadata"`
}

// media names what came with the post, "" if nothing.
func (p post) media() string {
	switch {
	case len(p.Metadata.Files) > 0:
		return channels.MediaKind(p.Metadata.Files[0].MimeType)
	case len(p.FileIDs) > 0:
		return channels.File
	}
	return ""
}

func (c *Channel) socket(ctx context.Context, handler channels.Handler) error {
	wsURL := strings.Replace(strings.Replace(c.url, "https://", "wss://", 1), "http://", "ws://", 1) + "/api/v4/websocket"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(4 << 20)
	c.backlog.Connected()
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	auth, _ := json.Marshal(map[string]any{"seq": 1, "action": "authentication_challenge", "data": map[string]string{"token": c.token}})
	if err := conn.Write(ctx, websocket.MessageText, auth); err != nil {
		return err
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var ev wsEvent
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		if ev.Status == "OK" || ev.Event == "hello" {
			c.Up()
		}
		if ev.Event != "posted" {
			continue
		}
		var p post
		if json.Unmarshal([]byte(ev.Data.Post), &p) != nil {
			continue
		}
		c.onPost(ctx, p, ev.Data.ChannelType, strings.TrimPrefix(ev.Data.SenderName, "@"), handler)
	}
}

func (c *Channel) onPost(ctx context.Context, p post, channelType, senderName string, handler channels.Handler) {
	c.mu.Lock()
	self, selfName, ownerID := c.selfID, c.selfName, c.ownerID
	c.mu.Unlock()
	kind := p.media()
	if p.UserID == self || p.Props.FromBot == "true" || (strings.TrimSpace(p.Message) == "" && kind == "") {
		return
	}
	if p.CreateAt > 0 && c.backlog.Stale(time.UnixMilli(p.CreateAt)) {
		return
	}
	text := p.Message
	if channelType != "D" {
		if !strings.Contains(text, "@"+selfName) {
			return
		}
		text = strings.TrimSpace(strings.ReplaceAll(text, "@"+selfName, ""))
		if text == "" && kind == "" {
			return
		}
	}
	isOwner := (ownerID != "" && p.UserID == ownerID) || (ownerID == "" && channels.MatchOwner(c.owner, senderName, p.UserID))
	if isOwner && channelType == "D" {
		c.mu.Lock()
		c.ownerChat = p.ChannelID
		if c.ownerID == "" {
			c.ownerID = p.UserID
		}
		c.mu.Unlock()
	}
	if !isOwner && !c.replyToOthers {
		return
	}
	a := c.attachment(ctx, p)
	if a != nil {
		kind = channels.MediaKind(a.Mime)
	}
	handler(ctx, channels.Inbound{Channel: "mattermost", ChatID: p.ChannelID, Sender: "@" + senderName, Text: strings.TrimSpace(text), Media: kind, Attachment: a, IsOwner: isOwner})
}

// Typing shows "typing…" in the channel; clients clear it after a few seconds.
func (c *Channel) Typing(ctx context.Context, chatID string) (time.Duration, error) {
	c.mu.Lock()
	self := c.selfID
	c.mu.Unlock()
	return 4 * time.Second, c.api(ctx, http.MethodPost, "/users/"+self+"/typing", map[string]string{"channel_id": chatID}, nil)
}

// Send posts text to a channel.
func (c *Channel) Send(ctx context.Context, chatID, text string) error {
	return channels.SendParts(text, 16000, func(part string) error {
		return c.api(ctx, http.MethodPost, "/posts", map[string]any{"channel_id": chatID, "message": part}, nil)
	})
}

// SendImage uploads a file and attaches it to a post.
func (c *Channel) SendImage(ctx context.Context, chatID, path, caption string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("channel_id", chatID)
	fw, _ := mw.CreateFormFile("files", filepath.Base(path))
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/api/v4/files", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var up struct {
		Infos []struct {
			ID string `json:"id"`
		} `json:"file_infos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&up); err != nil || len(up.Infos) == 0 {
		return fmt.Errorf("mattermost upload failed: %s", resp.Status)
	}
	return c.api(ctx, http.MethodPost, "/posts", map[string]any{"channel_id": chatID, "message": caption, "file_ids": []string{up.Infos[0].ID}}, nil)
}
