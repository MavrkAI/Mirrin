package google

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
)

// Mail is one recent email, read for a brief: who, when, what about, and
// the start of what it said (earlier messages quoted under it left out).
// Every field is someone else's words.
type Mail struct {
	ID      string
	From    string
	To      string
	Date    string
	Subject string
	Text    string
}

// mailWithText is as much of each email's text as MailWith keeps.
const mailWithText = 1200

// MailWith lists up to n emails from the last `within` that addr sent, or
// that were sent to or copied to addr, newest first. Gmail's search is
// loose, so a message whose From, To or Cc doesn't actually name addr is
// left out. It only reads.
func (a *Auth) MailWith(ctx context.Context, addr string, within time.Duration, n int) ([]Mail, error) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" || strings.ContainsAny(addr, " \"(){}") || !strings.Contains(addr, "@") {
		return nil, nil
	}
	if n <= 0 {
		n = 3
	}
	days := max(1, int(within/(24*time.Hour)))
	svc, err := a.gmail(ctx)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf("{from:%s to:%s cc:%s} newer_than:%dd", addr, addr, addr, days)
	list, err := svc.Users.Messages.List("me").Q(q).MaxResults(int64(n * 3)).Context(ctx).Do()
	if err != nil {
		return nil, a.Explain("gmail", err)
	}
	var out []Mail
	for _, ref := range list.Messages {
		if len(out) == n {
			break
		}
		m, err := svc.Users.Messages.Get("me", ref.Id).Format("full").Context(ctx).Do()
		if err != nil {
			continue
		}
		if !names(addr, header(m, "From"), header(m, "To"), header(m, "Cc")) {
			continue
		}
		text, _ := readable(ctx, svc, m)
		text, _, _ = mailtext.Clean(text)
		out = append(out, Mail{ID: m.Id, From: header(m, "From"), To: header(m, "To"), Date: header(m, "Date"),
			Subject: header(m, "Subject"), Text: mailtext.Truncate(strings.TrimSpace(text), mailWithText)})
	}
	return out, nil
}

// names reports whether any of the address lists holds addr.
func names(addr string, lists ...string) bool {
	for _, l := range lists {
		if strings.TrimSpace(l) == "" {
			continue
		}
		as, err := mail.ParseAddressList(l)
		if err != nil {
			// A list Go can't parse: only an address in angle brackets, or
			// the whole list, counts ("sam@x" mustn't match "pam@x").
			l = strings.ToLower(strings.TrimSpace(l))
			if l == addr || strings.Contains(l, "<"+addr+">") {
				return true
			}
			continue
		}
		for _, a := range as {
			if strings.EqualFold(a.Address, addr) {
				return true
			}
		}
	}
	return false
}
