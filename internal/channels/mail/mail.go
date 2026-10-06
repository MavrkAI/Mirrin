// Package mail lets the owner talk to the twin by email: send a message to
// the twin's mailbox (the one under skills.email) and the reply comes back
// threaded. Polls IMAP; no inbound server needed. Works with any provider,
// which makes it the connector of last resort for phones, cars and watches.
// Mail from the owner's address counts as the owner only when the mailbox
// provider confirmed it came from them (see auth.go).
package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/skills/email"
	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
)

// Mailer is the part of the email skill the channel needs.
type Mailer interface {
	UnreadAfter(mark email.Mark, limit int) ([]email.Incoming, email.Mark, error)
	MarkRead(uids ...uint32) error
	AuthServers() (servers []string, guessed bool)
	Reply(to, subject, body, inReplyTo string) error
	// SelfSent reports whether mail in the mailbox's own address was sent
	// from the mailbox itself (its copy is in Sent), for mail between a
	// provider's own users that carries no header vouching for it.
	SelfSent(m email.Incoming) (bool, error)
}

// Channel is the email transport.
type Channel struct {
	mail          Mailer
	owner         string
	replyToOthers bool
	tag           string
	every         time.Duration
	log           *slog.Logger
	trusted       []string // receiving servers that vouch for senders
	guessed       bool     // trusted was worked out from imap_host

	mu      sync.Mutex
	threads map[string]thread // chat id (sender address) → last subject / message id
	ignored string            // why the last mail in the owner's name was ignored, until one is verified
}

type thread struct {
	subject   string
	messageID string
}

// New builds the channel. tag, if set, must appear in the subject ("[Mirrin]").
func New(m Mailer, owner string, replyToOthers bool, tag string, every time.Duration, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	if every <= 0 {
		every = 30 * time.Second
	}
	c := &Channel{mail: m, owner: strings.ToLower(strings.TrimSpace(owner)), replyToOthers: replyToOthers,
		tag: strings.TrimSpace(tag), every: every, log: log, threads: map[string]thread{}}
	if m != nil {
		c.trusted, c.guessed = m.AuthServers()
	}
	return c
}

func (c *Channel) Name() string { return "mail" }

// OwnerChatID is the owner's address.
func (c *Channel) OwnerChatID() string { return c.owner }

// Start polls the inbox until ctx ends. Only mail that arrives after start
// is considered, and only mail the twin answers is marked read; everything
// else in the mailbox is left exactly as it was.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if c.mail == nil {
		return channels.Fatal(errors.New("mail channel needs skills.email configured"))
	}
	_, mark, err := c.mail.UnreadAfter(email.Mark{}, 0)
	if err != nil {
		return err
	}
	c.log.Info("mail channel connected", "owner", c.owner, "trusted", c.trusted, "guessed", c.guessed)
	if w := c.Warning(); w != "" {
		c.log.Warn("mail: " + w)
	}
	t := time.NewTicker(c.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			msgs, next, err := c.mail.UnreadAfter(mark, 20)
			if err != nil {
				c.log.Warn("mail poll", "err", err)
				continue
			}
			mark = next
			for _, m := range msgs {
				c.deliver(ctx, m, handler)
			}
		}
	}
}

// deliver hands one message to the twin if it is for the twin.
func (c *Channel) deliver(ctx context.Context, m email.Incoming, handler channels.Handler) {
	in, ok := c.accept(m)
	if !ok {
		return
	}
	if err := c.mail.MarkRead(m.UID); err != nil {
		c.log.Warn("mail mark read", "err", err)
	}
	handler(ctx, in)
}

// accept decides whether a message is for the twin and from whom.
func (c *Channel) accept(m email.Incoming) (channels.Inbound, bool) {
	from := strings.ToLower(strings.TrimSpace(m.From))
	if from == "" || strings.Contains(strings.ToLower(m.Subject), "auto-reply") || strings.Contains(strings.ToLower(m.Subject), "out of office") {
		return channels.Inbound{}, false
	}
	isOwner := from == c.owner && vouched(m.AuthResults, c.trusted, c.owner, sealed(m.Trace, c.trusted))
	if from == c.owner && !isOwner && !dmarcFailed(m.AuthResults, c.trusted) {
		isOwner = c.selfSent(m) // a DMARC fail is a forgery: no mailbox look
	}
	if from == c.owner && !isOwner {
		// Someone may be forging the owner's address. It shares the owner's
		// conversation, so it isn't answered even as a stranger.
		why := c.whyIgnored(m)
		c.mu.Lock()
		c.ignored = why
		c.mu.Unlock()
		c.log.Warn("mail: "+why, "trusted", c.trusted)
		return channels.Inbound{}, false
	}
	if isOwner {
		c.mu.Lock()
		c.ignored = ""
		c.mu.Unlock()
	}
	if !isOwner && !c.replyToOthers {
		return channels.Inbound{}, false
	}
	if c.tag != "" && !strings.Contains(strings.ToLower(m.Subject), strings.ToLower(c.tag)) {
		return channels.Inbound{}, false
	}
	if strings.TrimSpace(m.Body) == email.Unreadable {
		// The server wouldn't give the text: there is nothing to answer,
		// and it stays unread for the owner to open.
		c.log.Warn("mail: skipped a message whose text couldn't be read", "uid", m.UID)
		return channels.Inbound{}, false
	}
	body, _, _ := mailtext.Clean(m.Body)
	if body == "" {
		body = strings.TrimSpace(m.Subject)
	}
	if body == "" {
		return channels.Inbound{}, false
	}
	c.mu.Lock()
	c.threads[from] = thread{subject: m.Subject, messageID: m.MessageID}
	c.mu.Unlock()
	sender := m.FromName
	if sender == "" {
		sender = from
	}
	return channels.Inbound{Channel: "mail", ChatID: from, Sender: sender, Text: body, IsOwner: isOwner}, true
}

// Warning is what the owner should fix, for the Channels page: mail in
// their name that can't be checked, or was ignored ("" when all is well).
func (c *Channel) Warning() string {
	if len(c.trusted) == 0 {
		return "Mail in your name can't be checked on this mailbox, so none of it is treated as you. " +
			"Set skills.email.auth_servers to the server named first in the Authentication-Results header of a message you sent it."
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ignored
}

// whyIgnored explains, in words the owner can act on, why mail in their name
// didn't count as theirs.
func (c *Channel) whyIgnored(m email.Incoming) string {
	subject := []rune(strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, m.Subject))
	if len(subject) > 60 {
		subject = append(subject[:60], '…')
	}
	what := fmt.Sprintf("Ignored mail in your name (%q)", string(subject))
	top, ok := topServer(m.AuthResults)
	switch {
	case len(c.trusted) == 0:
		return what + " because this mailbox can't check who sent it. Set skills.email.auth_servers to the server named first in its Authentication-Results header."
	case !ok:
		return what + " because it carries no Authentication-Results header, so there's no way to tell it came from you."
	case trustedServer(top, c.trusted) < 0:
		name := top
		if name == "" {
			name = `"" (a header with no server name, as Microsoft writes)`
		}
		return what + " because it was checked by " + name + ", which isn't trusted yet. If that's your mail provider, add it to skills.email.auth_servers."
	}
	return what + " because your provider didn't confirm it came from you (no SPF, DKIM or DMARC pass). If you sent it, send from your own account rather than through another service."
}

// Send replies in the last thread from chatID (or starts one).
func (c *Channel) Send(_ context.Context, chatID, text string) error {
	c.mu.Lock()
	th := c.threads[chatID]
	c.mu.Unlock()
	subject := th.subject
	if subject == "" {
		subject = firstLine(text)
		if c.tag != "" {
			subject = c.tag + " " + subject
		}
	} else if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	return c.mail.Reply(chatID, subject, text, th.messageID)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n."); i > 0 {
		s = s[:i]
	}
	if len(s) > 70 {
		s = s[:70]
	}
	return s
}

// selfSent asks the mailbox whether mail in the owner's name was sent from
// the mailbox itself; a failed look counts as no.
func (c *Channel) selfSent(m email.Incoming) bool {
	ok, err := c.mail.SelfSent(m)
	if err != nil {
		c.log.Warn("mail: couldn't look for the message in Sent", "err", err)
		return false
	}
	return ok
}
