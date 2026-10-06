package email

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
)

func selfMail(id, to, subject string) string {
	return "From: Sam <sam@example.com>\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Mon, 03 Oct 2026 10:02:00 +1100\r\n" +
		"Message-ID: <" + id + ">\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"What's on tomorrow?\r\n"
}

func (m *mailbox) put(t *testing.T, box, raw string, at time.Time) {
	t.Helper()
	if _, err := m.user.Append(box, bytes.NewReader([]byte(raw)), &imap.AppendOptions{Time: at}); err != nil {
		t.Fatal(err)
	}
}

// Mail the mailbox sends itself carries no header vouching for it; its copy
// in Sent does.
func TestMailTheMailboxSentItselfIsFoundInSent(t *testing.T) {
	m := newMailbox(t)
	_, mark, err := m.client.UnreadAfter(Mark{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m.put(t, "Sent", selfMail("self-1@example.com", "sam@example.com", "Plans"), now)
	m.put(t, "INBOX", selfMail("self-1@example.com", "sam@example.com", "Plans"), now)
	m.put(t, "INBOX", selfMail("forged-1@example.com", "sam@example.com", "Plans"), now) // not in Sent
	m.put(t, "Sent", selfMail("old-1@example.com", "sam@example.com", "Plans"), now.Add(-time.Hour))
	m.put(t, "INBOX", selfMail("old-1@example.com", "sam@example.com", "Plans"), now) // sent long ago: a replay
	m.put(t, "Sent", selfMail("other-1@example.com", "friend@example.org", "Plans"), now)
	m.put(t, "INBOX", selfMail("other-1@example.com", "sam@example.com", "Plans"), now) // Sent copy went to someone else
	m.put(t, "Sent", selfMail("subj-1@example.com", "sam@example.com", "Plans"), now)
	m.put(t, "INBOX", selfMail("subj-1@example.com", "sam@example.com", "Wire the money"), now)
	msgs, _, err := m.client.UnreadAfter(mark, 20)
	if err != nil {
		t.Fatal(err)
	}
	var owner []string
	for _, in := range msgs {
		ok, err := m.client.SelfSent(in)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			owner = append(owner, in.MessageID)
		}
	}
	if !slices.Equal(owner, []string{"self-1@example.com"}) {
		t.Fatalf("taken as sent from the mailbox itself: %v", owner)
	}

	// A second copy in the inbox is someone replaying the Message-ID.
	m.put(t, "INBOX", selfMail("self-1@example.com", "sam@example.com", "Plans"), now)
	if ok, _ := m.client.SelfSent(msgs[0]); ok {
		t.Fatal("a replayed Message-ID was accepted")
	}
}

// The twin's own replies to its own mailbox are its words coming back.
func TestTheTwinsOwnMailIsNotTheOwner(t *testing.T) {
	m := newMailbox(t)
	composed, err := mailtext.Compose(mailtext.Outgoing{From: "sam@example.com", To: "sam@example.com", Subject: "Plans", Body: "Nothing until noon."})
	if err != nil {
		t.Fatal(err)
	}
	_, mark, _ := m.client.UnreadAfter(Mark{}, 0)
	m.put(t, "Sent", string(composed.Raw), time.Now())
	m.put(t, "INBOX", string(composed.Raw), time.Now())
	msgs, _, err := m.client.UnreadAfter(mark, 20)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("%v %v", msgs, err)
	}
	if ok, _ := m.client.SelfSent(msgs[0]); ok {
		t.Fatal("the twin's own reply was taken for the owner")
	}
}

func TestTraceKeepsReceivedAndAuthResultsInOrder(t *testing.T) {
	raw := "Received: from a by mx.example.org;\r\n\tMon, 3 Oct 2026\r\n" +
		"Authentication-Results: mx.example.org; spf=pass\r\n" +
		"Subject: hi\r\n" +
		"Authentication-Results: mx.example.org; dkim=pass\r\n" +
		"received: by mail.sender.example\r\n\r\n" +
		"Authentication-Results: body; not a header\r\n"
	got := traceFields([]byte(raw))
	want := []Field{
		{"Received", "from a by mx.example.org; Mon, 3 Oct 2026"},
		{"Authentication-Results", "mx.example.org; spf=pass"},
		{"Authentication-Results", "mx.example.org; dkim=pass"},
		{"Received", "by mail.sender.example"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}

	// The mail channel gets them from the whole header of each message.
	m := newMailbox(t)
	_, mark, _ := m.client.UnreadAfter(Mark{}, 0)
	m.put(t, "INBOX", "Authentication-Results: mx.example.org; spf=pass smtp.mailfrom=me@example.com\r\n"+
		"Authentication-Results: mx.example.org; dkim=pass header.d=example.com\r\n"+
		"Received: from out.example.com by mx.example.org\r\n"+selfMail("t-1@example.com", "sam@example.com", "hi"), time.Now())
	msgs, _, err := m.client.UnreadAfter(mark, 20)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("%v %v", msgs, err)
	}
	if len(msgs[0].Trace) != 3 || msgs[0].Trace[2].Name != "Received" || len(msgs[0].AuthResults) != 2 {
		t.Fatalf("trace %q, auth %q", msgs[0].Trace, msgs[0].AuthResults)
	}
}
