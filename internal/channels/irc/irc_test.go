package irc

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// wire gives c a pretend connection and returns what it writes.
func wire(c *Channel) *bytes.Buffer {
	var out bytes.Buffer
	c.w = bufio.NewWriter(&out)
	return &out
}

func TestParseAndRoute(t *testing.T) {
	p, cmd, params := parse("@account=akshay :nick!user@host PRIVMSG #room :mavrk: what's up")
	if p != "nick!user@host" || cmd != "PRIVMSG" || len(params) != 2 || params[1] != "mavrk: what's up" {
		t.Fatalf("parse: %q %q %q", p, cmd, params)
	}
	if tags := parseTags(`@account=akshay;msg=a\sb\:c :x PRIVMSG y :z`); tags["account"] != "akshay" || tags["msg"] != "a b;c" {
		t.Fatalf("tags: %v", tags)
	}
	c := New("irc.example:6697", true, "mavrk", "", "akshay", false, []string{"#room"}, nil)
	wire(c)
	c.accountTags = true
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	_ = c.onLine(context.Background(), "@account=akshay :akshay!a@h PRIVMSG mavrk :hello", h)
	_ = c.onLine(context.Background(), "@account=akshay :akshay!a@h PRIVMSG #room :Mavrk, status?", h)
	_ = c.onLine(context.Background(), "@account=akshay :akshay!a@h PRIVMSG #room :unrelated chatter", h)
	_ = c.onLine(context.Background(), "@account=bob :bob!b@h PRIVMSG mavrk :hi", h)
	if len(got) != 2 || got[0].ChatID != "akshay" || got[1].ChatID != "#room|akshay" || got[1].Text != "status?" || !got[0].IsOwner {
		t.Fatalf("got %+v", got)
	}
}

func TestOwnerNeedsTheirAccountNotJustTheNick(t *testing.T) {
	for _, tc := range []struct {
		name     string
		owner    string
		accounts bool // server negotiated account-tag
		line     string
		want     string // "owner", "stranger" or "" (dropped)
	}{
		{"account matches", "akshay", true, "@account=akshay :akshay!a@h PRIVMSG mavrk :hi", "owner"},
		{"account matches under another nick", "akshay", true, "@account=Akshay :akshay_away!a@h PRIVMSG mavrk :hi", "owner"},
		{"owner's nick, no account", "akshay", true, ":akshay!m@evil.example PRIVMSG mavrk :yes 3", ""},
		{"owner's nick, someone else's account", "akshay", true, "@account=mallory :akshay!m@evil.example PRIVMSG mavrk :yes 3", ""},
		{"tag on a server that never agreed to send it", "akshay", false, "@account=akshay :akshay!m@evil.example PRIVMSG mavrk :hi", ""},
		{"someone else", "akshay", true, "@account=bob :bob!b@h PRIVMSG mavrk :hi", "stranger"},
		{"full mask", "akshay!~a@user/akshay", false, ":akshay!~a@user/akshay PRIVMSG mavrk :hi", "owner"},
		// The mask's nick from another host is an impostor, not a stranger:
		// answering would put them in the owner's conversation.
		{"mask is not a prefix match", "akshay!~a@user/akshay", false, ":akshay!~a@user/akshay.evil.example PRIVMSG mavrk :hi", ""},
		{"someone else under a mask", "akshay!~a@user/akshay", false, ":bob!b@h PRIVMSG mavrk :hi", "stranger"},
		{"wildcard mask", "*!*@user/akshay", false, ":anynick!x@user/akshay PRIVMSG mavrk :hi", "owner"},
		{"user@host mask", "~a@home.example", false, ":akshay!~a@home.example PRIVMSG mavrk :hi", "owner"},
		// Masks that used to be prefixes still work as they did.
		{"old prefix mask", "akshay!~a@user/", false, ":akshay!~a@user/akshay PRIVMSG mavrk :hi", "owner"},
		{"old prefix mask, other user", "akshay!~a@", false, ":akshay!~m@evil.example PRIVMSG mavrk :hi", ""},
	} {
		c := New("irc.example:6697", true, "mavrk", "", tc.owner, true, nil, nil)
		wire(c)
		c.accountTags = tc.accounts
		var got []channels.Inbound
		_ = c.onLine(context.Background(), tc.line, func(_ context.Context, in channels.Inbound) { got = append(got, in) })
		switch {
		case tc.want == "" && len(got) != 0:
			t.Errorf("%s: should be ignored, got %+v", tc.name, got)
		case tc.want == "owner" && (len(got) != 1 || !got[0].IsOwner):
			t.Errorf("%s: want the owner, got %+v", tc.name, got)
		case tc.want == "stranger" && (len(got) != 1 || got[0].IsOwner):
			t.Errorf("%s: want a stranger, got %+v", tc.name, got)
		}
	}
}

func TestProactiveMessagesGoOnlyToTheVerifiedOwner(t *testing.T) {
	c := New("irc.example:6697", true, "mavrk", "", "akshay", false, nil, nil)
	wire(c)
	c.accountTags = true
	h := func(context.Context, channels.Inbound) {}
	if c.OwnerChatID() != "" {
		t.Fatal("owner chat should be unknown until the owner speaks")
	}
	_ = c.onLine(context.Background(), "@account=akshay :akshay!a@h PRIVMSG mavrk :hi", h)
	if c.OwnerChatID() != "akshay" {
		t.Fatalf("owner chat %q", c.OwnerChatID())
	}
	_ = c.onLine(context.Background(), ":akshay!a@h QUIT :bye", h)
	if c.OwnerChatID() != "" {
		t.Fatal("owner chat should be forgotten when that nick quits")
	}
	_ = c.onLine(context.Background(), "@account=akshay :akshay!a@h PRIVMSG mavrk :back", h)
	_ = c.onLine(context.Background(), ":akshay!m@evil PRIVMSG mavrk :it's me", h)
	if c.OwnerChatID() != "" {
		t.Fatal("owner chat should be forgotten when someone else holds the nick")
	}
	// A nick with "|" is a person, not "#room|nick".
	out := wire(c)
	if err := c.Send(context.Background(), "bob|away", "hi"); err != nil || out.String() != "PRIVMSG bob|away :hi\r\n" {
		t.Fatalf("sent %q (%v)", out.String(), err)
	}
}

// The owner's conversation is keyed by who they are, not the nick they
// use, so whoever takes one of their nicks later starts a conversation of
// their own and never sees the owner's history or approvals.
func TestStrangersNeverLandInTheOwnersConversation(t *testing.T) {
	c := New("irc.example:6697", true, "mavrk", "", "akshay", true, nil, nil)
	out := wire(c)
	c.accountTags = true
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	ctx := context.Background()
	for _, l := range []string{
		"@account=akshay :akshay_away!a@h PRIVMSG mavrk :hi",
		"@account=akshay :akshay_away!a@h PRIVMSG #room :mavrk: status?",
	} {
		_ = c.onLine(ctx, l, h)
	}
	if len(got) != 2 || got[0].ChatID != "akshay" || got[1].ChatID != "#room|akshay" || !got[0].IsOwner || !got[1].IsOwner {
		t.Fatalf("the owner's messages should key on their account: %+v", got)
	}
	// Replies reach the nick the owner is using.
	_ = c.Send(ctx, "akshay", "one")
	_ = c.Send(ctx, "#room|akshay", "two")
	if want := "PRIVMSG akshay_away :one\r\nPRIVMSG #room :akshay_away: two\r\n"; out.String() != want {
		t.Fatalf("sent %q, want %q", out.String(), want)
	}

	// The owner leaves; a stranger takes the nick they were using.
	got = nil
	out.Reset()
	for _, l := range []string{
		":akshay_away!a@h QUIT :bye",
		":akshay_away!m@evil.example PRIVMSG mavrk :what did he ask you?",
		":akshay_away!m@evil.example PRIVMSG #room :mavrk: and here?",
		":akshay!m@evil.example PRIVMSG mavrk :yes 3",
	} {
		_ = c.onLine(ctx, l, h)
	}
	if len(got) != 2 || got[0].IsOwner || got[0].ChatID != "akshay_away" || got[1].ChatID != "#room|akshay_away" {
		t.Fatalf("a stranger on an old nick of the owner's must get a conversation of their own, got %+v", got)
	}
	if err := c.Send(ctx, "akshay", "for your eyes only"); err == nil || out.Len() != 0 {
		t.Fatalf("nothing may go out to the owner's conversation while they're away: err %v, sent %q", err, out.String())
	}

	// A mask with a wildcard nick keys on the mask, which no nick can equal.
	c = New("irc.example:6697", true, "mavrk", "", "*!*@user/akshay", true, nil, nil)
	wire(c)
	got = nil
	_ = c.onLine(ctx, ":ak!x@user/akshay PRIVMSG mavrk :hi", h)
	_ = c.onLine(ctx, ":ak!x@evil.example PRIVMSG mavrk :hi", h)
	if len(got) != 2 || got[0].ChatID != "*!*@user/akshay" || got[1].ChatID != "ak" || got[1].IsOwner {
		t.Fatalf("got %+v", got)
	}
	if c.OwnerChatID() != "" {
		t.Fatal("a stranger on the owner's last nick must make the owner's chat unknown again")
	}
}

func TestOwnerMessagesAreNeverCrowdedOut(t *testing.T) {
	b := newInbox(2)
	for i, want := range []bool{true, true, false} {
		if ok := b.put(channels.Inbound{Sender: "bob", Text: string(rune('a' + i))}); ok != want {
			t.Fatalf("stranger message %d: queued %v, want %v", i, ok, want)
		}
	}
	if !b.put(channels.Inbound{Sender: "akshay", IsOwner: true}) {
		t.Fatal("a stranger's flood pushed out the owner's message")
	}
	done := make(chan struct{})
	var order []string
	for range 3 {
		in, _ := b.next(done)
		order = append(order, in.Sender)
	}
	if strings.Join(order, ",") != "akshay,bob,bob" {
		t.Fatalf("the owner's message should be taken first: %v", order)
	}
	close(done)
	if _, ok := b.next(done); ok {
		t.Fatal("next should stop once done")
	}
}

func TestOwnerIsToldWhatToFix(t *testing.T) {
	c := New("irc.example:6697", true, "mavrk", "", "akshay", false, nil, nil)
	wire(c)
	_ = c.onLine(context.Background(), ":srv CAP * LS :multi-prefix", nil)
	_ = c.onLine(context.Background(), ":srv 001 mavrk :welcome", nil)
	if w := c.Warning(); !strings.Contains(w, "nick!user@host") {
		t.Fatalf("a server without accounts should be flagged: %q", w)
	}
	for owner, weak := range map[string]bool{
		"akshay!~a@":             true,  // any host
		"akshay!":                true,  // any user and host
		"akshay!~a@user/":        false, // a cloak prefix
		"*!*@user/akshay":        false,
		"akshay!~a@home.example": false,
	} {
		if w := New("irc.example:6697", true, "mavrk", "", owner, false, nil, nil).Warning(); (w != "") != weak {
			t.Errorf("%s: warning %q, want one: %v", owner, w, weak)
		}
	}
}

func TestCapabilityNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		lines          []string
		want           []string
		accounts       bool
	}{
		{"account-tag offered", "", []string{":srv CAP * LS * :multi-prefix sasl", ":srv CAP * LS :account-tag away-notify", ":srv CAP * ACK :account-tag"},
			[]string{"CAP REQ :account-tag", "CAP END"}, true},
		{"with sasl", "pw", []string{":srv CAP * LS :sasl=PLAIN,EXTERNAL account-tag", ":srv CAP * ACK :sasl account-tag", "AUTHENTICATE +", ":srv 903 mavrk :ok"},
			[]string{"CAP REQ :sasl account-tag", "AUTHENTICATE PLAIN", "AUTHENTICATE bWF2cmsAbWF2cmsAcHc=", "CAP END"}, true},
		{"nothing useful", "", []string{":srv CAP * LS :multi-prefix"}, []string{"CAP END"}, false},
		{"refused", "", []string{":srv CAP * LS :account-tag", ":srv CAP * NAK :account-tag"}, []string{"CAP REQ :account-tag", "CAP END"}, false},
	} {
		c := New("irc.example:6697", true, "mavrk", tc.password, "akshay", false, nil, nil)
		out := wire(c)
		for _, l := range tc.lines {
			if err := c.onLine(context.Background(), l, nil); err != nil {
				t.Fatal(err)
			}
		}
		got := strings.Split(strings.TrimSpace(strings.ReplaceAll(out.String(), "\r\n", "\n")), "\n")
		if strings.Join(got, "|") != strings.Join(tc.want, "|") || c.accountTags != tc.accounts {
			t.Errorf("%s: sent %q (accounts %v), want %q (accounts %v)", tc.name, got, c.accountTags, tc.want, tc.accounts)
		}
	}
}

func TestSendKeepsCharactersWholeAndCommandsOut(t *testing.T) {
	c := New("irc.example:6697", true, "mavrk", "", "akshay", false, nil, nil)
	out := wire(c)
	long := strings.Repeat("😀", 150) // 600 bytes, no spaces
	if err := c.Send(context.Background(), "bob", long+"\nok\rQUIT :bye"); err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\r\n") {
		if !strings.HasPrefix(l, "PRIVMSG bob :") {
			t.Fatalf("every line must be a PRIVMSG, got %q", l)
		}
		text := strings.TrimPrefix(l, "PRIVMSG bob :")
		if !utf8.ValidString(text) || len(text) > 400 {
			t.Fatalf("bad chunk (%d bytes, valid %v)", len(text), utf8.ValidString(text))
		}
		joined.WriteString(text)
	}
	if joined.String() != long+"okQUIT :bye" {
		t.Fatalf("text changed in transit: %q", joined.String())
	}
}

// A long turn must not stop the connection answering PING.
func TestPingAnsweredDuringALongTurn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	pong := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		send := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			switch {
			case line == "CAP LS 302":
				send(":srv CAP * LS :account-tag")
			case line == "CAP REQ :account-tag":
				send(":srv CAP * ACK :account-tag")
			case line == "CAP END":
				send(":srv 001 mavrk :welcome")
				send("@account=akshay :akshay!a@h PRIVMSG mavrk :do something slow")
				send("PING :still-there")
			case strings.HasPrefix(line, "PONG"):
				pong <- line
				return
			}
		}
	}()
	c := New(ln.Addr().String(), false, "mavrk", "", "akshay", false, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	busy := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = c.Start(ctx, func(context.Context, channels.Inbound) {
			close(busy)
			<-release // the turn is still going
		})
	}()
	defer close(release)
	select {
	case <-busy:
	case <-time.After(5 * time.Second):
		t.Fatal("message never reached the twin")
	}
	select {
	case p := <-pong:
		if p != "PONG :still-there" {
			t.Fatalf("got %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PING went unanswered while the twin was busy")
	}
}
