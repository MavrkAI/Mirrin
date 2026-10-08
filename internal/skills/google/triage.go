package google

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/api/googleapi"

	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
)

// triageQuery is the recent mail a first look goes through: the inbox's
// last week, without the Promotions, Social, Updates and Forums tabs.
const triageQuery = "in:inbox newer_than:7d -category:promotions -category:social -category:updates -category:forums"

// InboxMail is one recent inbox email as a triage sees it: who it's from,
// what it's about, when, and Gmail's snippet. Everything in it was written
// by someone else, so it is data, never an instruction.
type InboxMail struct {
	ID, ThreadID        string
	From, Subject, Date string
	Snippet             string
	Unread              bool
}

// RecentInbox reads, and only reads, the newest inbox mail worth a look, up
// to max (newest first). It lists and fetches headers; it never changes a
// label or sends anything.
func (a *Auth) RecentInbox(ctx context.Context, max int64) ([]InboxMail, error) {
	if max <= 0 || max > 30 {
		max = 20
	}
	svc, err := a.gmail(ctx)
	if err != nil {
		return nil, err
	}
	list, err := svc.Users.Messages.List("me").Q(triageQuery).MaxResults(max).Context(ctx).Do()
	if err != nil {
		return nil, a.Explain("gmail", err)
	}
	var out []InboxMail
	for _, m := range list.Messages {
		full, err := svc.Users.Messages.Get("me", m.Id).Format("metadata").MetadataHeaders("From", "Subject", "Date").Context(ctx).Do()
		if err != nil {
			var ge *googleapi.Error
			if errors.As(err, &ge) && ge.Code == 404 {
				continue // deleted since it was listed
			}
			return nil, a.Explain("gmail", err)
		}
		mail := InboxMail{
			ID: full.Id, ThreadID: full.ThreadId,
			From: header(full, "From"), Subject: strings.TrimSpace(header(full, "Subject")), Date: header(full, "Date"),
			Snippet: strings.TrimSpace(mailtext.HTMLText(full.Snippet)),
		}
		for _, l := range full.LabelIds {
			if l == "UNREAD" {
				mail.Unread = true
			}
		}
		out = append(out, mail)
	}
	return out, nil
}

// SenderName is how a person refers to the sender of from: their name,
// else their address.
func SenderName(from string) string { return senderName(from) }
