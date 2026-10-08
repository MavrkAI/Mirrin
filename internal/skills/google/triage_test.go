package google

import (
	"context"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
)

// A first look at the inbox reads the newest inbox mail, with who, what
// and a snippet, and changes nothing: no label is touched, nothing is sent.
func TestRecentInboxOnlyReads(t *testing.T) {
	a, fake := connected(t)
	fake.AddMessage(googletest.Message{ID: "old", Labels: []string{"INBOX"}, Snippet: "Minutes attached",
		Payload: googletest.Headers(googletest.Part("text/plain", "text/plain", "", []byte("x"), 0), "From", "Board <board@example.com>", "Subject", "Minutes")})
	fake.AddMessage(googletest.Message{ID: "new", ThreadID: "t9", Labels: []string{"INBOX", "UNREAD"}, Snippet: "Can you sign by Friday &amp; send it back?",
		Payload: googletest.Headers(googletest.Part("text/plain", "text/plain", "", []byte("x"), 0), "From", "Sam Lee <sam@example.com>", "Subject", " The lease ", "Date", "Mon, 5 Oct 2026 09:00:00 +0100")})
	fake.AddMessage(googletest.Message{ID: "archived", Labels: []string{"UNREAD"}, Payload: googletest.Headers(googletest.Part("text/plain", "text/plain", "", []byte("x"), 0), "From", "x@example.com", "Subject", "Not in the inbox")})

	got, err := a.RecentInbox(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "new" || got[1].ID != "old" {
		t.Fatalf("got %+v", got)
	}
	m := got[0]
	if m.From != "Sam Lee <sam@example.com>" || m.Subject != "The lease" || !m.Unread || m.ThreadID != "t9" || m.Snippet != "Can you sign by Friday & send it back?" {
		t.Fatalf("mail %+v", m)
	}
	if got[1].Unread {
		t.Fatal("read mail marked unread")
	}
	if n := fake.Calls("/gmail/v1/users/me/messages/send"); n != 0 || len(fake.Sent()) != 0 {
		t.Fatal("a look at the inbox sent mail")
	}
	for _, id := range []string{"new", "old"} {
		if fake.Calls("/gmail/v1/users/me/messages/"+id+"/modify") != 0 {
			t.Fatalf("a look at the inbox changed %s", id)
		}
	}
	if SenderName(m.From) != "Sam Lee" {
		t.Fatal(SenderName(m.From))
	}
}
