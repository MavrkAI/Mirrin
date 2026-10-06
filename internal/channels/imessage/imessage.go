//go:build darwin

// Package imessage connects Mirrin to iMessage on a Mac. Recommended setup:
// sign the Mac's Messages app into a dedicated Apple ID for Mirrin, and set
// owner to your own phone number or email. The daemon reads new messages
// from the Messages database (needs Full Disk Access for mirrin) and
// replies through the Messages app.
package imessage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Channel is the iMessage transport.
type Channel struct {
	channels.Tracker
	owner         string
	replyToOthers bool
	log           *slog.Logger
	db            *sql.DB
}

// New builds the channel. owner is a phone number (E.164) or email handle.
func New(owner string, replyToOthers bool, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	return &Channel{owner: strings.TrimSpace(owner), replyToOthers: replyToOthers, log: log}
}

func (c *Channel) Name() string        { return "imessage" }
func (c *Channel) OwnerChatID() string { return c.owner }

func dbPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Messages", "chat.db")
}

// Start polls the Messages database until ctx ends.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	db, err := sql.Open("sqlite", "file:"+dbPath()+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return err
	}
	c.db = db
	defer db.Close()
	var last int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(ROWID),0) FROM message`).Scan(&last); err != nil {
		// Not Fatal: once access is granted the next attempt gets in by itself.
		err = fmt.Errorf("can't read the Messages database; allow Full Disk Access for mirrin in System Settings → Privacy & Security (%w)", err)
		c.Retrying(err)
		return err
	}
	c.Up()
	c.log.Info("imessage connected", "owner", c.owner)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		rows, err := poll(ctx, db, last)
		if err != nil {
			c.Retrying(err)
			c.log.Warn("imessage poll", "err", err)
			continue
		}
		c.Up()
		for _, r := range rows {
			last = r.ID
			text, kind, ok := r.message()
			if !ok {
				continue
			}
			isOwner := normalize(r.Handle) == normalize(c.owner)
			if !isOwner && !c.replyToOthers {
				continue
			}
			handler(ctx, channels.Inbound{Channel: "imessage", ChatID: r.Handle, Sender: r.Handle, Text: text, Media: kind, Attachment: r.attachment(), IsOwner: isOwner})
		}
	}
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.Contains(s, "@") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Send delivers text through the Messages app.
func (c *Channel) Send(ctx context.Context, chatID, text string) error {
	script := fmt.Sprintf(`tell application "Messages"
	set targetService to 1st account whose service type = iMessage
	set targetBuddy to participant %q of targetService
	send %q to targetBuddy
end tell`, chatID, text)
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("imessage send: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
