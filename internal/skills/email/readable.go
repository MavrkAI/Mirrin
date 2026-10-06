package email

import (
	"errors"
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
)

// partLimit caps how much of one text part is fetched: far more than anyone
// reads, and a 20 MB attachment is never downloaded to read the note above it.
const partLimit = 1 << 20

// textPart is a part of a message a person would read.
type textPart struct {
	path      []int
	mediaType string
	encoding  string
	charset   string
	prefix    string // what introduces it (a forwarded message's headers)
}

// Readable is a message as a person reads it: its text, decoded, and what
// is attached.
type Readable struct {
	Text        string
	Attachments []mailtext.Attachment
}

// plan picks what to read from a message's structure: the plain-text version
// where there is one (else the HTML), every text part of a mixed message in
// order, the text of a message forwarded as an attachment, and the names of
// real attachments (not pictures embedded in the HTML, not signatures).
func plan(bs imap.BodyStructure) (texts []textPart, atts []mailtext.Attachment) {
	walk(bs, nil, false, &texts, &atts)
	return texts, atts
}

func walk(bs imap.BodyStructure, path []int, related bool, texts *[]textPart, atts *[]mailtext.Attachment) {
	switch p := bs.(type) {
	case *imap.BodyStructureMultiPart:
		child := func(i int) []int { return append(append([]int(nil), path...), i+1) }
		if strings.EqualFold(p.Subtype, "alternative") {
			// The versions are the same message: read the plainest one
			// that has text, else the last (the richest).
			var best []textPart
			var bestAtts []mailtext.Attachment
			bestScore := -1
			for i, c := range p.Children {
				var t []textPart
				var a []mailtext.Attachment
				walk(c, child(i), related, &t, &a)
				score := 0
				if len(t) > 0 {
					score = 1
					if allPlain(t) {
						score = 2
					}
				}
				if score > bestScore || (score == 1 && bestScore == 1) {
					best, bestAtts, bestScore = t, a, score
				}
			}
			*texts = append(*texts, best...)
			*atts = append(*atts, bestAtts...)
			return
		}
		sub := strings.EqualFold(p.Subtype, "related")
		for i, c := range p.Children {
			walk(c, child(i), sub, texts, atts)
		}
	case *imap.BodyStructureSinglePart:
		if len(path) == 0 {
			path = []int{1} // a message that isn't multipart has one part, number 1
		}
		mt := p.MediaType()
		disp := ""
		var dparams map[string]string
		if d := p.Disposition(); d != nil {
			disp, dparams = strings.ToLower(d.Value), d.Params
		}
		name := mailtext.Filename(dparams)
		if name == "" {
			name = mailtext.Filename(p.Params)
		}
		switch {
		case mt == "message/rfc822" && p.MessageRFC822 != nil && p.MessageRFC822.BodyStructure != nil:
			var t []textPart
			inner := p.MessageRFC822.BodyStructure
			innerPath := path
			if _, single := inner.(*imap.BodyStructureSinglePart); single {
				innerPath = append(append([]int(nil), path...), 1)
			}
			walk(inner, innerPath, false, &t, atts)
			if len(t) > 0 {
				t[0].prefix = forwardHeader(p.MessageRFC822.Envelope)
				*texts = append(*texts, t...)
			} else {
				*atts = append(*atts, mailtext.Attachment{Name: forwardName(p.MessageRFC822.Envelope), Type: mt, Size: int64(p.Size)})
			}
		case (mt == "text/plain" || mt == "text/html") && disp != "attachment" && (name == "" || disp == "inline"):
			*texts = append(*texts, textPart{path: path, mediaType: mt, encoding: p.Encoding, charset: p.Params["charset"]})
		case strings.HasSuffix(mt, "pgp-signature") || strings.HasSuffix(mt, "pkcs7-signature") || strings.HasSuffix(mt, "x-pkcs7-signature"):
			// a signature proves who sent it; it isn't something to read
		case disp != "attachment" && (related && p.ID != "" || strings.HasPrefix(mt, "image/") && name == ""):
			// a picture embedded in the HTML (a logo, a signature image)
		default:
			*atts = append(*atts, mailtext.Attachment{Name: name, Type: mt, Size: decodedSize(p)})
		}
	}
}

func allPlain(t []textPart) bool {
	for _, p := range t {
		if p.mediaType != "text/plain" {
			return false
		}
	}
	return true
}

// decodedSize estimates a part's size once its transfer encoding is undone.
func decodedSize(p *imap.BodyStructureSinglePart) int64 {
	n := int64(p.Size)
	if strings.EqualFold(p.Encoding, "base64") {
		return n * 3 / 4
	}
	return n
}

func forwardHeader(env *imap.Envelope) string {
	var b strings.Builder
	b.WriteString("---------- Forwarded message ----------\n")
	if env != nil {
		if len(env.From) > 0 {
			fmt.Fprintf(&b, "From: %s\n", addrString(env.From[0]))
		}
		if !env.Date.IsZero() {
			fmt.Fprintf(&b, "Date: %s\n", env.Date.Format("Mon 2 Jan 2006 15:04"))
		}
		if env.Subject != "" {
			fmt.Fprintf(&b, "Subject: %s\n", env.Subject)
		}
	}
	return b.String() + "\n"
}

func forwardName(env *imap.Envelope) string {
	if env != nil && env.Subject != "" {
		return "forwarded email: " + env.Subject
	}
	return "forwarded email"
}

func addrString(a imap.Address) string {
	if a.Name == "" {
		return a.Addr()
	}
	return a.Name + " <" + a.Addr() + ">"
}

// Unreadable stands in for the text of a message the server wouldn't give
// (Incoming.Body is exactly this), so a caller can tell there is nothing
// to answer.
const Unreadable = "(this email's text couldn't be read)"

const unreadable = Unreadable

// readable fetches what a person would read of the given messages in the
// selected mailbox, keyed by uid: the structure first, then only the text
// parts, so attachments are named but never downloaded. One message the
// server won't give up doesn't hold back the rest: it is marked unreadable.
// A lost connection fails the whole batch, to be tried again.
func readable(cl *imapclient.Client, uids []imap.UID) (map[imap.UID]Readable, error) {
	out := map[imap.UID]Readable{}
	if len(uids) == 0 {
		return out, nil
	}
	msgs, err := cl.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true}}).Collect()
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m.BodyStructure == nil {
			out[m.UID] = Readable{Text: unreadable}
			continue
		}
		texts, atts := plan(m.BodyStructure)
		r := Readable{Attachments: atts}
		if len(texts) > 0 {
			sections := make([]*imap.FetchItemBodySection, len(texts))
			for i, t := range texts {
				sections[i] = &imap.FetchItemBodySection{Part: t.path, Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: partLimit}}
			}
			parts, err := cl.Fetch(imap.UIDSetNum(m.UID), &imap.FetchOptions{UID: true, BodySection: sections}).Collect()
			var refused *imap.Error
			if errors.As(err, &refused) {
				r.Text = unreadable // the server said no to this one message
				out[m.UID] = r
				continue
			}
			if err != nil {
				return nil, err
			}
			var chunks []string
			if len(parts) > 0 {
				for i, t := range texts {
					raw := parts[0].FindBodySection(sections[i])
					text := mailtext.Text(mailtext.Decode(raw, t.encoding, t.charset), t.mediaType)
					if text == "" && t.prefix == "" {
						continue
					}
					chunks = append(chunks, t.prefix+text)
				}
			}
			r.Text = strings.TrimSpace(strings.Join(chunks, "\n\n"))
		}
		out[m.UID] = r
	}
	return out, nil
}
