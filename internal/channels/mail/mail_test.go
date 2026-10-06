package mail

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/skills/email"
)

// fake is a mailbox: polls hand out batches, and it records what was marked read.
type fake struct {
	mu      sync.Mutex
	trusted []string
	batches [][]email.Incoming
	marks   []email.Mark
	read    []uint32
	sent    []string
	inSent  map[string]bool // Message-IDs the mailbox has in Sent, sent to itself
	looks   int             // SelfSent calls: each is an IMAP login
}

func (f *fake) UnreadAfter(mark email.Mark, _ int) ([]email.Incoming, email.Mark, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks = append(f.marks, mark)
	next := email.Mark{Validity: 7, Next: mark.Next + 1}
	if mark.Next == 0 {
		return nil, email.Mark{Validity: 7, Next: 100}, nil
	}
	if len(f.batches) == 0 {
		return nil, mark, nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b, next, nil
}

func (f *fake) MarkRead(uids ...uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = append(f.read, uids...)
	return nil
}

func (f *fake) AuthServers() ([]string, bool) { return f.trusted, false }

func (f *fake) SelfSent(m email.Incoming) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.looks++
	return f.inSent[m.MessageID], nil
}

func (f *fake) Reply(to, subject, body, inReplyTo string) error {
	f.sent = append(f.sent, to+"|"+subject+"|"+inReplyTo)
	return nil
}

const gmailPass = "mx.google.com; dkim=pass header.i=@example.com header.s=20230601 header.b=AbC; " +
	"spf=pass (google.com: domain of me@example.com designates 209.85.220.41 as permitted sender) smtp.mailfrom=me@example.com; " +
	"dmarc=pass (p=REJECT sp=REJECT dis=NONE) header.from=example.com"

func TestMailRoundTrip(t *testing.T) {
	f := &fake{trusted: []string{"mx.google.com"}}
	c := New(f, "Me@Example.com", false, "", 0, nil)
	var got []channels.Inbound
	for _, m := range []email.Incoming{
		{UID: 1, From: "me@example.com", FromName: "Me", Subject: "Plans", MessageID: "<m1>", AuthResults: []string{gmailPass},
			Body: "What's on tomorrow?\n\nOn Mon, 3 Oct 2026 at 10:02, Mirrin <twin@example.com> wrote:\n> old stuff"},
		{UID: 2, From: "spam@example.com", Subject: "Buy", Body: "now"},
	} {
		if in, ok := c.accept(m); ok {
			got = append(got, in)
		}
	}
	if len(got) != 1 || got[0].Text != "What's on tomorrow?" || !got[0].IsOwner {
		t.Fatalf("got %+v", got)
	}
	if err := c.Send(context.Background(), "me@example.com", "Nothing until noon."); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0] != "me@example.com|Re: Plans|<m1>" {
		t.Fatalf("sent %v", f.sent)
	}
}

func TestOnlyVerifiedMailIsTheOwner(t *testing.T) {
	icloud := []string{
		`bimi.icloud.com; bimi=skipped reason="insufficient dmarc"`,
		"dmarc.icloud.com; dmarc=pass header.from=example.com",
		"dkim-verifier.icloud.com; dkim=pass (2048-bit key) header.d=example.com header.i=@example.com header.b=xyz",
		"spf.icloud.com; spf=pass (spf.icloud.com: domain of me@example.com designates 1.2.3.4 as permitted sender) smtp.mailfrom=me@example.com",
	}
	icloudNames := []string{"bimi.icloud.com", "dmarc.icloud.com", "dkim-verifier.icloud.com", "spf.icloud.com"}
	microsoft := "spf=pass (sender IP is 209.85.221.49) smtp.mailfrom=example.com; dkim=pass (signature was verified) header.d=example.com;dmarc=pass action=none header.from=example.com;compauth=pass reason=100"
	for _, tc := range []struct {
		name    string
		trusted []string
		headers []string
		owner   bool
	}{
		{"gmail pass", []string{"mx.google.com"}, []string{gmailPass}, true},
		{"no headers at all", []string{"mx.google.com"}, nil, false},
		{"forger's own server", []string{"mx.google.com"}, []string{"mail.evil.example; dmarc=pass header.from=example.com"}, false},
		{"forged pass under the real fail", []string{"mx.google.com"}, []string{
			"mx.google.com; spf=softfail smtp.mailfrom=me@example.com; dkim=none; dmarc=fail (p=NONE) header.from=example.com",
			"mx.google.com; spf=pass smtp.mailfrom=me@example.com; dkim=pass header.d=example.com; dmarc=pass header.from=example.com",
		}, false},
		{"signed by someone else", []string{"mx.google.com"}, []string{"mx.google.com; dkim=pass header.d=evil.example; spf=pass smtp.mailfrom=bounce@evil.example; dmarc=fail header.from=example.com"}, false},
		{"another sender at the domain", []string{"mx.google.com"}, []string{"mx.google.com; spf=pass smtp.mailfrom=other@example.com"}, false},
		{"signed by a subdomain", []string{"mx.google.com"}, []string{"mx.google.com; dkim=pass header.d=mail.example.com"}, true},
		{"nested comments and quotes", []string{"mx.google.com"}, []string{`mx.google.com; dkim=pass (good (nested) sig) header.d="example.com"`}, true},
		{"icloud splits its checks", icloudNames, icloud, true},
		{"microsoft leaves out its name", []string{""}, []string{microsoft}, true},
		{"nameless header when a name is expected", []string{"mx.google.com"}, []string{microsoft}, false},
		{"look-alike server name", []string{"mx.google.com"}, []string{"mx.google.com.evil.example; dmarc=pass header.from=example.com"}, false},
		// Below the receiving server's own header is what the sender wrote:
		// it can't supply a check the server left out.
		{"forged check the server left out", []string{"mx.google.com"}, []string{
			"mx.google.com; spf=pass smtp.mailfrom=bounce@evil.example",
			"mx.google.com; dkim=pass header.d=example.com",
		}, false},
		{"same, with the server's dmarc fail", []string{"mx.google.com"}, []string{
			"mx.google.com; spf=pass smtp.mailfrom=attacker@evil.example; dmarc=fail header.from=example.com",
			"mx.google.com; dkim=pass header.d=example.com",
		}, false},
		{"forged header under a new name at the same provider", []string{"example.org"}, []string{
			"mx1.example.org; spf=pass smtp.mailfrom=bounce@evil.example",
			"anything.example.org; dkim=pass header.d=example.com",
		}, false},
		{"forged nameless header under microsoft's", []string{""}, []string{
			"spf=pass (sender IP is 203.0.113.9) smtp.mailfrom=evil.example; dmarc=none action=none header.from=example.com",
			"dkim=pass (signature was verified) header.d=example.com",
		}, false},
		{"trusted header hidden under another server's", []string{"mx.google.com"}, []string{
			"mail.evil.example; spf=none",
			gmailPass,
		}, false},
		{"dmarc fail outweighs a forged pass at a split provider", []string{"bimi.icloud.com", "dmarc.icloud.com", "dkim-verifier.icloud.com", "spf.icloud.com"}, []string{
			"dmarc.icloud.com; dmarc=fail header.from=example.com",
			"spf.icloud.com; spf=fail smtp.mailfrom=me@example.com",
			"dkim-verifier.icloud.com; dkim=pass header.d=example.com",
		}, false},
	} {
		for _, others := range []bool{false, true} {
			c := New(&fake{trusted: tc.trusted}, "me@example.com", others, "", 0, nil)
			in, ok := c.accept(email.Incoming{From: "me@example.com", Subject: "hi", Body: "yes 12", AuthResults: tc.headers})
			if tc.owner && (!ok || !in.IsOwner) {
				t.Errorf("%s (reply_to_others=%v): want the owner, got %v %+v", tc.name, others, ok, in)
			}
			// Unverified mail in the owner's name is dropped, never answered as a stranger in the owner's thread.
			if !tc.owner && ok {
				t.Errorf("%s (reply_to_others=%v): forged mail got through as %+v", tc.name, others, in)
			}
		}
	}
	// Strangers are still answered, as strangers, when reply_to_others is on.
	c := New(&fake{trusted: []string{"mx.google.com"}}, "me@example.com", true, "", 0, nil)
	if in, ok := c.accept(email.Incoming{From: "friend@example.org", Subject: "hi", Body: "hello"}); !ok || in.IsOwner {
		t.Fatalf("stranger: %v %+v", ok, in)
	}
}

func TestIgnoredMailStaysUnread(t *testing.T) {
	f := &fake{trusted: []string{"mx.google.com"}, batches: [][]email.Incoming{{
		{UID: 100, From: "news@shop.example", Subject: "Sale", Body: "50% off"},
		{UID: 101, From: "me@example.com", Subject: "Groceries", Body: "milk", AuthResults: []string{gmailPass}},
		{UID: 102, From: "me@example.com", Subject: "[Mirrin] plans", Body: "what's on?", AuthResults: []string{gmailPass}},
	}}}
	c := New(f, "me@example.com", false, "[Mirrin]", 5*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan channels.Inbound, 4)
	done := make(chan error)
	go func() { done <- c.Start(ctx, func(_ context.Context, in channels.Inbound) { got <- in }) }()
	select {
	case in := <-got:
		if in.Text != "what's on?" || !in.IsOwner {
			t.Fatalf("got %+v", in)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tagged owner mail never arrived")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.read, []uint32{102}) {
		t.Fatalf("only the answered message may be marked read, marked %v", f.read)
	}
	// Start takes a baseline first (so old mail isn't answered), then polls from it.
	if len(f.marks) < 2 || f.marks[0] != (email.Mark{}) || f.marks[1] != (email.Mark{Validity: 7, Next: 100}) {
		t.Fatalf("marks %v", f.marks)
	}
}

func TestOwnerIsToldWhyTheirMailWasIgnored(t *testing.T) {
	c := New(&fake{}, "me@example.com", false, "", 0, nil)
	if w := c.Warning(); !strings.Contains(w, "skills.email.auth_servers") {
		t.Fatalf("a mailbox that can't check senders should say so: %q", w)
	}

	c = New(&fake{trusted: []string{"example.org"}}, "me@example.com", false, "", 0, nil)
	if w := c.Warning(); w != "" {
		t.Fatalf("nothing to fix yet, got %q", w)
	}
	for _, tc := range []struct {
		headers []string
		want    string
	}{
		{[]string{"mx.proton.example; dkim=pass header.d=example.com"}, "checked by mx.proton.example, which isn't trusted yet"},
		{nil, "no Authentication-Results header"},
		{[]string{"mx1.example.org; spf=fail smtp.mailfrom=me@example.com"}, "didn't confirm it came from you"},
	} {
		if _, ok := c.accept(email.Incoming{From: "me@example.com", Subject: "Plans\r\nBcc: x", Body: "hi", AuthResults: tc.headers}); ok {
			t.Fatal("unverified mail got through")
		}
		w := c.Warning()
		if !strings.Contains(w, tc.want) || !strings.Contains(w, `"PlansBcc: x"`) {
			t.Errorf("warning %q should say %q and name the message", w, tc.want)
		}
	}
	if _, ok := c.accept(email.Incoming{From: "me@example.com", Subject: "ok", Body: "hi",
		AuthResults: []string{"mx1.example.org; dkim=pass header.d=example.com"}}); !ok {
		t.Fatal("verified mail should get through")
	}
	if w := c.Warning(); w != "" {
		t.Fatalf("verified mail should clear the warning, got %q", w)
	}
}

func TestWithoutAMailboxTheChannelStops(t *testing.T) {
	err := New(nil, "me@example.com", false, "", 0, nil).Start(context.Background(), func(context.Context, channels.Inbound) {})
	if !channels.IsFatal(err) {
		t.Fatalf("got %v", err)
	}
}
