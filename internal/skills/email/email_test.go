package email

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// A reply from Gmail: plain and HTML versions (quoted-printable and
// base64), quoted history under the reply, and a PDF attached.
const gmailReply = "From: Sarah Smith <sarah@example.com>\r\n" +
	"To: Sam <sam@example.com>\r\n" +
	"Subject: =?UTF-8?Q?Re:_Caf=C3=A9_on_Thursday?=\r\n" +
	"Date: Mon, 03 Oct 2026 10:02:00 +1100\r\n" +
	"Message-ID: <abc123@mail.example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"mixed\"\r\n" +
	"\r\n" +
	"--mixed\r\n" +
	"Content-Type: multipart/alternative; boundary=\"alt\"\r\n" +
	"\r\n" +
	"--alt\r\n" +
	"Content-Type: text/plain; charset=\"UTF-8\"\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"Thursday works =E2=80=94 the caf=C3=A9 on King St at 12:30?\r\n" +
	"The invoice is attached.\r\n" +
	"\r\n" +
	"On Sun, 2 Oct 2026 at 18:00, Sam <sam@example.com> wrote:\r\n" +
	"> Lunch this week? I owe you one.\r\n" +
	"> Sam\r\n" +
	"\r\n" +
	"--alt\r\n" +
	"Content-Type: text/html; charset=\"UTF-8\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"PGRpdj5UaHVyc2RheSB3b3JrcyAmbWRhc2g7IHRoZSBjYWbDqSBvbiBLaW5nIFN0IGF0IDEyOjMw\r\n" +
	"PzwvZGl2Pg==\r\n" +
	"--alt--\r\n" +
	"\r\n" +
	"--mixed\r\n" +
	"Content-Type: application/pdf; name=\"invoice-4411.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"invoice-4411.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQKJcOkw7zDtsOfCjIgMCBvYmoKPDwvTGVuZ3RoIDMgMCBSL0ZpbHRlci9GbGF0ZURl\r\n" +
	"Y29kZT4+CnN0cmVhbQp4nD2OywoCMQxF9/mKu3YRk7bptDAIDuh+oOAP+AAXgrOZ37etjmSTe3IS\r\n" +
	"--mixed--\r\n"

// A newsletter: HTML only, Windows-1252, quoted-printable, a hidden preview.
const htmlOnly = "From: Jetstar <noreply@jetstar.com>\r\n" +
	"To: sam@example.com\r\n" +
	"Subject: Your flight has changed\r\n" +
	"Date: Mon, 03 Oct 2026 11:00:00 +1100\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/html; charset=windows-1252\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"<html><head><style>.x{color:red}</style></head><body><div style=3D\"display:=\r\n" +
	"none\">Preview you never see</div><p>Hi Sam,</p><p>JQ501 now leaves at <b>10:=\r\n" +
	"40</b> =96 we=92re sorry.</p></body></html>\r\n"

// Latin-1 subject and body, base64, not multipart.
const latin1 = "From: =?ISO-8859-1?Q?J=FCrgen?= <jurgen@example.de>\r\n" +
	"To: sam@example.com\r\n" +
	"Subject: =?ISO-8859-1?Q?Gr=FC=DFe?=\r\n" +
	"Date: Mon, 03 Oct 2026 12:00:00 +0200\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=iso-8859-1\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"R3L832UgYXVzIE38bmNoZW4h\r\n"

// A receipt forwarded as an attachment, with a logo embedded in the HTML.
const forwarded = "From: Sam <sam@example.com>\r\n" +
	"To: sam@example.com\r\n" +
	"Subject: Fwd: Receipt\r\n" +
	"Date: Mon, 03 Oct 2026 13:00:00 +1100\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"m\"\r\n" +
	"\r\n" +
	"--m\r\n" +
	"Content-Type: multipart/related; boundary=\"r\"\r\n" +
	"\r\n" +
	"--r\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>For the refund.</p><img src=\"cid:logo\">\r\n" +
	"--r\r\n" +
	"Content-Type: image/png\r\n" +
	"Content-ID: <logo>\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"iVBORw0KGgo=\r\n" +
	"--r--\r\n" +
	"--m\r\n" +
	"Content-Type: message/rfc822\r\n" +
	"\r\n" +
	"From: Shop <orders@shop.example>\r\n" +
	"Subject: Receipt 889\r\n" +
	"Date: Sun, 02 Oct 2026 09:00:00 +1100\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Order 889: 1 kettle, $49.00. Paid by card.\r\n" +
	"--m--\r\n"

type mailbox struct {
	client *Client
	user   *imapmemserver.User
}

func newMailbox(t *testing.T, msgs ...string) *mailbox {
	t.Helper()
	return newMailboxWith(t, nil, msgs...)
}

// newMailboxWith is newMailbox with the server's sessions wrapped, to make
// the server misbehave.
func newMailboxWith(t *testing.T, wrap func(imapserver.Session) imapserver.Session, msgs ...string) *mailbox {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("sam@example.com", "pw")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if err := user.Create("Sent", &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrSent}}); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	for _, m := range msgs {
		if _, err := user.Append("INBOX", bytes.NewReader([]byte(m)), &imap.AppendOptions{Time: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			if wrap != nil {
				return wrap(mem.NewSession()), nil, nil
			}
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		InsecureAuth: true,
		Logger:       discard{},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	c := New(config.Email{Username: "sam@example.com", SMTPHost: "smtp.example.com", SMTPPort: 587}, "pw", time.UTC)
	c.dial = func(opts *imapclient.Options) (*imapclient.Client, error) {
		return imapclient.DialInsecure(ln.Addr().String(), opts)
	}
	return &mailbox{client: c, user: user}
}

type discard struct{}

func (discard) Printf(string, ...any) {}

func (m *mailbox) run(t *testing.T, name string, input any) string {
	t.Helper()
	for _, tl := range m.client.Tools() {
		if tl.Spec().Name == name {
			raw, _ := json.Marshal(input)
			out, err := tl.Run(context.Background(), tools.Call{Input: raw})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return out
		}
	}
	t.Fatalf("no tool %s", name)
	return ""
}

func assertReads(t *testing.T, out string, want, never []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	for _, n := range never {
		if strings.Contains(out, n) {
			t.Errorf("should not contain %q:\n%s", n, out)
		}
	}
}

// MIME boundaries, base64 and quoted-printable must never reach the model.
var mimeJunk = []string{"Content-Type", "Content-Transfer-Encoding", "--alt", "--mixed", "=E2=80", "=C3=A9", "JVBERi0", "PGRpdj", "=\r\n"}

func TestReadEmailGivesTheModelReadableText(t *testing.T) {
	m := newMailbox(t, gmailReply, htmlOnly, latin1, forwarded)

	out := m.run(t, "read_email", map[string]any{"uid": 1})
	assertReads(t, out, []string{
		"From: Sarah Smith <sarah@example.com>",
		"Subject: Re: Café on Thursday",
		"Thursday works — the café on King St at 12:30?",
		"The invoice is attached.",
		"earlier messages in this thread left out",
		"Attachments: invoice-4411.pdf (PDF,",
	}, append(mimeJunk, "Lunch this week", "<div>"))

	full := m.run(t, "read_email", map[string]any{"uid": 1, "full": true})
	assertReads(t, full, []string{"> Lunch this week? I owe you one."}, mimeJunk)

	out = m.run(t, "read_email", map[string]any{"uid": 2})
	assertReads(t, out, []string{"Hi Sam,", "JQ501 now leaves at 10:40 – we’re sorry."}, append(mimeJunk, "Preview you never see", "color:red", "<p>", "=96"))

	out = m.run(t, "read_email", map[string]any{"uid": 3})
	assertReads(t, out, []string{"From: Jürgen <jurgen@example.de>", "Subject: Grüße", "Grüße aus München!"}, []string{"R3L8", "=?ISO"})

	out = m.run(t, "read_email", map[string]any{"uid": 4})
	assertReads(t, out, []string{"For the refund.", "Forwarded message", "From: Shop <orders@shop.example>", "Subject: Receipt 889", "Order 889: 1 kettle, $49.00."}, []string{"image", "iVBOR", "Attachments"})

	list := m.run(t, "list_emails", map[string]any{"count": 10})
	assertReads(t, list, []string{"Re: Café on Thursday", "Grüße", "Jürgen"}, []string{"=?"})
}

func TestUnreadAfterDecodesBodiesForTheMailChannel(t *testing.T) {
	m := newMailbox(t)
	_, mark, err := m.client.UnreadAfter(Mark{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{gmailReply, htmlOnly} {
		if _, err := m.user.Append("INBOX", bytes.NewReader([]byte(raw)), &imap.AppendOptions{Time: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _, err := m.client.UnreadAfter(mark, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	assertReads(t, msgs[0].Body, []string{"Thursday works — the café", "> Lunch this week?"}, mimeJunk)
	if len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].Name != "invoice-4411.pdf" {
		t.Errorf("attachments: %+v", msgs[0].Attachments)
	}
	if msgs[0].MessageID != "abc123@mail.example.com" || msgs[0].Subject != "Re: Café on Thursday" {
		t.Errorf("envelope: %+v", msgs[0])
	}
	assertReads(t, msgs[1].Body, []string{"JQ501 now leaves at 10:40"}, append(mimeJunk, "<p>"))
}

func TestRepliesThreadAndNothingCanSmuggleAHeader(t *testing.T) {
	m := newMailbox(t, gmailReply)
	var sent []byte
	var rcpts []string
	m.client.smtpSend = func(_ string, _ smtp.Auth, _ string, to []string, msg []byte) error {
		sent, rcpts = msg, to
		return nil
	}
	m.client.cfg.FromName = "Sam Smith"
	out := m.run(t, "reply_email", map[string]any{"uid": 1, "body": "Perfect, see you then.\nSam"})
	if !strings.Contains(out, "sent to sarah@example.com") {
		t.Fatalf("reply: %s", out)
	}
	msg := string(sent)
	assertReads(t, msg, []string{
		"In-Reply-To: <abc123@mail.example.com>\r\n",
		"References: <abc123@mail.example.com>\r\n",
		"Subject: =?utf-8?q?Re:_Caf=C3=A9_on_Thursday?=\r\n",
		"From: \"Sam Smith\" <sam@example.com>\r\n",
		"To: <sarah@example.com>\r\n",
		"Content-Transfer-Encoding: quoted-printable",
		"Perfect, see you then.\r\nSam",
		"Message-ID: <",
	}, nil)
	if len(rcpts) != 1 || rcpts[0] != "sarah@example.com" {
		t.Fatalf("recipients: %v", rcpts)
	}

	for _, tl := range m.client.Tools() {
		if tl.Spec().Name != "send_email" {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"to": "Jo <jo@example.com>; kim@example.com", "subject": "Plans\r\nBcc: spy@evil.example", "body": "hi"})
		if _, err := tl.Run(context.Background(), tools.Call{Input: raw}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(sent), "\r\nBcc:") || !strings.Contains(string(sent), "Subject: Plans Bcc: spy@evil.example\r\n") {
			t.Fatalf("header smuggled:\n%s", sent)
		}
		if strings.Join(rcpts, ",") != "jo@example.com,kim@example.com" {
			t.Fatalf("recipients: %v", rcpts)
		}
		raw, _ = json.Marshal(map[string]any{"to": "jo@example.com\r\nBcc: spy@evil.example", "subject": "x", "body": "hi"})
		if _, err := tl.Run(context.Background(), tools.Call{Input: raw}); err == nil {
			t.Fatal("an address with a line break was accepted")
		}
	}
}

// refusingSession is a server that won't give the text of one message.
type refusingSession struct {
	imapserver.Session
	uid imap.UID
}

func (s refusingSession) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, opts *imap.FetchOptions) error {
	if set, ok := numSet.(imap.UIDSet); ok && set.Contains(s.uid) && len(opts.BodySection) > 0 && len(opts.BodySection[0].Part) > 0 {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "message is damaged"}
	}
	return s.Session.Fetch(w, numSet, opts)
}

// One message whose text the server won't give doesn't stall the mail
// channel: the others arrive, and that one says it couldn't be read.
func TestOneUnreadableMessageDoesNotHoldUpTheRest(t *testing.T) {
	m := newMailboxWith(t, func(s imapserver.Session) imapserver.Session { return refusingSession{s, 2} })
	_, mark, err := m.client.UnreadAfter(Mark{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{gmailReply, htmlOnly, latin1} {
		if _, err := m.user.Append("INBOX", bytes.NewReader([]byte(raw)), &imap.AppendOptions{Time: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _, err := m.client.UnreadAfter(mark, 10)
	if err != nil {
		t.Fatalf("one bad message failed the batch: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if !strings.Contains(msgs[0].Body, "Thursday works") || msgs[1].Body != unreadable || !strings.Contains(msgs[2].Body, "Grüße aus München") {
		t.Fatalf("bodies: %q / %q / %q", msgs[0].Body, msgs[1].Body, msgs[2].Body)
	}
	out := m.run(t, "read_email", map[string]any{"uid": 2})
	if !strings.Contains(out, "couldn't be read") || !strings.Contains(out, "Subject: Your flight has changed") {
		t.Fatalf("read_email: %s", out)
	}
}
