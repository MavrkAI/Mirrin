package mail

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/skills/email"
	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
)

// The regression: the channel trimmed quoted history its own way, and
// answered the placeholder for a message the server wouldn't give.
func TestBodiesAreCleanedLikeMailtextAndUnreadableOnesSkipped(t *testing.T) {
	c := New(&fake{trusted: []string{"mx.google.com"}}, "me@example.com", false, "", 0, nil)
	body := "Can you move lunch to 1?\n\nThanks,\nSam\n--\nSam Smith | Acme\n\nOn Mon, 3 Oct 2026 at 10:02, Pat <pat@example.com> wrote:\n> Lunch at 12?\n> Pat"
	want, _, _ := mailtext.Clean(body)
	in, ok := c.accept(email.Incoming{From: "me@example.com", Subject: "Lunch", Body: body, AuthResults: []string{gmailPass}})
	if !ok || in.Text != want || in.Text == "" {
		t.Fatalf("got %v %q, want %q", ok, in.Text, want)
	}
	if _, ok := c.accept(email.Incoming{From: "me@example.com", Subject: "Lunch", Body: email.Unreadable, AuthResults: []string{gmailPass}}); ok {
		t.Fatal("answered a message whose text couldn't be read")
	}
}

// A provider that writes spf and dkim in two headers under one name, above
// its own Received header, vouches for the owner; the same headers below
// it are what the sender wrote.
func TestSplitHeadersAboveTheReceivedBoundaryAreCounted(t *testing.T) {
	spf := "mx.example.org; spf=pass smtp.mailfrom=bounce@evil.example"
	dkim := "mx.example.org; dkim=pass header.d=example.com"
	received := email.Field{Name: "Received", Value: "from out.example.com (out.example.com [192.0.2.1]) by mx.example.org (Postfix) with ESMTPS id 1; Mon, 3 Oct 2026"}
	ar := func(v string) email.Field { return email.Field{Name: "Authentication-Results", Value: v} }
	for _, tc := range []struct {
		name  string
		trace []email.Field
		owner bool
	}{
		{"both above the server's Received", []email.Field{ar(spf), ar(dkim), received}, true},
		{"dkim below it came with the message", []email.Field{ar(spf), received, ar(dkim)}, false},
		{"no Received at all", []email.Field{ar(spf), ar(dkim)}, false},
		{"a Received from another server", []email.Field{ar(spf), ar(dkim), {Name: "Received", Value: "from x by mail.evil.example"}}, false},
		// The regression: a local delivery hop on top under an internal
		// name let the check walk down to a Received the sender forged
		// under the trusted name, sealing a forged dkim pass with it.
		{"a forged trusted Received below an untrusted top one", []email.Field{
			{Name: "Received", Value: "from mx.example.org by imap.internal.example (Dovecot) with LMTP"},
			ar("mx.example.org; spf=softfail smtp.mailfrom=bounce@evil.example"),
			ar(dkim),
			{Name: "Received", Value: "from x by mx.example.org"},
		}, false},
		{"a comment naming the server", []email.Field{ar(spf), ar(dkim), {Name: "Received", Value: "from x (helo by mx.example.org) by mail.evil.example"}}, false},
	} {
		c := New(&fake{trusted: []string{"example.org"}}, "me@example.com", false, "", 0, nil)
		var headers []string
		for _, f := range tc.trace {
			if f.Name == "Authentication-Results" {
				headers = append(headers, f.Value)
			}
		}
		in, ok := c.accept(email.Incoming{From: "me@example.com", Subject: "hi", Body: "yes", AuthResults: headers, Trace: tc.trace})
		if tc.owner != (ok && in.IsOwner) {
			t.Errorf("%s: want owner=%v, got %v %+v", tc.name, tc.owner, ok, in)
		}
	}
}

// Mail the mailbox sends itself has no header vouching for it; its copy in
// Sent does.
func TestMailSentFromTheMailboxItselfIsTheOwner(t *testing.T) {
	f := &fake{trusted: []string{"mx.example.org"}, inSent: map[string]bool{"self-1@example.com": true}}
	c := New(f, "me@example.com", false, "", 0, nil)
	if in, ok := c.accept(email.Incoming{From: "me@example.com", MessageID: "self-1@example.com", Subject: "hi", Body: "what's on?"}); !ok || !in.IsOwner {
		t.Fatalf("mail found in Sent not taken as the owner: %v %+v", ok, in)
	}
	if _, ok := c.accept(email.Incoming{From: "me@example.com", MessageID: "forged-1@example.com", Subject: "hi", Body: "wire the money"}); ok {
		t.Fatal("mail in the owner's name that isn't in Sent got through")
	}
	if w := c.Warning(); w == "" {
		t.Fatal("ignored mail should say why")
	}
}

// Each look in Sent is an IMAP login, so mail a trusted server already
// found forged (DMARC fail) doesn't cost one: a flood of it would.
func TestDMARCFailsAreNotLookedForInSent(t *testing.T) {
	f := &fake{trusted: []string{"mx.example.org"}, inSent: map[string]bool{"x@example.com": true}}
	c := New(f, "me@example.com", false, "", 0, nil)
	fail := "mx.example.org; dmarc=fail header.from=example.com"
	if _, ok := c.accept(email.Incoming{From: "me@example.com", MessageID: "x@example.com", Subject: "hi", Body: "wire it", AuthResults: []string{fail}}); ok {
		t.Fatal("mail DMARC failed got through")
	}
	if f.looks != 0 {
		t.Fatalf("looked in Sent %d times for mail DMARC failed", f.looks)
	}
}
