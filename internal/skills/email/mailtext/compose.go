package mailtext

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// Outgoing is a plain-text email to send.
type Outgoing struct {
	From      string // an address, optionally with a name: "Sam <sam@example.com>"
	To, Cc    string // comma-separated addresses, as a person (or the model) writes them
	Subject   string
	Body      string
	InReplyTo string // the Message-ID being answered, with or without <>
	Date      time.Time
	// MessageIDDomain is the domain for the Message-ID when From is "" (the
	// server fills in the sender). With neither, the server assigns one.
	MessageIDDomain string
}

// Composed is a message ready for SMTP or Gmail.
type Composed struct {
	Raw        []byte
	Recipients []string // bare addresses, To then Cc
}

// Compose builds a standards-correct message: addresses parsed (so nothing
// can smuggle in another header, like a Bcc), non-ASCII subjects and names
// encoded, the body sent as quoted-printable UTF-8, threading headers in
// angle brackets, and a Message-ID of its own.
func Compose(m Outgoing) (Composed, error) {
	from := &mail.Address{}
	if strings.TrimSpace(m.From) != "" { // "" leaves the sender to the server (Gmail)
		var err error
		if from, err = mail.ParseAddress(oneLine(m.From)); err != nil {
			return Composed{}, fmt.Errorf("the sender address %q doesn't look right", m.From)
		}
	}
	to, err := addresses(m.To)
	if err != nil {
		return Composed{}, err
	}
	cc, err := addresses(m.Cc)
	if err != nil {
		return Composed{}, err
	}
	if len(to)+len(cc) == 0 {
		return Composed{}, errors.New("no recipients")
	}
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	var b bytes.Buffer
	if from.Address != "" {
		fmt.Fprintf(&b, "From: %s\r\n", from.String())
	}
	if len(to) > 0 {
		fmt.Fprintf(&b, "To: %s\r\n", joinAddrs(to))
	}
	if len(cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", joinAddrs(cc))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", oneLine(m.Subject)))
	fmt.Fprintf(&b, "Date: %s\r\n", date.Format(time.RFC1123Z))
	if domain := idDomain(from.Address, m.MessageIDDomain); domain != "" {
		id := messageID(domain)
		remember(id)
		fmt.Fprintf(&b, "Message-ID: %s\r\n", id)
	}
	if id := msgID(m.InReplyTo); id != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\nReferences: %s\r\n", id, id)
	}
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(&b)
	body := strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n", "\r\n")
	if _, err := qp.Write([]byte(body)); err != nil {
		return Composed{}, err
	}
	if err := qp.Close(); err != nil {
		return Composed{}, err
	}
	b.WriteString("\r\n")
	rcpts := make([]string, 0, len(to)+len(cc))
	for _, a := range append(to, cc...) {
		rcpts = append(rcpts, a.Address)
	}
	return Composed{Raw: b.Bytes(), Recipients: rcpts}, nil
}

// addresses parses a comma-separated list the way people write it, bare
// addresses included.
func addresses(s string) ([]*mail.Address, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if strings.ContainsAny(s, "\r\n") {
		return nil, fmt.Errorf("an address list can't span lines: %q", s)
	}
	if list, err := mail.ParseAddressList(s); err == nil {
		return list, nil
	}
	// "a@x.com; b@y.com" and other near misses: one address at a time.
	var out []*mail.Address
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		a, err := mail.ParseAddress(part)
		if err != nil {
			return nil, fmt.Errorf("%q isn't an email address I can send to", part)
		}
		out = append(out, a)
	}
	return out, nil
}

func joinAddrs(as []*mail.Address) string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.String()
	}
	return strings.Join(out, ", ")
}

// oneLine keeps a header value on one line: a line break in a subject is how
// another header gets smuggled in.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ").Replace(s)), " ")
}

// msgID puts a message id in the angle brackets headers need ("" if there
// is none or it isn't one).
func msgID(id string) string {
	id = strings.Trim(oneLine(id), "<>")
	if id == "" || strings.ContainsAny(id, " <>") {
		return ""
	}
	return "<" + id + ">"
}

// idDomain is the domain a Message-ID is made in: the sender's, else the
// one given, else none.
func idDomain(from, fallback string) string {
	for _, s := range []string{from, fallback} {
		s = oneLine(s)
		if i := strings.LastIndexByte(s, '@'); i >= 0 {
			s = s[i+1:]
		}
		if s != "" && !strings.ContainsAny(s, " <>") {
			return s
		}
	}
	return ""
}

func messageID(domain string) string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(buf), domain)
}
