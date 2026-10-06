package google

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"

	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
)

func mailbox(t *testing.T) (*Auth, *googletest.Fake) {
	t.Helper()
	a, fake := connected(t)
	reply := googletest.Headers(googletest.Part("multipart/mixed", "", "", nil, 0,
		googletest.Part("multipart/alternative", "", "", nil, 0,
			googletest.Part("text/plain", "text/plain; charset=ISO-8859-1", "", []byte("Gr\xfc\xdfe! Thursday works.\r\n\r\nOn Mon, 3 Oct 2026, Sam <sam@example.com> wrote:\r\n> Lunch?\r\n"), 0),
			googletest.Part("text/html", "text/html; charset=UTF-8", "", []byte("<div>Grüße! Thursday works.</div>"), 0),
		),
		googletest.Part("application/pdf", "application/pdf", "invoice.pdf", nil, 245_000),
	), "From", "Sarah Smith <sarah@example.com>", "To", "sam@example.com", "Subject", "Re: Lunch", "Date", "Mon, 3 Oct 2026 10:02:00 +1100", "Message-ID", "<orig-1@mail.example.com>")
	fake.AddMessage(googletest.Message{ID: "m1", ThreadID: "t1", Labels: []string{"INBOX", "UNREAD"}, Snippet: "Grüße! Thursday &amp; more", Payload: reply})

	long := &gmail.MessagePart{MimeType: "text/html", Headers: []*gmail.MessagePartHeader{{Name: "Content-Type", Value: "text/html; charset=utf-8"}, {Name: "From", Value: "news@shop.example"}, {Name: "Subject", Value: "Sale"}},
		Body: &gmail.MessagePartBody{AttachmentId: "big-html", Size: 90_000}}
	fake.AddAttachment("big-html", []byte("<p>Everything is 20% off &mdash; until Sunday.</p>"))
	fake.AddMessage(googletest.Message{ID: "m2", ThreadID: "t2", Labels: []string{"INBOX"}, Payload: long})
	return a, fake
}

func TestGmailReadGivesTheModelReadableText(t *testing.T) {
	a, _ := mailbox(t)
	out, err := runTool(t, a.GmailTools(), "gmail_read", map[string]any{"id": "m1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"From: Sarah Smith <sarah@example.com>", "Subject: Re: Lunch", "Grüße! Thursday works.", "earlier messages in this thread left out", "Attachments: invoice.pdf (PDF, 239 KB)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"> Lunch?", "<div>", "�"} {
		if strings.Contains(out, bad) {
			t.Errorf("kept %q in:\n%s", bad, out)
		}
	}
	full, _ := runTool(t, a.GmailTools(), "gmail_read", map[string]any{"id": "m1", "full": true})
	if !strings.Contains(full, "> Lunch?") {
		t.Errorf("full lost the thread:\n%s", full)
	}
	// A long part Gmail keeps aside is fetched and read.
	out, err = runTool(t, a.GmailTools(), "gmail_read", map[string]any{"id": "m2"})
	if err != nil || !strings.Contains(out, "Everything is 20% off — until Sunday.") {
		t.Fatalf("long part: %v\n%s", err, out)
	}
	list, _ := runTool(t, a.GmailTools(), "gmail_search", map[string]any{"query": "in:inbox"})
	if !strings.Contains(list, "Thursday & more") || !strings.Contains(list, "(unread)") {
		t.Errorf("search:\n%s", list)
	}
}

func TestGmailReplyThreadsProperly(t *testing.T) {
	a, fake := mailbox(t)
	out, err := runTool(t, a.GmailTools(), "gmail_reply", map[string]any{"id": "m1", "body": "See you then.\nSam"})
	if err != nil || !strings.Contains(out, "sent to sarah@example.com") {
		t.Fatalf("reply: %v %s", err, out)
	}
	sent := fake.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d", len(sent))
	}
	msg := string(sent[0])
	for _, want := range []string{"To: \"Sarah Smith\" <sarah@example.com>\r\n", "Subject: Re: Lunch\r\n", "In-Reply-To: <orig-1@mail.example.com>\r\n", "See you then.\r\nSam"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	// Gmail writes the sender itself, with the name the owner set in Gmail
	// ("Sam Smith <sam@…>"); a bare address from here would replace it.
	if strings.Contains(msg, "From:") {
		t.Errorf("a From header was sent, hiding the owner's name:\n%s", msg)
	}
	if n := fake.Calls("/oauth2/v2/userinfo"); n != 0 {
		t.Errorf("sending asked Google who the owner is (%d calls)", n)
	}
}

// Gmail is a watch source: unread inbox mail, each fetched once.
func TestGmailWatchSeesUnreadMailAndFetchesEachOnce(t *testing.T) {
	a, fake := mailbox(t)
	w := a.GmailWatch()
	if w != a.GmailWatch() {
		t.Fatal("one watch per account")
	}
	snap, err := w.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 1 || snap["m1"] != "unread from Sarah Smith: Re: Lunch" {
		t.Fatalf("snapshot: %v", snap)
	}
	gets := fake.Calls("/gmail/v1/users/me/messages/m1")
	if _, err := w.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Calls("/gmail/v1/users/me/messages/m1") != gets {
		t.Fatal("a message already seen was fetched again")
	}
	fake.SignOut()
	a.expireAccess()
	if _, err := w.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "signed Mirrin out") {
		t.Fatalf("signed out: %v", err)
	}
}
