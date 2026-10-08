package google

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
)

// MailWith finds the recent mail to or from one person, reads it without
// the quoted thread, and leaves out what Gmail's loose search let through
// but doesn't name them.
func TestMailWithReadsOnlyThatPersonsMail(t *testing.T) {
	a, fake := connected(t)
	body := func(id, text string, kv ...string) googletest.Message {
		return googletest.Message{ID: id, ThreadID: "t-" + id, Payload: googletest.Headers(googletest.Part("text/plain", "text/plain; charset=utf-8", "", []byte(text), 0), kv...)}
	}
	fake.AddMessage(body("m1", "I'll send the Q3 numbers before we meet.\r\n\r\nOn Mon, 5 Oct 2026, Priya <priya@example.com> wrote:\r\n> Can you bring the Q3 numbers?\r\n",
		"From", "Me <me@example.com>", "To", "Priya Shah <Priya@Example.com>", "Subject", "Re: Thursday", "Date", "Mon, 5 Oct 2026 09:00:00 +0100"))
	fake.AddMessage(body("m2", "Lunch?", "From", "pam@example.com", "To", "me@example.com", "Subject", "Lunch"))

	got, err := a.MailWith(context.Background(), "priya@example.com", 30*24*time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "m1" || got[0].Subject != "Re: Thursday" {
		t.Fatalf("mail with Priya: %+v", got)
	}
	if !strings.Contains(got[0].Text, "send the Q3 numbers") || strings.Contains(got[0].Text, "> Can you") {
		t.Fatalf("text: %q", got[0].Text)
	}
	if q := fake.Queries(); len(q) != 1 || !strings.Contains(q[0], "from:priya@example.com") || !strings.Contains(q[0], "newer_than:30d") {
		t.Fatalf("searched for %q", q)
	}
	// Something that isn't an address is never put into a search.
	if got, err := a.MailWith(context.Background(), "x OR in:anywhere", time.Hour, 3); got != nil || err != nil || len(fake.Queries()) != 1 {
		t.Fatalf("a bad address searched: %v %v %q", got, err, fake.Queries())
	}
}

func TestNamesMatchesWholeAddressesOnly(t *testing.T) {
	for _, c := range []struct {
		list string
		want bool
	}{
		{"Sam <sam@example.com>, pam@example.com", true},
		{"pam@example.com", false},
		{"undisclosed-recipients:;", false},
		{"\"broken <sam@example.com>", true},
		{"\"broken <xsam@example.com>", false},
	} {
		if got := names("sam@example.com", c.list); got != c.want {
			t.Errorf("names(%q) = %v, want %v", c.list, got, c.want)
		}
	}
}
