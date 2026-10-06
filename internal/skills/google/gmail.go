package google

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"strings"

	"google.golang.org/api/gmail/v1"

	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// readMax is how much of an email's text the model is given.
const readMax = 20000

func (a *Auth) gmail(ctx context.Context) (*gmail.Service, error) {
	opts, err := a.ClientOptions()
	if err != nil {
		return nil, a.Explain("gmail", err)
	}
	return gmail.NewService(ctx, opts...)
}

func header(m *gmail.Message, name string) string {
	if m.Payload == nil {
		return ""
	}
	return partHeader(m.Payload, name)
}

func partHeader(p *gmail.MessagePart, name string) string {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// gpart is a part of a Gmail message a person would read.
type gpart struct {
	part   *gmail.MessagePart
	prefix string
}

// gmailPlan picks what to read, as the IMAP reader does: the plain version
// where there is one, else the HTML; the text of a forwarded message; and
// the names of real attachments (not pictures embedded in the HTML).
func gmailPlan(p *gmail.MessagePart, related bool, texts *[]gpart, atts *[]mailtext.Attachment) {
	if p == nil {
		return
	}
	mt := strings.ToLower(p.MimeType)
	disp := strings.ToLower(partHeader(p, "Content-Disposition"))
	switch {
	case mt == "multipart/alternative":
		var best []gpart
		var bestAtts []mailtext.Attachment
		bestScore := -1
		for _, c := range p.Parts {
			var t []gpart
			var at []mailtext.Attachment
			gmailPlan(c, related, &t, &at)
			score := 0
			if len(t) > 0 {
				score = 1
				plain := true
				for _, x := range t {
					if !strings.HasPrefix(strings.ToLower(x.part.MimeType), "text/plain") {
						plain = false
					}
				}
				if plain {
					score = 2
				}
			}
			if score > bestScore || (score == 1 && bestScore == 1) {
				best, bestAtts, bestScore = t, at, score
			}
		}
		*texts = append(*texts, best...)
		*atts = append(*atts, bestAtts...)
	case strings.HasPrefix(mt, "multipart/"):
		for _, c := range p.Parts {
			gmailPlan(c, mt == "multipart/related", texts, atts)
		}
	case mt == "message/rfc822" && len(p.Parts) > 0:
		var t []gpart
		for _, c := range p.Parts {
			gmailPlan(c, false, &t, atts)
		}
		if len(t) > 0 {
			t[0].prefix = gmailForwardHeader(p)
			*texts = append(*texts, t...)
		}
	case (mt == "text/plain" || mt == "text/html") && !strings.HasPrefix(disp, "attachment") && (p.Filename == "" || strings.HasPrefix(disp, "inline")):
		*texts = append(*texts, gpart{part: p})
	case strings.HasSuffix(mt, "pgp-signature") || strings.HasSuffix(mt, "pkcs7-signature"):
	case !strings.HasPrefix(disp, "attachment") && (related && partHeader(p, "Content-ID") != "" || strings.HasPrefix(mt, "image/") && p.Filename == ""):
	default:
		var size int64
		if p.Body != nil {
			size = p.Body.Size
		}
		*atts = append(*atts, mailtext.Attachment{Name: p.Filename, Type: p.MimeType, Size: size})
	}
}

func gmailForwardHeader(p *gmail.MessagePart) string {
	var b strings.Builder
	b.WriteString("---------- Forwarded message ----------\n")
	src := p
	if partHeader(src, "From") == "" && len(p.Parts) > 0 {
		src = p.Parts[0]
	}
	for _, h := range []string{"From", "Date", "Subject"} {
		if v := partHeader(src, h); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", h, v)
		}
	}
	return b.String() + "\n"
}

func decodeData(s string) []byte {
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		if b, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")); err != nil {
			b, _ = base64.StdEncoding.DecodeString(s)
		}
	}
	return b
}

// readable is what a person would read of a Gmail message: the text,
// decoded from its charset, and what is attached. Long text parts that
// Gmail keeps aside are fetched; attachments are named, never fetched.
func readable(ctx context.Context, svc *gmail.Service, m *gmail.Message) (string, []mailtext.Attachment) {
	var texts []gpart
	var atts []mailtext.Attachment
	gmailPlan(m.Payload, false, &texts, &atts)
	var chunks []string
	for _, t := range texts {
		data := ""
		if t.part.Body != nil {
			data = t.part.Body.Data
			if data == "" && t.part.Body.AttachmentId != "" && svc != nil {
				if att, err := svc.Users.Messages.Attachments.Get("me", m.Id, t.part.Body.AttachmentId).Context(ctx).Do(); err == nil {
					data = att.Data
				}
			}
		}
		cs := ""
		if _, params, err := mime.ParseMediaType(partHeader(t.part, "Content-Type")); err == nil {
			cs = params["charset"]
		}
		text := mailtext.Text(mailtext.ToUTF8(decodeData(data), cs), strings.ToLower(t.part.MimeType))
		if text == "" && t.prefix == "" {
			continue
		}
		chunks = append(chunks, t.prefix+text)
	}
	return strings.TrimSpace(strings.Join(chunks, "\n\n")), atts
}

// GmailTools returns gmail_search, gmail_read, gmail_send, gmail_reply and gmail_archive.
func (a *Auth) GmailTools() []tools.Tool {
	return []tools.Tool{
		tools.New("gmail_search", "Search Gmail with normal Gmail query syntax (from:, subject:, newer_than:2d, is:unread, has:attachment, in:sent, label:…). Returns id, date, from, subject and snippet, newest first.",
			tools.Schema(map[string]tools.Prop{
				"query": {Type: "string", Description: "e.g. \"from:jetstar newer_than:7d\"", Required: true},
				"max":   {Type: "integer", Description: "default 10, up to 30"},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					Query string
					Max   int64
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if in.Max <= 0 || in.Max > 30 {
					in.Max = 10
				}
				svc, err := a.gmail(ctx)
				if err != nil {
					return "", err
				}
				list, err := svc.Users.Messages.List("me").Q(in.Query).MaxResults(in.Max).Context(ctx).Do()
				if err != nil {
					return "", a.Explain("gmail", err)
				}
				if len(list.Messages) == 0 {
					return "no messages match", nil
				}
				var b strings.Builder
				for _, m := range list.Messages {
					full, err := svc.Users.Messages.Get("me", m.Id).Format("metadata").MetadataHeaders("From", "Subject", "Date").Context(ctx).Do()
					if err != nil {
						continue
					}
					unread := ""
					for _, l := range full.LabelIds {
						if l == "UNREAD" {
							unread = " (unread)"
						}
					}
					fmt.Fprintf(&b, "id=%s | %s | %s | %s%s\n  %s\n", full.Id, header(full, "Date"), header(full, "From"), header(full, "Subject"), unread, strings.TrimSpace(mailtext.HTMLText(full.Snippet)))
				}
				return b.String(), nil
			}),
		tools.New("gmail_read", "Read one email by id (from gmail_search): who it's from, to whom, when, the text (earlier messages quoted under a reply are left out unless full is true) and the names and sizes of any attachments.",
			tools.Schema(map[string]tools.Prop{
				"id":   {Type: "string", Required: true},
				"full": {Type: "boolean", Description: "Include the quoted earlier messages and signature too"},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					ID   string
					Full bool
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				svc, err := a.gmail(ctx)
				if err != nil {
					return "", err
				}
				m, err := svc.Users.Messages.Get("me", in.ID).Format("full").Context(ctx).Do()
				if err != nil {
					return "", a.Explain("gmail", err)
				}
				text, atts := readable(ctx, svc, m)
				subject := header(m, "Subject")
				var b strings.Builder
				fmt.Fprintf(&b, "From: %s\nTo: %s\n", header(m, "From"), header(m, "To"))
				if cc := header(m, "Cc"); cc != "" {
					fmt.Fprintf(&b, "Cc: %s\n", cc)
				}
				fmt.Fprintf(&b, "Date: %s\nSubject: %s\nThread: %s\n\n", header(m, "Date"), subject, m.ThreadId)
				b.WriteString(mailtext.Render(text, atts, in.Full || mailtext.IsForward(subject), readMax))
				return b.String(), nil
			}),
		tools.New("gmail_send", "Send a new email from the connected Gmail account.",
			tools.Schema(map[string]tools.Prop{
				"to":      {Type: "string", Required: true},
				"subject": {Type: "string", Required: true},
				"body":    {Type: "string", Description: "Plain text", Required: true},
				"cc":      {Type: "string"},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ To, Subject, Body, Cc string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				return a.send(ctx, in.To, in.Cc, in.Subject, in.Body, "", "")
			}),
		tools.New("gmail_reply", "Reply to an email (by id) in its thread.",
			tools.Schema(map[string]tools.Prop{
				"id":   {Type: "string", Description: "The message being replied to", Required: true},
				"body": {Type: "string", Required: true},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ ID, Body string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				svc, err := a.gmail(ctx)
				if err != nil {
					return "", err
				}
				orig, err := svc.Users.Messages.Get("me", in.ID).Format("metadata").MetadataHeaders("From", "Reply-To", "Subject", "Message-ID").Context(ctx).Do()
				if err != nil {
					return "", a.Explain("gmail", err)
				}
				to := header(orig, "Reply-To")
				if to == "" {
					to = header(orig, "From")
				}
				subject := header(orig, "Subject")
				if !strings.HasPrefix(strings.ToLower(subject), "re:") {
					subject = "Re: " + subject
				}
				return a.send(ctx, to, "", subject, in.Body, orig.ThreadId, header(orig, "Message-ID"))
			}),
		tools.New("gmail_archive", "Archive an email (remove it from the inbox) or mark it read.",
			tools.Schema(map[string]tools.Prop{
				"id":        {Type: "string", Required: true},
				"mark_read": {Type: "boolean", Description: "Only mark as read, don't archive"},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					ID       string
					MarkRead bool `json:"mark_read"`
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				svc, err := a.gmail(ctx)
				if err != nil {
					return "", err
				}
				remove := []string{"UNREAD"}
				if !in.MarkRead {
					remove = append(remove, "INBOX")
				}
				if _, err := svc.Users.Messages.Modify("me", in.ID, &gmail.ModifyMessageRequest{RemoveLabelIds: remove}).Context(ctx).Do(); err != nil {
					return "", a.Explain("gmail", err)
				}
				if in.MarkRead {
					return "marked read", nil
				}
				return "archived", nil
			}),
	}
}

func (a *Auth) send(ctx context.Context, to, cc, subject, text, threadID, inReplyTo string) (string, error) {
	svc, err := a.gmail(ctx)
	if err != nil {
		return "", err
	}
	// No From: Gmail fills in the account's address with the name the owner
	// set in Gmail, which is how their mail should look.
	msg, err := mailtext.Compose(mailtext.Outgoing{To: to, Cc: cc, Subject: subject, Body: text, InReplyTo: inReplyTo, MessageIDDomain: a.knownEmail()})
	if err != nil {
		return "", err
	}
	sent, err := svc.Users.Messages.Send("me", &gmail.Message{Raw: base64.URLEncoding.EncodeToString(msg.Raw), ThreadId: threadID}).Context(ctx).Do()
	if err != nil {
		return "", a.Explain("gmail", err)
	}
	return fmt.Sprintf("sent to %s (%s)", strings.Join(msg.Recipients, ", "), sent.Id), nil
}
