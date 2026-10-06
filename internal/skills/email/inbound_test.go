package email

import (
	"slices"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

func TestAuthResultsKeepsOrderAndUnfolds(t *testing.T) {
	raw := "Authentication-Results: mx.google.com;\r\n" +
		"       dkim=pass header.i=@example.com;\r\n" +
		"       spf=pass smtp.mailfrom=me@example.com\r\n" +
		"Authentication-Results: forged.example; dmarc=pass header.from=example.com\r\n\r\n"
	got := authResults([]byte(raw))
	if len(got) != 2 || got[0] != "mx.google.com; dkim=pass header.i=@example.com; spf=pass smtp.mailfrom=me@example.com" || got[1] != "forged.example; dmarc=pass header.from=example.com" {
		t.Fatalf("got %q", got)
	}
	if authResults(nil) != nil || authResults([]byte("\r\n")) != nil {
		t.Fatal("no headers should give nothing")
	}
}

func TestAuthServersGuessFromIMAPHost(t *testing.T) {
	icloud := []string{"bimi.icloud.com", "dmarc.icloud.com", "dkim-verifier.icloud.com", "spf.icloud.com"}
	for host, want := range map[string][]string{
		"imap.gmail.com":        {"mx.google.com"},
		"imap.fastmail.com":     {"messagingengine.com"},
		"imap.mail.me.com":      icloud,
		"outlook.office365.com": {""},
		"imap.example.org":      {"example.org"},
		"example.org":           {"example.org"},
		// A local bridge (Proton) or an address says nothing about the provider.
		"127.0.0.1":        nil,
		"[::1]":            nil,
		"localhost":        nil,
		"10.0.0.7":         nil,
		"":                 nil,
		"bridge.localhost": nil,
	} {
		c := New(config.Email{IMAPHost: host}, "", time.UTC)
		got, guessed := c.AuthServers()
		if !slices.Equal(got, want) || !guessed {
			t.Errorf("%q: got %q (guessed %v), want %q", host, got, guessed, want)
		}
	}
	c := New(config.Email{IMAPHost: "imap.gmail.com", AuthServers: []string{"mx.mine.net"}}, "", time.UTC)
	if got, guessed := c.AuthServers(); !slices.Equal(got, []string{"mx.mine.net"}) || guessed {
		t.Errorf("configured servers should win: %q (guessed %v)", got, guessed)
	}
}
