package email

import (
	"cmp"
	"net"
	"net/textproto"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
)

// Incoming is an inbox message for the mail channel.
type Incoming struct {
	UID       uint32
	MessageID string
	From      string
	FromName  string
	Subject   string
	// Body is the decoded text (the plain version, or the HTML as text),
	// with any quoted history still in it for the caller to trim.
	Body string
	// Attachments are named, not downloaded.
	Attachments []mailtext.Attachment
	Date        time.Time
	// AuthResults are the message's Authentication-Results headers, top
	// first: the receiving server's own verdicts come before anything the
	// sender wrote into the message.
	AuthResults []string
	// Trace is the Received and Authentication-Results fields, top first,
	// in the order they appear among each other: the Received fields mark
	// where the receiving servers' part of the header ends.
	Trace []Field
}

// Mark is a place in the inbox: messages delivered after it are new.
type Mark struct{ Validity, Next uint32 }

// UnreadAfter returns unread inbox messages delivered after mark, oldest
// first, and the mark to pass next time. Nothing is marked read: mail the
// caller ignores stays unread for the owner. A zero mark, or one from before
// the mailbox was rebuilt, returns no messages, only where the inbox is now.
func (c *Client) UnreadAfter(mark Mark, limit int) ([]Incoming, Mark, error) {
	cl, err := c.imap()
	if err != nil {
		return nil, mark, err
	}
	defer cl.Close()
	sel, err := cl.Select("INBOX", nil).Wait()
	if err != nil {
		return nil, mark, err
	}
	now := Mark{Validity: sel.UIDValidity, Next: uint32(sel.UIDNext)}
	if now.Next == 0 { // UIDNEXT is required, but be kind to odd servers
		all, err := cl.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
		if err != nil {
			return nil, mark, err
		}
		for _, u := range all.AllUIDs() {
			now.Next = max(now.Next, uint32(u)+1)
		}
		now.Next = max(now.Next, 1)
	}
	if mark.Next == 0 || mark.Validity != now.Validity {
		return nil, now, nil
	}
	var since imap.UIDSet
	since.AddRange(imap.UID(mark.Next), 0) // mark.Next:*
	res, err := cl.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{since}, NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil {
		return nil, mark, err
	}
	var uids []imap.UID
	for _, u := range res.AllUIDs() {
		if uint32(u) >= mark.Next { // "n:*" always matches the newest message, even below n
			uids = append(uids, u)
		}
	}
	slices.Sort(uids)
	next := Mark{Validity: now.Validity, Next: max(now.Next, mark.Next)}
	if limit > 0 && len(uids) > limit {
		uids = uids[:limit]
		next.Next = uint32(uids[len(uids)-1]) + 1 // the rest come next time
	} else if len(uids) > 0 {
		next.Next = max(next.Next, uint32(uids[len(uids)-1])+1)
	}
	if len(uids) == 0 {
		return nil, next, nil
	}
	// The whole header, not just its Authentication-Results: where they sit
	// among the Received fields says which the receiving servers wrote.
	auth := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	msgs, err := cl.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{Envelope: true, UID: true, BodySection: []*imap.FetchItemBodySection{auth}}).Collect()
	if err != nil {
		return nil, mark, err
	}
	bodies, err := readable(cl, uids)
	if err != nil {
		return nil, mark, err
	}
	out := make([]Incoming, 0, len(msgs))
	for _, m := range msgs {
		hdr := m.FindBodySection(auth)
		in := Incoming{UID: uint32(m.UID), AuthResults: authResults(hdr), Trace: traceFields(hdr)}
		if m.Envelope != nil {
			in.MessageID, in.Subject, in.Date = m.Envelope.MessageID, m.Envelope.Subject, m.Envelope.Date
			if len(m.Envelope.From) > 0 {
				in.From, in.FromName = m.Envelope.From[0].Addr(), m.Envelope.From[0].Name
			}
		}
		r := bodies[m.UID]
		in.Body, in.Attachments = strings.TrimSpace(r.Text), r.Attachments
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b Incoming) int { return cmp.Compare(a.UID, b.UID) })
	return out, next, nil
}

// MarkRead marks inbox messages read.
func (c *Client) MarkRead(uids ...uint32) error {
	if len(uids) == 0 {
		return nil
	}
	cl, err := c.imap()
	if err != nil {
		return err
	}
	defer cl.Close()
	if _, err := cl.Select("INBOX", nil).Wait(); err != nil {
		return err
	}
	set := make([]imap.UID, len(uids))
	for i, u := range uids {
		set[i] = imap.UID(u)
	}
	return cl.Store(imap.UIDSetNum(set...), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}, Silent: true}, nil).Close()
}

// authResults pulls the Authentication-Results values, in order, out of a
// raw header block.
func authResults(raw []byte) []string {
	var out []string
	for _, f := range traceFields(raw) {
		if f.Name == "Authentication-Results" {
			out = append(out, f.Value)
		}
	}
	return out
}

// Field is one header field, its name in canonical form.
type Field struct{ Name, Value string }

// traceFields are the Received and Authentication-Results fields of a raw
// header block, unfolded, top first, in the order they appear relative to
// each other.
func traceFields(raw []byte) []Field {
	var out []Field
	cur := -1 // the field a folded line continues, if it is kept
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			if line == "" {
				break // the end of the header
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if cur >= 0 {
				out[cur].Value = strings.TrimSpace(out[cur].Value + " " + strings.TrimSpace(line))
			}
			continue
		}
		cur = -1
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
		if name != "Authentication-Results" && name != "Received" {
			continue
		}
		out = append(out, Field{Name: name, Value: strings.TrimSpace(value)})
		cur = len(out) - 1
	}
	return out
}

// Receiving servers that sign Authentication-Results, by the domain of the
// provider's IMAP host. Each entry vouches for one header (see the mail
// channel), so iCloud, which writes one header per check, is listed by
// name. Microsoft writes the header with no server name.
var knownAuthServers = map[string][]string{
	"gmail.com":      {"mx.google.com"},
	"googlemail.com": {"mx.google.com"},
	"fastmail.com":   {"messagingengine.com"},
	"me.com":         icloud,
	"icloud.com":     icloud,
	"yahoo.com":      {"yahoo.com"},
	"zoho.com":       {"zohomail.com"},
	"office365.com":  {""},
	"outlook.com":    {""},
}

var icloud = []string{"bimi.icloud.com", "dmarc.icloud.com", "dkim-verifier.icloud.com", "spf.icloud.com"}

// AuthServers are the receiving servers whose Authentication-Results this
// mailbox trusts, and whether they are a guess: skills.email.auth_servers,
// else a known provider's, else the IMAP host's own domain (example.org for
// imap.example.org). An address or localhost, as with Proton Mail Bridge,
// says nothing about where the mail was received, so there is no guess.
func (c *Client) AuthServers() (servers []string, guessed bool) {
	if len(c.cfg.AuthServers) > 0 {
		return c.cfg.AuthServers, false
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(c.cfg.IMAPHost), "."))
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host) != nil {
		return nil, true
	}
	for domain, servers := range knownAuthServers {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return servers, true
		}
	}
	if labels := strings.Split(host, "."); len(labels) > 2 {
		return []string{strings.Join(labels[1:], ".")}, true
	}
	return []string{host}, true
}
