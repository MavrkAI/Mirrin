// Package matrix connects the twin to Matrix using the client-server API
// directly (no libolm; unencrypted rooms only). Make an account for the twin
// on any homeserver, get an access token (Element → Settings → Help & About,
// or `curl -XPOST /_matrix/client/v3/login`), set channels.matrix.user_id and
// MATRIX_ACCESS_TOKEN, and put your own id in owner. The twin opens (or finds)
// an unencrypted DM with you on start and accepts your room invites.
package matrix

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
)

// Channel is the Matrix transport.
type Channel struct {
	channels.Tracker
	backlog           channels.Backlog // tells the replay at connect from live messages
	hs, userID, token string
	owner             string
	replyToOthers     bool
	log               *slog.Logger
	http              *http.Client

	mu        sync.Mutex
	ownerRoom string
	warned    map[string]bool
	txn       int64
}

// New builds the channel.
func New(homeserver, userID, token, owner string, replyToOthers bool, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	return &Channel{hs: strings.TrimRight(homeserver, "/"), userID: strings.TrimSpace(userID), token: token,
		owner: strings.TrimSpace(owner), replyToOthers: replyToOthers, log: log,
		http: &http.Client{Timeout: 50 * time.Second}, warned: map[string]bool{}}
}

func (c *Channel) Name() string { return "matrix" }

// ChatLink opens the DM room via matrix.to.
func (c *Channel) ChatLink() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ownerRoom == "" {
		return "", ""
	}
	return "Open the room", "https://matrix.to/#/" + url.PathEscape(c.ownerRoom)
}

// OwnerChatID is the DM room with the owner once known.
func (c *Channel) OwnerChatID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ownerRoom
}

func (c *Channel) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.hs+path, rd)
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
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == 429 {
		var rl struct {
			Retry int `json:"retry_after_ms"`
		}
		_ = json.Unmarshal(data, &rl)
		time.Sleep(time.Duration(rl.Retry+500) * time.Millisecond)
		return c.do(ctx, method, path, body, out)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Code string `json:"errcode"`
			Err  string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if resp.StatusCode == http.StatusUnauthorized {
			return channels.Fatal(fmt.Errorf("the homeserver doesn't accept the twin's access token (%s); sign in again from Channels", e.Code))
		}
		return fmt.Errorf("matrix %s %s: %d %s %s", method, path, resp.StatusCode, e.Code, e.Err)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type syncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events []event `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
		Invite map[string]struct {
			State struct {
				Events []event `json:"events"`
			} `json:"invite_state"`
		} `json:"invite"`
	} `json:"rooms"`
}

type event struct {
	Type     string          `json:"type"`
	Sender   string          `json:"sender"`
	StateKey *string         `json:"state_key"`
	TS       int64           `json:"origin_server_ts"`
	Content  json.RawMessage `json:"content"`
}

// Start syncs until ctx ends.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if c.token == "" {
		return channels.Fatal(errors.New("matrix access token missing (set MATRIX_ACCESS_TOKEN)"))
	}
	var who struct {
		UserID string `json:"user_id"`
	}
	if err := c.do(ctx, http.MethodGet, "/_matrix/client/v3/account/whoami", nil, &who); err != nil {
		c.Retrying(err)
		return err
	}
	if who.UserID != "" {
		c.userID = who.UserID
	}
	c.log.Info("matrix connected", "user", c.userID)
	c.findOwnerRoom(ctx)
	// Initial sync with no timeout just to get a position; skip the backlog.
	var first syncResponse
	if err := c.do(ctx, http.MethodGet, "/_matrix/client/v3/sync?timeout=0&filter="+url.QueryEscape(`{"room":{"timeline":{"limit":1}}}`), nil, &first); err != nil {
		c.Retrying(err)
		return err
	}
	c.Up()
	c.backlog.ConnectedUntilDone()
	since := first.NextBatch
	backoff := channels.Backoff{Min: time.Second, Max: 30 * time.Second}
	for ctx.Err() == nil {
		var res syncResponse
		err := c.do(ctx, http.MethodGet, "/_matrix/client/v3/sync?timeout=30000&since="+url.QueryEscape(since), nil, &res)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if channels.IsFatal(err) {
				c.Down(err)
				return err
			}
			c.Retrying(err)
			c.log.Warn("matrix sync", "err", err)
			if !backoff.Wait(ctx, time.Time{}) {
				return nil
			}
			continue
		}
		c.Up()
		backoff.Reset()
		since = res.NextBatch
		c.handleSync(ctx, res, handler)
		c.backlog.Done() // the first sync after the position holds the catch-up
	}
	return nil
}

// findOwnerRoom looks in m.direct for an existing DM with the owner, else creates one.
func (c *Channel) findOwnerRoom(ctx context.Context) {
	var direct map[string][]string
	if err := c.do(ctx, http.MethodGet, "/_matrix/client/v3/user/"+url.PathEscape(c.userID)+"/account_data/m.direct", nil, &direct); err == nil {
		if rooms := direct[c.owner]; len(rooms) > 0 {
			c.mu.Lock()
			c.ownerRoom = rooms[0]
			c.mu.Unlock()
			return
		}
	}
	var created struct {
		RoomID string `json:"room_id"`
	}
	err := c.do(ctx, http.MethodPost, "/_matrix/client/v3/createRoom", map[string]any{
		"is_direct": true, "invite": []string{c.owner}, "preset": "trusted_private_chat",
	}, &created)
	if err != nil {
		c.log.Warn("matrix create DM", "err", err)
		return
	}
	if direct == nil {
		direct = map[string][]string{}
	}
	direct[c.owner] = append(direct[c.owner], created.RoomID)
	_ = c.do(ctx, http.MethodPut, "/_matrix/client/v3/user/"+url.PathEscape(c.userID)+"/account_data/m.direct", direct, nil)
	c.mu.Lock()
	c.ownerRoom = created.RoomID
	c.mu.Unlock()
	c.log.Info("matrix DM created; accept the invite", "room", created.RoomID)
}

func (c *Channel) handleSync(ctx context.Context, res syncResponse, handler channels.Handler) {
	for roomID, inv := range res.Rooms.Invite {
		inviter := ""
		for _, ev := range inv.State.Events {
			if ev.Type == "m.room.member" && ev.StateKey != nil && *ev.StateKey == c.userID {
				inviter = ev.Sender
			}
		}
		if channels.MatchOwner(c.owner, inviter) || c.replyToOthers {
			if err := c.do(ctx, http.MethodPost, "/_matrix/client/v3/join/"+url.PathEscape(roomID), map[string]any{}, nil); err != nil {
				c.log.Warn("matrix join", "room", roomID, "err", err)
			}
		}
	}
	for roomID, room := range res.Rooms.Join {
		for _, ev := range room.Timeline.Events {
			c.onEvent(ctx, roomID, ev, handler)
		}
	}
}

func (c *Channel) onEvent(ctx context.Context, roomID string, ev event, handler channels.Handler) {
	if ev.Sender == c.userID {
		return
	}
	isOwner := channels.MatchOwner(c.owner, ev.Sender)
	if ev.Type == "m.room.encrypted" {
		c.mu.Lock()
		w := c.warned[roomID]
		c.warned[roomID] = true
		c.mu.Unlock()
		if !w && isOwner {
			_ = c.Send(ctx, roomID, "This room is encrypted and I can't read it yet. Start an unencrypted DM with me (turn off encryption when creating the room) and I'll answer there.")
		}
		return
	}
	if ev.Type != "m.room.message" {
		return
	}
	if ev.TS > 0 && c.backlog.Stale(time.UnixMilli(ev.TS)) {
		return
	}
	var content mediaContent
	if err := json.Unmarshal(ev.Content, &content); err != nil {
		return
	}
	text, kind := strings.TrimSpace(content.Body), ""
	switch content.MsgType {
	case "m.text":
	case "m.image":
		kind = channels.Photo
	case "m.audio":
		kind = channels.Voice
	case "m.video":
		kind = channels.Video
	case "m.file":
		kind = channels.File
	default:
		return // notices from bots, emotes, locations
	}
	if kind != "" && (content.Filename == "" || content.Filename == content.Body) {
		text = "" // no caption: the body is just the file name
	}
	if text == "" && kind == "" {
		return
	}
	if !isOwner && !c.replyToOthers {
		return
	}
	if isOwner {
		c.mu.Lock()
		if c.ownerRoom == "" {
			c.ownerRoom = roomID
		}
		c.mu.Unlock()
	}
	handler(ctx, channels.Inbound{Channel: "matrix", ChatID: roomID, Sender: ev.Sender, Text: text, Media: kind, Attachment: c.attachment(content), IsOwner: isOwner})
}

// Typing shows "typing…" in the room for a few seconds.
func (c *Channel) Typing(ctx context.Context, roomID string) (time.Duration, error) {
	return 8 * time.Second, c.typing(ctx, roomID, map[string]any{"typing": true, "timeout": 10000})
}

// EndTyping clears the indicator once the reply is sent.
func (c *Channel) EndTyping(ctx context.Context, roomID string) error {
	return c.typing(ctx, roomID, map[string]any{"typing": false})
}

func (c *Channel) typing(ctx context.Context, roomID string, body map[string]any) error {
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/typing/" + url.PathEscape(c.userID)
	return c.do(ctx, http.MethodPut, path, body, nil)
}

func (c *Channel) sendEvent(ctx context.Context, roomID string, content any) error {
	c.mu.Lock()
	c.txn++
	txn := strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatInt(c.txn, 10)
	c.mu.Unlock()
	return c.do(ctx, http.MethodPut, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/send/m.room.message/"+txn, content, nil)
}

// Send posts a text message to a room.
func (c *Channel) Send(ctx context.Context, chatID, text string) error {
	return channels.SendParts(text, 8000, func(part string) error {
		return c.sendEvent(ctx, chatID, map[string]any{"msgtype": "m.text", "body": part})
	})
}

// SendImage uploads a file to the media repo and posts it.
func (c *Channel) SendImage(ctx context.Context, chatID, path, caption string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	mime := "image/png"
	if ext := strings.ToLower(filepath.Ext(path)); ext == ".jpg" || ext == ".jpeg" {
		mime = "image/jpeg"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.hs+"/_matrix/media/v3/upload?filename="+url.QueryEscape(filepath.Base(path)), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mime)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var up struct {
		URI string `json:"content_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&up); err != nil || up.URI == "" {
		return fmt.Errorf("matrix upload failed: %s", resp.Status)
	}
	if err := c.sendEvent(ctx, chatID, map[string]any{"msgtype": "m.image", "body": filepath.Base(path), "url": up.URI, "info": map[string]any{"mimetype": mime, "size": len(data)}}); err != nil {
		return err
	}
	if caption != "" {
		return c.Send(ctx, chatID, caption)
	}
	return nil
}

// Login exchanges a password for an access token (m.login.password).
func Login(ctx context.Context, homeserver, userID, password string) (token, resolvedUser string, err error) {
	body, _ := json.Marshal(map[string]any{
		"type":                        "m.login.password",
		"identifier":                  map[string]string{"type": "m.id.user", "user": userID},
		"password":                    password,
		"initial_device_display_name": "Mirrin",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(homeserver, "/")+"/_matrix/client/v3/login", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var res struct {
		Token  string `json:"access_token"`
		UserID string `json:"user_id"`
		Err    string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if resp.StatusCode != 200 || res.Token == "" {
		if res.Err == "" {
			res.Err = resp.Status
		}
		return "", "", fmt.Errorf("matrix login: %s", res.Err)
	}
	return res.Token, res.UserID, nil
}
