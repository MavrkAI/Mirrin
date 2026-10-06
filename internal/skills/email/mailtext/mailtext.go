// Package mailtext turns the parts of an email into what a person would
// read: transfer encodings undone, charsets converted to UTF-8, HTML made
// into plain text, quoted history and signatures set aside, and attachments
// named instead of pasted. IMAP (the email skill) and Gmail share it, so the
// model reads mail the same way whichever way it arrives.
package mailtext

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"mime"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message/charset"
)

// Attachment is a file carried by a message, named rather than read.
type Attachment struct {
	Name string
	Type string // MIME type, e.g. application/pdf
	Size int64  // bytes, decoded
}

// Decode undoes a part's transfer encoding (base64, quoted-printable) and
// converts its charset to UTF-8. It never fails: what can't be decoded is
// kept as it came, and bytes that are not text in any charset are replaced.
func Decode(data []byte, transferEncoding, cs string) string {
	switch strings.ToLower(strings.TrimSpace(transferEncoding)) {
	case "base64":
		data = decodeBase64(data)
	case "quoted-printable":
		data = decodeQP(data)
	}
	return ToUTF8(data, cs)
}

// ToUTF8 converts text in the named charset to UTF-8. An unknown or wrong
// label falls back to the likeliest reading: UTF-8 if the bytes are valid
// UTF-8, else Windows-1252 (what "Latin-1" mail usually really is).
func ToUTF8(data []byte, cs string) string {
	cs = strings.ToLower(strings.Trim(strings.TrimSpace(cs), `"'`))
	switch cs {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		if utf8.Valid(data) {
			return string(data)
		}
		cs = "windows-1252"
	}
	if r, err := charset.Reader(cs, bytes.NewReader(data)); err == nil {
		if out, err := io.ReadAll(r); err == nil {
			return strings.ToValidUTF8(string(out), "\uFFFD")
		}
	}
	if utf8.Valid(data) {
		return string(data)
	}
	if r, err := charset.Reader("windows-1252", bytes.NewReader(data)); err == nil {
		if out, err := io.ReadAll(r); err == nil {
			return string(out)
		}
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

// decodeBase64 decodes as much as it can: line breaks and stray characters
// are skipped, and a body cut short (a size limit) still gives its start.
func decodeBase64(data []byte) []byte {
	clean := make([]byte, 0, len(data))
	for _, c := range data {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			clean = append(clean, c)
		case c == '-':
			clean = append(clean, '+') // URL-safe alphabet
		case c == '_':
			clean = append(clean, '/')
		}
	}
	if len(clean)%4 == 1 { // a lone character can't be decoded: cut short
		clean = clean[:len(clean)-1]
	}
	out := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
	n, _ := base64.RawStdEncoding.Decode(out, clean)
	return out[:n]
}

// decodeQP decodes quoted-printable leniently: a "=" that doesn't start an
// escape is kept as it is (real mail has plenty), and soft line breaks join.
func decodeQP(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c != '=' {
			out = append(out, c)
			continue
		}
		rest := data[i+1:]
		switch {
		case bytes.HasPrefix(rest, []byte("\r\n")):
			i += 2
		case bytes.HasPrefix(rest, []byte("\n")):
			i++
		case len(rest) >= 2 && isHex(rest[0]) && isHex(rest[1]):
			out = append(out, unhex(rest[0])<<4|unhex(rest[1]))
			i += 2
		default:
			// "=" followed by spaces then a line break is a soft break too.
			j := 0
			for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
				j++
			}
			if j > 0 && j < len(rest) && (rest[j] == '\n' || rest[j] == '\r') {
				i += j
				if rest[j] == '\r' && j+1 < len(rest) && rest[j+1] == '\n' {
					i++
				}
				i++
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

var (
	reComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	reInvis    = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head|title|xml|template)\b[^>]*>.*?</(script|style|noscript|svg|head|title|xml|template)\s*>`)
	reHidden   = regexp.MustCompile(`(?is)<(div|span|p|td|table|tr)\b[^>]*\bstyle\s*=\s*("[^"]*display\s*:\s*none[^"]*"|'[^']*display\s*:\s*none[^']*')[^>]*>.*?</(div|span|p|td|table|tr)\s*>`)
	reBreak    = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|h[1-6]|tr|table|blockquote|pre|section|article|header|footer|ul|ol)\s*>|<(p|div|tr|table|h[1-6]|ul|ol|blockquote|section|article|header|footer|hr)\b[^>]*>`)
	reItem     = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	reCell     = regexp.MustCompile(`(?i)</t[dh]\s*>`)
	reTag      = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces   = regexp.MustCompile(`[ \t\f\v]+`)
	reManyNL   = regexp.MustCompile(`\n{3,}`)
	invisibles = strings.NewReplacer("\u200b", "", "\u200c", "", "\u200d", "", "\u2060", "", "\ufeff", "", "\u00ad", "", "\u034f", "", "\u00a0", " ", "\r", "")
)

// HTMLText turns an HTML email into plain text a person would read: hidden
// preview text, styles and tracking markup go, paragraphs and list items
// keep their shape, and every entity is decoded.
func HTMLText(s string) string {
	s = reComment.ReplaceAllString(s, " ")
	s = reInvis.ReplaceAllString(s, " ")
	s = reHidden.ReplaceAllString(s, " ")
	s = reItem.ReplaceAllString(s, "\n• ")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reCell.ReplaceAllString(s, " ")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return tidy(s)
}

// tidy trims each line, drops invisible padding, and keeps at most one
// blank line in a row.
func tidy(s string) string {
	s = invisibles.Replace(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(reSpaces.ReplaceAllString(l, " "))
	}
	s = strings.Join(lines, "\n")
	s = reManyNL.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// Text is the readable text of a part of the given media type: HTML is
// turned into text, plain text is tidied.
func Text(decoded, mediaType string) string {
	if strings.EqualFold(strings.TrimSpace(mediaType), "text/html") {
		return HTMLText(decoded)
	}
	return tidy(decoded)
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charset.Reader}

// Header decodes an RFC 2047 header value ("=?UTF-8?Q?M=C3=A4rz?=") in any
// charset. A value that isn't encoded is returned unchanged.
func Header(s string) string {
	if out, err := wordDecoder.DecodeHeader(s); err == nil {
		return out
	}
	return s
}

// Filename decodes an attachment name given as a MIME parameter, including
// the RFC 2231 form (name*=UTF-8”Rechnung%20M%C3%A4rz.pdf).
func Filename(params map[string]string) string {
	for _, key := range []string{"filename*", "name*"} {
		if v := params[key]; v != "" {
			if parts := strings.SplitN(v, "'", 3); len(parts) == 3 {
				if raw, err := url.PathUnescape(parts[2]); err == nil {
					return ToUTF8([]byte(raw), parts[0])
				}
			}
		}
	}
	for _, key := range []string{"filename", "name"} {
		if v := params[key]; v != "" {
			return Header(v)
		}
	}
	return ""
}

// Size says how big a file is the way a person would: 812 bytes, 240 KB, 3.1 MB.
func Size(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", (n+512)/1024)
	case n < 10*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
	return fmt.Sprintf("%d MB", (n+512*1024)/(1024*1024))
}

// Kind names a MIME type the way a person would: PDF, image, spreadsheet.
func Kind(mediaType string) string {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case mt == "application/pdf":
		return "PDF"
	case strings.HasPrefix(mt, "image/"):
		return "image"
	case strings.HasPrefix(mt, "audio/"):
		return "audio"
	case strings.HasPrefix(mt, "video/"):
		return "video"
	case mt == "text/calendar" || mt == "application/ics":
		return "calendar invite"
	case strings.Contains(mt, "spreadsheet") || strings.Contains(mt, "excel") || mt == "text/csv":
		return "spreadsheet"
	case strings.Contains(mt, "wordprocessing") || strings.Contains(mt, "msword") || strings.Contains(mt, "opendocument.text"):
		return "document"
	case strings.Contains(mt, "presentation") || strings.Contains(mt, "powerpoint"):
		return "slides"
	case mt == "application/zip" || strings.Contains(mt, "compressed"):
		return "archive"
	case mt == "message/rfc822":
		return "email"
	}
	if i := strings.IndexByte(mt, '/'); i >= 0 {
		return mt[i+1:]
	}
	return "file"
}

// Attachments lists attachments on one line: "invoice.pdf (PDF, 240 KB), …".
func Attachments(atts []Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(atts))
	for _, a := range atts {
		name := a.Name
		if name == "" {
			name = "unnamed"
		}
		desc := Kind(a.Type)
		if a.Size > 0 {
			desc += ", " + Size(a.Size)
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", name, desc))
	}
	return "Attachments: " + strings.Join(parts, "; ")
}

// Truncate cuts s to at most max bytes on a character boundary, saying so.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " \n") + "\n…[truncated]"
}

// Render is what the model reads of a message body: the text (with quoted
// history and signatures set aside unless full), a note of what was left
// out, and the attachments.
func Render(text string, atts []Attachment, full bool, max int) string {
	text = strings.TrimSpace(text)
	note := ""
	if !full {
		var quoted, signature int
		text, quoted, signature = Clean(text)
		switch {
		case quoted > 0 && signature > 0:
			note = fmt.Sprintf("[earlier messages in this thread left out (%d lines), and the signature; read again with full: true to see them]", quoted)
		case quoted > 0:
			note = fmt.Sprintf("[earlier messages in this thread left out (%d lines); read again with full: true to see them]", quoted)
		case signature > 0:
			note = "[signature left out; read again with full: true to see it]"
		}
	}
	if max > 0 {
		text = Truncate(text, max)
	}
	if text == "" {
		text = "(no text in this email)"
	}
	var b strings.Builder
	b.WriteString(text)
	if note != "" {
		b.WriteString("\n\n" + note)
	}
	if a := Attachments(atts); a != "" {
		b.WriteString("\n\n" + a)
	}
	return b.String()
}
