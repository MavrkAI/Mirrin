//go:build !nowhatsapp

// Package whatsapp connects Mirrin to the owner's WhatsApp account via the
// multi-device protocol (whatsmeow). Pair once with a QR code; from then on
// Mirrin reads and replies as a linked device.
package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// Channel is the WhatsApp transport. It reports its real connection state
// (channels.Tracker): connecting until WhatsApp accepts the link, then
// connected, and stopped with what to do when the phone unlinks it.
type Channel struct {
	channels.Tracker
	backlog       channels.Backlog // tells the replay at connect from live messages
	dataDir       string
	owner         string // digits only
	replyToOthers bool
	log           *slog.Logger

	mu     sync.Mutex
	client *whatsmeow.Client
	// download fetches an attachment: the client's, or a stand-in in tests.
	download func(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
}

// New builds the channel. owner is the principal's phone number in E.164.
func New(dataDir, owner string, replyToOthers bool, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	return &Channel{dataDir: dataDir, owner: digits(owner), replyToOthers: replyToOthers, log: log,
		backlog: channels.Backlog{Window: time.Minute}}
}

func (c *Channel) Name() string { return "whatsapp" }

// OwnerChatID is the owner's personal chat.
func (c *Channel) OwnerChatID() string {
	return types.NewJID(c.owner, types.DefaultUserServer).String()
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cl is the client, or nil before open or after pairing handed it back.
func (c *Channel) cl() *whatsmeow.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client
}

func (c *Channel) setClient(cl *whatsmeow.Client) {
	c.mu.Lock()
	c.client = cl
	c.mu.Unlock()
}

// connected is the client while it is connected, or an error saying it isn't.
func (c *Channel) connected() (*whatsmeow.Client, error) {
	cl := c.cl()
	if cl == nil || !cl.IsConnected() {
		return nil, errors.New("whatsapp not connected")
	}
	return cl, nil
}

// dbPath is where the linked device's keys live. The file: URI escapes the
// path, so a data folder with '#', '?' or '%' in its name opens this file
// and no other.
func dbPath(dataDir string) string { return filepath.Join(dataDir, "whatsapp.db") }

func (c *Channel) open(ctx context.Context) error {
	if c.cl() != nil {
		return nil
	}
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return err
	}
	dsn := memory.FileURI(dbPath(c.dataDir)) + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	if err := container.Upgrade(ctx); err != nil {
		db.Close()
		return fmt.Errorf("whatsapp store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		db.Close()
		return err
	}
	c.setClient(whatsmeow.NewClient(device, waLog.Noop))
	return nil
}

// Login pairs this machine as a linked device by printing a QR code.
// name is the twin's display name for the confirmation line.
func (c *Channel) Login(ctx context.Context, name string) error {
	if name == "" {
		name = "Mirrin"
	}
	if err := c.open(ctx); err != nil {
		return err
	}
	cl := c.cl()
	if cl.Store.ID != nil {
		fmt.Printf("Already paired as %s.\n", cl.Store.ID.User)
		return nil
	}
	qrChan, err := cl.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := cl.Connect(); err != nil {
		return err
	}
	fmt.Println("Open WhatsApp on your phone → Linked devices → Link a device, then scan:")
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
		case "success":
			fmt.Printf("Paired. %s can now read and send WhatsApp messages.\n", name)
			return nil
		case "timeout":
			return errors.New("QR code timed out; run login again")
		}
	}
	return nil
}

// Why the channel stopped or is reconnecting, in the owner's words. What
// stops it for good says what to do on the Channels page.
var (
	errNotPaired = errors.New("WhatsApp isn't linked yet; open Channels from the menu and press Pair by scanning a QR code")
	errLoggedOut = errors.New("WhatsApp unlinked this computer; to link it again, open Channels and press Pair by scanning a QR code")
	errReplaced  = errors.New("WhatsApp is using this link on another computer (is the twin running twice?); stop the other one, then press Reconnect on Channels")
	errOutdated  = errors.New("WhatsApp says this version of Mirrin is too old to connect; update Mirrin, then press Reconnect on Channels")
	errDropped   = errors.New("the connection to WhatsApp dropped")
	errNoAnswer  = errors.New("WhatsApp isn't answering; is this computer online?")
)

// Start connects and streams messages to handler. It returns when ctx ends,
// or when WhatsApp ends the link in a way reconnecting can't fix (the phone
// unlinked this computer): then with a Fatal error saying what to do.
// Ordinary drops are healed by whatsmeow itself and only reported.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if err := c.open(ctx); err != nil {
		c.Retrying(err)
		return err
	}
	cl := c.cl()
	if cl.Store.ID == nil {
		err := channels.Fatal(errNotPaired)
		c.Down(err)
		return err
	}
	stop := make(chan error, 1)
	id := cl.AddEventHandler(func(evt any) { c.onEvent(ctx, evt, handler, stop) })
	defer cl.RemoveEventHandler(id)
	if err := cl.Connect(); err != nil {
		c.Retrying(err)
		return err
	}
	select {
	case <-ctx.Done():
		cl.Disconnect()
		return nil
	case err := <-stop:
		cl.Disconnect()
		return c.stopped(err)
	}
}

// stopped records why Start is returning: for good (Fatal, shown on the
// Channels page until the owner acts) or to be retried.
func (c *Channel) stopped(err error) error {
	if channels.IsFatal(err) {
		c.Down(err)
	} else {
		c.Retrying(err)
	}
	c.log.Warn("whatsapp stopped", "err", err)
	return err
}

// onEvent keeps the reported state true to the connection, hands messages
// on, and stops Start (through stop) when WhatsApp won't take the link back.
func (c *Channel) onEvent(ctx context.Context, evt any, handler channels.Handler, stop chan<- error) {
	halt := func(err error) {
		select {
		case stop <- err:
		default: // already stopping
		}
	}
	switch v := evt.(type) {
	case *events.Message:
		c.onMessage(ctx, v, handler)
	case *events.Connected:
		c.Up()
		c.backlog.ConnectedUntilDone()
		if cl := c.cl(); cl != nil && cl.Store.ID != nil {
			c.log.Info("whatsapp connected", "as", cl.Store.ID.User)
		}
	case *events.OfflineSyncCompleted:
		c.backlog.Done()
	case *events.Disconnected:
		c.Retrying(errDropped) // whatsmeow reconnects by itself
	case *events.KeepAliveTimeout:
		if v.ErrorCount >= 2 {
			c.Retrying(errNoAnswer)
		}
	case *events.KeepAliveRestored:
		c.Up()
	case *events.LoggedOut:
		halt(channels.Fatal(errLoggedOut))
	case *events.StreamReplaced:
		halt(channels.Fatal(errReplaced))
	case *events.ClientOutdated:
		halt(channels.Fatal(errOutdated))
	case *events.TemporaryBan:
		when := "for now"
		if v.Expire > 0 {
			when = "for " + about(v.Expire)
		}
		halt(channels.Fatal(fmt.Errorf("WhatsApp has blocked this account from linked devices %s (%s); press Reconnect on Channels once it lifts", when, banReason(v.Code))))
	case *events.ConnectFailure:
		halt(fmt.Errorf("WhatsApp refused the connection (%s)", v.Reason))
	case *events.CATRefreshError:
		halt(errors.New("WhatsApp refused the connection"))
	}
}

// about says a length of time the way a person would: "about 20 minutes",
// "about 2 hours", "about 3 days".
func about(d time.Duration) string {
	d = d.Round(time.Minute)
	n, unit := int(d/time.Minute), "minute"
	switch {
	case d >= 36*time.Hour:
		n, unit = int((d+12*time.Hour)/(24*time.Hour)), "day"
	case d >= time.Hour:
		n, unit = int((d+30*time.Minute)/time.Hour), "hour"
	}
	n = max(n, 1)
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("about %d %s", n, unit)
}

// banReason is WhatsApp's reason for a ban, without its number ("too many
// people blocked you").
func banReason(code events.TempBanReason) string {
	s := code.String()
	if _, reason, ok := strings.Cut(s, ": "); ok {
		return reason
	}
	return s
}

func (c *Channel) onMessage(ctx context.Context, m *events.Message, handler channels.Handler) {
	if in, ok := c.inbound(m, c.selfJIDs()); ok {
		handler(ctx, in)
	}
}

// inbound is the message as the twin should hear it, and whether it should
// hear it at all. self is the linked account's own addresses.
func (c *Channel) inbound(m *events.Message, self []types.JID) (channels.Inbound, bool) {
	if m.Info.IsGroup || m.Info.Chat.Server == types.BroadcastServer {
		return channels.Inbound{}, false
	}
	text := extractText(m.Message)
	kind, att := c.attachment(m.Message)
	if text == "" && kind == "" {
		return channels.Inbound{}, false // a sticker, a reaction, a receipt
	}
	senderUser := m.Info.Sender.User
	if m.Info.Sender.Server != types.DefaultUserServer {
		// LID addressing: fall back to the resolved phone-number sender when available.
		if alt := m.Info.SenderAlt; !alt.IsEmpty() {
			senderUser = alt.User
		}
	}
	if m.Info.IsFromMe && !ownChat(m.Info.MessageSource, self) {
		return channels.Inbound{}, false // the account talking to someone else, not to the twin
	}
	isOwner := m.Info.IsFromMe || senderUser == c.owner
	if !isOwner && !c.replyToOthers {
		return channels.Inbound{}, false
	}
	// Ignore history replayed at connect so restarts don't answer it.
	if c.backlog.Stale(m.Info.Timestamp) {
		return channels.Inbound{}, false
	}
	chat := m.Info.Chat.String()
	if isOwner {
		chat = c.OwnerChatID()
	}
	return channels.Inbound{
		Channel:    "whatsapp",
		ChatID:     chat,
		Sender:     senderUser,
		Text:       text,
		Media:      kind,
		Attachment: att,
		IsOwner:    isOwner,
	}, true
}

// selfJIDs are the linked account's own addresses, by phone number and by LID.
func (c *Channel) selfJIDs() []types.JID {
	cl := c.cl()
	if cl == nil || cl.Store == nil {
		return nil
	}
	var self []types.JID
	if id := cl.Store.ID; id != nil {
		self = append(self, *id)
	}
	if lid := cl.Store.GetLID(); !lid.IsEmpty() {
		self = append(self, lid)
	}
	return self
}

// ownChat reports whether a message sits in the account's "message yourself"
// chat. A linked device also receives everything the account sends to other
// people, and those messages are not addressed to the twin.
func ownChat(src types.MessageSource, self []types.JID) bool {
	for _, j := range self {
		if src.Chat.User == j.User || (!src.RecipientAlt.IsEmpty() && src.RecipientAlt.User == j.User) {
			return true
		}
	}
	return false
}

// extractText is what was typed: the message, or the caption of a photo,
// video or document (what came with it is Inbound.Media).
func extractText(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	if t := m.GetConversation(); t != "" {
		return t
	}
	if e := m.GetExtendedTextMessage(); e != nil && e.GetText() != "" {
		return e.GetText()
	}
	if i := m.GetImageMessage(); i != nil {
		return i.GetCaption()
	}
	if v := m.GetVideoMessage(); v != nil {
		return v.GetCaption()
	}
	if d := m.GetDocumentMessage(); d != nil {
		return d.GetCaption()
	}
	return ""
}

// Send delivers a text message.
func (c *Channel) Send(ctx context.Context, chatID, text string) error {
	cl, err := c.connected()
	if err != nil {
		return err
	}
	jid, err := types.ParseJID(chatID)
	if err != nil {
		return err
	}
	_, err = cl.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(text)})
	return err
}

// Typing shows "typing…" in the chat while a reply is worked on. WhatsApp
// lets it fade unless it is refreshed, and clears it when the reply arrives.
// In the account's own "message yourself" chat (the owner's, when the twin
// is linked to their own number) nobody would see it, so nothing is sent.
func (c *Channel) Typing(ctx context.Context, chatID string) (time.Duration, error) {
	if c.isSelfChat(chatID) {
		return 0, nil
	}
	return 8 * time.Second, c.chatPresence(ctx, chatID, types.ChatPresenceComposing)
}

// EndTyping clears the indicator straight away (the reply didn't come).
func (c *Channel) EndTyping(ctx context.Context, chatID string) error {
	if c.isSelfChat(chatID) {
		return nil
	}
	return c.chatPresence(ctx, chatID, types.ChatPresencePaused)
}

// isSelfChat reports whether chatID is the linked account's own chat.
func (c *Channel) isSelfChat(chatID string) bool {
	jid, err := types.ParseJID(chatID)
	if err != nil {
		return false
	}
	for _, self := range c.selfJIDs() {
		if jid.User == self.User {
			return true
		}
	}
	return false
}

func (c *Channel) chatPresence(ctx context.Context, chatID string, state types.ChatPresence) error {
	cl, err := c.connected()
	if err != nil {
		return err
	}
	jid, err := types.ParseJID(chatID)
	if err != nil {
		return err
	}
	return cl.SendChatPresence(ctx, jid, state, types.ChatPresenceMediaText)
}

// SendImage uploads a PNG and sends it as an image message.
func (c *Channel) SendImage(ctx context.Context, chatID, path, caption string) error {
	cl, err := c.connected()
	if err != nil {
		return err
	}
	jid, err := types.ParseJID(chatID)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	up, err := cl.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return err
	}
	msg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		Mimetype:      proto.String("image/png"),
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
		Caption:       proto.String(caption),
	}}
	_, err = cl.SendMessage(ctx, jid, msg)
	return err
}

// Paired reports whether a linked device is stored.
func Paired(dataDir string) bool {
	path := dbPath(dataDir)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	db, err := sql.Open("sqlite", memory.FileURI(path)+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("select count(*) from whatsmeow_device").Scan(&n); err != nil {
		return false
	}
	return n > 0
}
