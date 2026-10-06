package email

import (
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
)

// selfSentWithin is how recently mail the mailbox sent itself must have
// been sent to count: the mail channel looks every half minute, so a
// genuine one is always fresh, and an old Message-ID can't be replayed.
const selfSentWithin = 10 * time.Minute

// SelfSent reports whether in, mail in the mailbox's own address, was
// really sent from this mailbox to itself. A provider delivering mail
// between its own users often writes no Authentication-Results for it, so
// there is nothing to vouch for the sender; but only someone signed in to
// the mailbox can have put the same message in its Sent folder. So it
// counts when, in Sent, there is a message with the same Message-ID and
// subject, addressed to the mailbox itself and sent in the last ten
// minutes, and the inbox holds just this one copy (a second is someone
// replaying a Message-ID they were sent). Mail the twin composed itself
// (its own replies) never counts.
func (c *Client) SelfSent(in Incoming) (bool, error) {
	me := strings.ToLower(strings.TrimSpace(c.cfg.Username))
	id := strings.Trim(strings.TrimSpace(in.MessageID), "<>")
	if id == "" || !strings.Contains(me, "@") || !strings.EqualFold(strings.TrimSpace(in.From), me) ||
		mailtext.Ours(id) || strings.ContainsAny(id, "\"\\\r\n") {
		return false, nil
	}
	cl, err := c.imap()
	if err != nil {
		return false, err
	}
	defer cl.Close()
	crit := &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: id}}}

	if _, err := cl.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return false, err
	}
	res, err := cl.UIDSearch(crit, nil).Wait()
	if err != nil {
		return false, err
	}
	if uids := res.AllUIDs(); len(uids) != 1 || uint32(uids[0]) != in.UID {
		return false, nil
	}

	sent, err := c.sentMailbox(cl)
	if err != nil {
		return false, nil // no Sent folder: nothing to check against
	}
	if _, err := cl.Select(sent, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return false, err
	}
	res, err = cl.UIDSearch(crit, nil).Wait()
	if err != nil {
		return false, err
	}
	uids := res.AllUIDs()
	if len(uids) == 0 {
		return false, nil
	}
	msgs, err := cl.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{Envelope: true, InternalDate: true, UID: true}).Collect()
	if err != nil {
		return false, err
	}
	for _, m := range msgs {
		env := m.Envelope
		if env == nil || strings.Trim(env.MessageID, "<>") != id || env.Subject != in.Subject {
			continue
		}
		if age := time.Since(m.InternalDate); age > selfSentWithin || age < -selfSentWithin {
			continue
		}
		for _, a := range append(append([]imap.Address(nil), env.To...), env.Cc...) {
			if strings.EqualFold(a.Addr(), me) {
				return true, nil
			}
		}
	}
	return false, nil
}
