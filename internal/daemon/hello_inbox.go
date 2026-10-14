package daemon

import (
	"context"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	gauth "github.com/MavrkAI/Mirrin/internal/skills/google"
)

// The first hello's look at the inbox. Go reads the newest inbox mail and
// picks the one most clearly waiting on the owner; the model only words
// what Go hands it. As in the first look (inbox_first.go), a health, money
// or relationship matter reaches the model as its kind and sender only,
// and nothing is remembered.

// automatedSender matches senders no one writes back to.
var automatedSender = regexp.MustCompile(`(?i)\b(no[-_]?reply|do[-_]?not[-_]?reply|notifications?|mailer-daemon|newsletter|bounces?|alerts?)[@.\-_]`)

// waitingMail words the inbox for the first hello: the newest unread email
// from a person (a plain one before a sensitive one), how long it has
// waited, and how many more people are waiting. mails are the newest
// unread, newest first; complete says they are all of it (fewer than were
// asked for), so "nothing" and "N more" are only said when they are known,
// and a full window says "at least" or, with no person in it, nothing.
func waitingMail(mails []gauth.InboxMail, complete bool, now time.Time) string {
	var pick *gauth.InboxMail
	people := 0
	for i := range mails {
		m := &mails[i]
		if !m.Unread || automatedSender.MatchString(m.From) {
			continue
		}
		people++
		if pick == nil || (sensitiveMail(*pick) != "" && sensitiveMail(*m) == "") {
			pick = m
		}
	}
	if pick == nil {
		if !complete {
			return "" // someone may be waiting further down
		}
		return "Inbox: nothing unread from a person."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Waiting in the inbox: an unread email from %s", mailLine(gauth.SenderName(pick.From), 60))
	if kind := sensitiveMail(*pick); kind != "" {
		fmt.Fprintf(&b, " about a %s matter (details left out)", kind)
	} else if subject := mailLine(pick.Subject, 80); subject != "" {
		fmt.Fprintf(&b, ", “%s”", subject)
	} else {
		b.WriteString(", (no subject)")
	}
	if ago := mailAgo(pick.Date, now); ago != "" {
		b.WriteString(", " + ago)
	}
	switch {
	case people > 1 && complete:
		fmt.Fprintf(&b, ", and %d more unread from people", people-1)
	case people > 1:
		fmt.Fprintf(&b, ", and at least %d more unread from people", people-1)
	}
	b.WriteString(".")
	return b.String()
}

// mailAgo is how long ago an email's Date header was, in words: "just
// now", "a minute ago", "10 minutes ago", "an hour ago", "2 hours ago",
// "yesterday", "3 days ago". A day or more is counted in calendar days
// where the owner is, so "yesterday" is the day before, never the one
// before that. "" for a date that can't be read.
func mailAgo(date string, now time.Time) string {
	t, err := mail.ParseDate(date)
	if err != nil {
		return ""
	}
	since := now.Sub(t)
	switch {
	case since < time.Minute:
		return "just now"
	case since < 2*time.Minute:
		return "a minute ago"
	case since < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(since.Minutes()))
	case since < 2*time.Hour:
		return "an hour ago"
	case since < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(since.Hours()))
	}
	days := daysBetween(t, now)
	if days <= 1 {
		return "yesterday"
	}
	return fmt.Sprintf("%d days ago", days)
}

// daysBetween is how many calendar days, where now is, lie between t and now.
func daysBetween(t, now time.Time) int {
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	y, m, d = t.In(now.Location()).Date()
	then := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	return int(today.Sub(then).Hours() / 24)
}

// helloMails is how many of the newest inbox emails the first hello reads,
// and helloInboxWait how long it gives Gmail: the hello must not wait on it.
const (
	helloMails     = 10
	helloInboxWait = 6 * time.Second
)

// helloReadsInbox reports whether the first hello can look at the inbox:
// Google connected before the hello, with Gmail granted.
func (d *Daemon) helloReadsInbox(c config.Config) bool {
	return d.google.Connected() && c.Skills.Gmail.Enabled
}

// helloInbox is the first hello's inbox line, "" when Gmail doesn't answer
// in time: then the hello says nothing of the inbox rather than guess. It
// reads the unread mail, so what the owner has read since hides no one.
func (d *Daemon) helloInbox(ctx context.Context, now time.Time) string {
	ictx, cancel := context.WithTimeout(ctx, helloInboxWait)
	defer cancel()
	mails, err := d.google.RecentUnread(ictx, helloMails)
	if err != nil {
		d.log.Warn("first hello: inbox", "err", err)
		return ""
	}
	return waitingMail(mails, len(mails) < helloMails, now)
}
