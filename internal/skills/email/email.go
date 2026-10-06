// Package email gives Mirrin an inbox over IMAP and an outbox over SMTP. It
// works with Gmail (app password), Fastmail, iCloud, Outlook and any
// standards-compliant provider.
package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Client holds mailbox settings.
type Client struct {
	cfg      config.Email
	password string
	loc      *time.Location
	// dial opens the IMAP connection and smtpSend sends; nil means the
	// configured servers. Tests use local ones.
	dial     func(opts *imapclient.Options) (*imapclient.Client, error)
	smtpSend func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
	validity atomic.Uint32 // the inbox's UIDVALIDITY at the last Snapshot
}

// snapshotMax caps how many unread emails the watcher tracks.
const snapshotMax = 100

// readMax is how much of an email's text the model is given.
const readMax = 20000

// New builds a client.
func New(cfg config.Email, password string, loc *time.Location) *Client {
	if loc == nil {
		loc = time.Local
	}
	return &Client{cfg: cfg, password: password, loc: loc}
}

func (c *Client) imap() (*imapclient.Client, error) {
	// Subjects and names in any charset, not just UTF-8 and Latin-1.
	opts := &imapclient.Options{WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader}}
	var cl *imapclient.Client
	var err error
	if c.dial != nil {
		cl, err = c.dial(opts)
	} else {
		cl, err = imapclient.DialTLS(net.JoinHostPort(c.cfg.IMAPHost, fmt.Sprint(c.cfg.IMAPPort)), opts)
	}
	if err != nil {
		return nil, fmt.Errorf("imap connect: %w", err)
	}
	if err := cl.Login(c.cfg.Username, c.password).Wait(); err != nil {
		cl.Close()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	return cl, nil
}

// Tools returns list_emails / read_email / send_email.
func (c *Client) Tools() []tools.Tool {
	return []tools.Tool{
		tools.New("list_emails", "List the most recent emails in the inbox (newest first) with sender, subject, date and uid.",
			tools.Schema(map[string]tools.Prop{
				"count":  {Type: "integer", Description: "How many (default 15, max 50)"},
				"unseen": {Type: "boolean", Description: "Only unread messages"},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					Count  int
					Unseen bool
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if in.Count <= 0 {
					in.Count = 15
				}
				if in.Count > 50 {
					in.Count = 50
				}
				cl, err := c.imap()
				if err != nil {
					return "", err
				}
				defer cl.Close()
				sel, err := cl.Select("INBOX", nil).Wait()
				if err != nil {
					return "", err
				}
				if sel.NumMessages == 0 {
					return "inbox is empty", nil
				}
				var set imap.NumSet
				if in.Unseen {
					res, err := cl.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
					if err != nil {
						return "", err
					}
					uids := res.AllUIDs()
					if len(uids) == 0 {
						return "no unread emails", nil
					}
					if len(uids) > in.Count {
						uids = uids[len(uids)-in.Count:]
					}
					set = imap.UIDSetNum(uids...)
				} else {
					from := uint32(1)
					if sel.NumMessages > uint32(in.Count) {
						from = sel.NumMessages - uint32(in.Count) + 1
					}
					var ss imap.SeqSet
					ss.AddRange(from, sel.NumMessages)
					set = ss
				}
				msgs, err := cl.Fetch(set, &imap.FetchOptions{Envelope: true, UID: true, Flags: true}).Collect()
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for i := len(msgs) - 1; i >= 0; i-- {
					m := msgs[i]
					if m.Envelope == nil {
						continue
					}
					from := "?"
					if len(m.Envelope.From) > 0 {
						a := m.Envelope.From[0]
						from = a.Name
						if from == "" {
							from = a.Addr()
						} else {
							from += " <" + a.Addr() + ">"
						}
					}
					flag := ""
					if !hasFlag(m.Flags, imap.FlagSeen) {
						flag = " [unread]"
					}
					fmt.Fprintf(&b, "uid %d | %s | %s | %s%s\n", m.UID, m.Envelope.Date.In(c.loc).Format("Mon 2 Jan 15:04"), from, m.Envelope.Subject, flag)
				}
				return b.String(), nil
			}),
		tools.New("read_email", "Read one email by uid: who it's from, to whom, when, the text (earlier messages quoted under a reply are left out unless full is true) and the names and sizes of any attachments.",
			tools.Schema(map[string]tools.Prop{
				"uid":  {Type: "integer", Description: "uid from list_emails", Required: true},
				"full": {Type: "boolean", Description: "Include the quoted earlier messages and signature too"},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					UID  uint32
					Full bool
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				cl, err := c.imap()
				if err != nil {
					return "", err
				}
				defer cl.Close()
				if _, err := cl.Select("INBOX", nil).Wait(); err != nil {
					return "", err
				}
				return c.readMessage(cl, in.UID, in.Full, true)
			}),
		tools.New("list_sent_emails", "List the user's most recent sent emails (subject, to, date, uid) — read a few with read_sent_email to learn how they write before drafting a reply in their voice.",
			tools.Schema(map[string]tools.Prop{"count": {Type: "integer", Description: "How many (default 10, max 30)"}}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Count int }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if in.Count <= 0 {
					in.Count = 10
				}
				if in.Count > 30 {
					in.Count = 30
				}
				cl, err := c.imap()
				if err != nil {
					return "", err
				}
				defer cl.Close()
				box, err := c.sentMailbox(cl)
				if err != nil {
					return "", err
				}
				sel, err := cl.Select(box, nil).Wait()
				if err != nil {
					return "", err
				}
				if sel.NumMessages == 0 {
					return "no sent mail", nil
				}
				from := uint32(1)
				if sel.NumMessages > uint32(in.Count) {
					from = sel.NumMessages - uint32(in.Count) + 1
				}
				var ss imap.SeqSet
				ss.AddRange(from, sel.NumMessages)
				msgs, err := cl.Fetch(ss, &imap.FetchOptions{Envelope: true, UID: true}).Collect()
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for i := len(msgs) - 1; i >= 0; i-- {
					m := msgs[i]
					if m.Envelope == nil {
						continue
					}
					to := "?"
					if len(m.Envelope.To) > 0 {
						to = m.Envelope.To[0].Addr()
					}
					fmt.Fprintf(&b, "uid %d | %s | to %s | %s\n", m.UID, m.Envelope.Date.In(c.loc).Format("2 Jan"), to, m.Envelope.Subject)
				}
				return b.String(), nil
			}),
		tools.New("read_sent_email", "Read one of the user's sent emails by uid (from list_sent_emails), to learn their voice. Only what they wrote is shown, not the message they were answering, unless full is true.",
			tools.Schema(map[string]tools.Prop{
				"uid":  {Type: "integer", Description: "uid from list_sent_emails", Required: true},
				"full": {Type: "boolean", Description: "Include the message they were answering too"},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					UID  uint32
					Full bool
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				cl, err := c.imap()
				if err != nil {
					return "", err
				}
				defer cl.Close()
				box, err := c.sentMailbox(cl)
				if err != nil {
					return "", err
				}
				if _, err := cl.Select(box, nil).Wait(); err != nil {
					return "", err
				}
				return c.readMessage(cl, in.UID, in.Full, false)
			}),
		tools.New("reply_email", "Reply to an email in the same thread (keeps the subject and threading headers). Draft in the user's own voice; the user approves before it is sent.",
			tools.Schema(map[string]tools.Prop{
				"uid":       {Type: "integer", Description: "uid of the email being replied to (from list_emails)", Required: true},
				"body":      {Type: "string", Description: "Plain-text reply body, written the way the user writes", Required: true},
				"reply_all": {Type: "boolean", Description: "Reply to everyone on the thread (default: sender only)"},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					UID      uint32
					Body     string
					ReplyAll bool `json:"reply_all"`
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				cl, err := c.imap()
				if err != nil {
					return "", err
				}
				defer cl.Close()
				if _, err := cl.Select("INBOX", nil).Wait(); err != nil {
					return "", err
				}
				msgs, err := cl.Fetch(imap.UIDSetNum(imap.UID(in.UID)), &imap.FetchOptions{Envelope: true}).Collect()
				if err != nil {
					return "", err
				}
				if len(msgs) == 0 || msgs[0].Envelope == nil {
					return "", fmt.Errorf("no email with uid %d", in.UID)
				}
				env := msgs[0].Envelope
				to, cc := replyRecipients(env, c.cfg.Username, in.ReplyAll)
				subject := env.Subject
				if !strings.HasPrefix(strings.ToLower(subject), "re:") {
					subject = "Re: " + subject
				}
				return c.sendMail(to, cc, subject, in.Body, env.MessageID)
			}),
		tools.New("send_email", "Send an email from the user's account.",
			tools.Schema(map[string]tools.Prop{
				"to":      {Type: "string", Description: "Comma-separated recipients", Required: true},
				"subject": {Type: "string", Description: "Subject", Required: true},
				"body":    {Type: "string", Description: "Plain-text body", Required: true},
				"cc":      {Type: "string", Description: "Comma-separated CC"},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ To, Subject, Body, Cc string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				return c.send(in.To, in.Cc, in.Subject, in.Body)
			}),
	}
}

// Name identifies the watch source.
func (c *Client) Name() string { return "inbox" }

// Arrival places an email in the order the inbox received it: its uid,
// under the inbox's UIDVALIDITY (which only grows if the server renumbers
// the inbox). Only mail past the newest seen is news: when one of the newest
// unread emails is read, the next-oldest moves into the window Snapshot
// lists, and that isn't new mail.
func (c *Client) Arrival(key string) (uint64, bool) {
	uid, err := strconv.ParseUint(key, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint64(c.validity.Load())<<32 | uid, true
}

// Snapshot lists unread emails keyed by uid, for change detection.
func (c *Client) Snapshot(ctx context.Context) (map[string]string, error) {
	cl, err := c.imap()
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	sel, err := cl.Select("INBOX", nil).Wait()
	if err != nil {
		return nil, err
	}
	c.validity.Store(sel.UIDValidity)
	res, err := cl.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil {
		return nil, err
	}
	uids := res.AllUIDs()
	out := map[string]string{}
	if len(uids) == 0 {
		return out, nil
	}
	if len(uids) > snapshotMax {
		uids = uids[len(uids)-snapshotMax:]
	}
	msgs, err := cl.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{Envelope: true, UID: true}).Collect()
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m.Envelope == nil {
			continue
		}
		from := "?"
		if len(m.Envelope.From) > 0 {
			from = m.Envelope.From[0].Name
			if from == "" {
				from = m.Envelope.From[0].Addr()
			}
		}
		out[fmt.Sprint(m.UID)] = fmt.Sprintf("unread from %s: %s", from, m.Envelope.Subject)
	}
	return out, nil
}

// sentMailbox finds the Sent folder under its common names.
func (c *Client) sentMailbox(cl *imapclient.Client) (string, error) {
	boxes, err := cl.List("", "*", nil).Collect()
	if err != nil {
		return "", err
	}
	var names []string
	for _, b := range boxes {
		for _, a := range b.Attrs {
			if a == imap.MailboxAttrSent {
				return b.Mailbox, nil
			}
		}
		names = append(names, b.Mailbox)
	}
	for _, cand := range []string{"[Gmail]/Sent Mail", "Sent Messages", "Sent Items", "Sent", "INBOX.Sent"} {
		for _, n := range names {
			if strings.EqualFold(n, cand) {
				return n, nil
			}
		}
	}
	return "", fmt.Errorf("no Sent mailbox found among: %s", strings.Join(names, ", "))
}

// readMessage is one message of the selected mailbox as the model reads it:
// the headers that matter, the decoded text, and what is attached.
func (c *Client) readMessage(cl *imapclient.Client, uid uint32, full, withFrom bool) (string, error) {
	msgs, err := cl.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{Envelope: true, UID: true}).Collect()
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", fmt.Errorf("no email with uid %d; list the emails again, it may have moved", uid)
	}
	rs, err := readable(cl, []imap.UID{imap.UID(uid)})
	if err != nil {
		return "", err
	}
	r, ok := rs[imap.UID(uid)]
	if !ok {
		return "", fmt.Errorf("the mail server didn't describe email %d's parts, so its text can't be read; try again in a moment", uid)
	}
	var hdr strings.Builder
	if env := msgs[0].Envelope; env != nil {
		if withFrom && len(env.From) > 0 {
			fmt.Fprintf(&hdr, "From: %s\n", addrString(env.From[0]))
		}
		if len(env.To) > 0 {
			fmt.Fprintf(&hdr, "To: %s\n", addrList(env.To))
		}
		if len(env.Cc) > 0 {
			fmt.Fprintf(&hdr, "Cc: %s\n", addrList(env.Cc))
		}
		fmt.Fprintf(&hdr, "Date: %s\nSubject: %s\n\n", env.Date.In(c.loc).Format("Mon 2 Jan 2006 15:04"), env.Subject)
		full = full || mailtext.IsForward(env.Subject)
	}
	return hdr.String() + mailtext.Render(r.Text, r.Attachments, full, readMax), nil
}

func addrList(as []imap.Address) string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		if a.Addr() != "" {
			out = append(out, addrString(a))
		}
	}
	return strings.Join(out, ", ")
}

// replyRecipients works out who a reply goes to.
func replyRecipients(env *imap.Envelope, me string, all bool) (to, cc string) {
	var tos []string
	if len(env.ReplyTo) > 0 {
		for _, a := range env.ReplyTo {
			tos = append(tos, a.Addr())
		}
	} else {
		for _, a := range env.From {
			tos = append(tos, a.Addr())
		}
	}
	if !all {
		return strings.Join(tos, ", "), ""
	}
	var ccs []string
	seen := map[string]bool{strings.ToLower(me): true}
	for _, t := range tos {
		seen[strings.ToLower(t)] = true
	}
	for _, a := range append(env.To, env.Cc...) {
		if addr := a.Addr(); addr != "" && !seen[strings.ToLower(addr)] {
			seen[strings.ToLower(addr)] = true
			ccs = append(ccs, addr)
		}
	}
	return strings.Join(tos, ", "), strings.Join(ccs, ", ")
}

func (c *Client) send(to, cc, subject, body string) (string, error) {
	return c.sendMail(to, cc, subject, body, "")
}

// sendMail sends a plain-text email, threaded under inReplyTo when given.
func (c *Client) sendMail(to, cc, subject, body, inReplyTo string) (string, error) {
	from := c.cfg.Username
	fromHdr := from
	if c.cfg.FromName != "" {
		fromHdr = (&mail.Address{Name: c.cfg.FromName, Address: from}).String()
	}
	msg, err := mailtext.Compose(mailtext.Outgoing{From: fromHdr, To: to, Cc: cc, Subject: subject, Body: body, InReplyTo: inReplyTo})
	if err != nil {
		return "", err
	}
	addr := net.JoinHostPort(c.cfg.SMTPHost, fmt.Sprint(c.cfg.SMTPPort))
	auth := smtp.PlainAuth("", c.cfg.Username, c.password, c.cfg.SMTPHost)
	switch {
	case c.smtpSend != nil:
		err = c.smtpSend(addr, auth, from, msg.Recipients, msg.Raw)
	case c.cfg.SMTPPort == 465:
		err = sendImplicitTLS(addr, c.cfg.SMTPHost, auth, from, msg.Recipients, msg.Raw)
	default:
		err = smtp.SendMail(addr, auth, from, msg.Recipients, msg.Raw)
	}
	if err != nil {
		return "", fmt.Errorf("smtp: %w", err)
	}
	return fmt.Sprintf("sent to %s", strings.Join(msg.Recipients, ", ")), nil
}

func sendImplicitTLS(addr, host string, auth smtp.Auth, from string, to []string, msg []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer cl.Close()
	if err := cl.Auth(auth); err != nil {
		return err
	}
	if err := cl.Mail(from); err != nil {
		return err
	}
	for _, r := range to {
		if err := cl.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}

func hasFlag(flags []imap.Flag, f imap.Flag) bool {
	for _, x := range flags {
		if x == f {
			return true
		}
	}
	return false
}

// Reply sends a plain-text email threaded under inReplyTo (may be empty,
// with or without angle brackets).
func (c *Client) Reply(to, subject, body, inReplyTo string) error {
	_, err := c.sendMail(to, "", subject, body, inReplyTo)
	return err
}
