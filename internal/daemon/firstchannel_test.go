package daemon

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// newFirstChannelDaemon is newChannelsDaemon before the twin has introduced
// itself anywhere, checking its channels often.
func newFirstChannelDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := newChannelsDaemon(t)
	if err := d.store.Set(context.Background(), firstHelloKey, ""); err != nil {
		t.Fatal(err)
	}
	oldEvery, oldRetry := firstHelloEvery, firstHelloRetry
	firstHelloEvery, firstHelloRetry = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { firstHelloEvery, firstHelloRetry = oldEvery, oldRetry })
	return d
}

// learningTransport learns the owner's chat from their first message, as
// Telegram does for an owner given as @username.
type learningTransport struct {
	*fakeTransport
	ownerChat atomic.Value
}

func (l *learningTransport) OwnerChatID() string {
	s, _ := l.ownerChat.Load().(string)
	return s
}

// quietFor checks, for a while, that nothing more was sent on any of chans.
func quietFor(t *testing.T, what string, chans ...*fakeTransport) {
	t.Helper()
	before := make([]int, len(chans))
	for i, ch := range chans {
		before[i] = len(ch.Sent())
	}
	time.Sleep(150 * time.Millisecond) // thirty checks of a channel
	for i, ch := range chans {
		if got := ch.Sent(); len(got) != before[i] {
			t.Fatalf("%s: %s sent %q", what, ch.name, got[before[i]:])
		}
	}
}

// The first channel up with the owner's chat hears who the twin is, in the
// owner's chat only, and no channel ever hears it again: not a second one,
// and not the first one reconnected.
func TestFirstChannelSaysHelloOnce(t *testing.T) {
	d := newFirstChannelDaemon(t)
	want := "42: This is " + d.Config().Name + ", on your phone now. Message me here any time; only you can give me instructions here."
	tg := &fakeTransport{name: "telegram", owner: "42"}
	d.launch("telegram", tg)
	waitFor(t, "the first hello", func() bool { return len(tg.Sent()) == 1 })
	if got := tg.Sent(); got[0] != want {
		t.Fatalf("sent %q, want %q", got[0], want)
	}

	slack := &fakeTransport{name: "slack", owner: "U1"}
	d.launch("slack", slack)
	d.StopChannel("telegram")
	again := &fakeTransport{name: "telegram", owner: "42"}
	d.launch("telegram", again)
	waitFor(t, "both up", func() bool {
		s, _ := d.channelStatus("slack")
		g, _ := d.channelStatus("telegram")
		return s.State == channels.Connected && g.State == channels.Connected
	})
	quietFor(t, "after the first hello", tg, slack, again)

	if said, _ := d.store.Get(context.Background(), firstHelloKey); !strings.HasPrefix(said, "telegram ") {
		t.Fatalf("recorded %q", said)
	}
	audit, _ := d.store.RecentAudit(context.Background(), 5)
	if len(audit) == 0 || audit[0].Kind != "message.out" || audit[0].ChatKey != "telegram:42" {
		t.Fatalf("audit %+v", audit)
	}
}

// Mail and the desktop chats aren't the owner's phone: connected first,
// they hear no "on your phone now", nor count as introduced, at start or
// later. The first phone app connected after them says hello.
func TestFirstChannelOnlyOnAPhone(t *testing.T) {
	ctx := context.Background()
	d := newFirstChannelDaemon(t)
	slack := &fakeTransport{name: "slack", owner: "U1"}
	mail := &fakeTransport{name: "mail", owner: "akshay@example.com"}
	d.introducedBefore(ctx, []channels.Channel{slack, mail})
	if said, _ := d.store.Get(ctx, firstHelloKey); said != "" {
		t.Fatalf("Slack and mail counted as an upgrade: %q", said)
	}
	d.launch("slack", slack)
	d.launch("mail", mail)
	waitFor(t, "slack and mail up", func() bool {
		s, _ := d.channelStatus("slack")
		m, _ := d.channelStatus("mail")
		return s.State == channels.Connected && m.State == channels.Connected
	})
	quietFor(t, "on Slack and mail", slack, mail)
	if s, m := slack.Sent(), mail.Sent(); len(s)+len(m) != 0 {
		t.Fatalf("said on Slack %q, by mail %q", s, m)
	}
	if said, _ := d.store.Get(ctx, firstHelloKey); said != "" {
		t.Fatalf("recorded %q", said)
	}

	tg := &fakeTransport{name: "telegram", owner: "42"}
	d.launch("telegram", tg)
	waitFor(t, "the first hello on the phone", func() bool { return len(tg.Sent()) == 1 })
	if got := tg.Sent()[0]; !strings.Contains(got, "on your phone now") {
		t.Fatalf("sent %q", got)
	}
	if s, m := slack.Sent(), mail.Sent(); len(s)+len(m) != 0 {
		t.Fatalf("said on Slack %q, by mail %q", s, m)
	}
}

// With the owner's chat not yet known (Telegram given an @username), the
// twin waits for it, says nothing to anyone else meanwhile, and then says
// hello in the owner's chat.
func TestFirstChannelWaitsForTheOwnersChat(t *testing.T) {
	d := newFirstChannelDaemon(t)
	tg := &learningTransport{fakeTransport: &fakeTransport{name: "telegram"}}
	d.launch("telegram", tg)
	waitFor(t, "telegram up", func() bool { st, _ := d.channelStatus("telegram"); return st.State == channels.Connected })
	quietFor(t, "before the owner's chat is known", tg.fakeTransport)

	tg.ownerChat.Store("7001")
	waitFor(t, "the first hello", func() bool { return len(tg.Sent()) == 1 })
	if got := tg.Sent()[0]; !strings.HasPrefix(got, "7001: This is ") {
		t.Fatalf("sent %q", got)
	}
}

// A twin that starts with a messaging app already set up is an upgrade:
// it has been talking to the owner there all along, so it says nothing,
// then or later. A twin starting with only the terminal is not.
func TestFirstChannelNeverOnUpgrade(t *testing.T) {
	ctx := context.Background()
	d := newFirstChannelDaemon(t)
	d.introducedBefore(ctx, []channels.Channel{&fakeChannel{name: "cli"}, &fakeChannel{name: "voice"}})
	if said, _ := d.store.Get(ctx, firstHelloKey); said != "" {
		t.Fatalf("the terminal alone counted as an upgrade: %q", said)
	}

	tg := &fakeTransport{name: "telegram", owner: "42"}
	d.introducedBefore(ctx, []channels.Channel{&fakeChannel{name: "cli"}, tg})
	if said, _ := d.store.Get(ctx, firstHelloKey); said != "already" {
		t.Fatalf("recorded %q", said)
	}
	d.launch("telegram", tg)
	wa := &fakeTransport{name: "whatsapp", owner: "61400000000@s.whatsapp.net"}
	d.launch("whatsapp", wa)
	waitFor(t, "telegram up", func() bool { st, _ := d.channelStatus("telegram"); return st.State == channels.Connected })
	quietFor(t, "an upgrade", tg, wa)
	if d.introduce(ctx, "whatsapp") {
		t.Fatal("an upgrade counted as just introduced, so WhatsApp's pairing hello would be skipped")
	}
}

// Nothing is said while the twin is paused; resumed, it says hello.
func TestFirstChannelWaitsWhilePaused(t *testing.T) {
	d := newFirstChannelDaemon(t)
	d.paused.Store(true)
	tg := &fakeTransport{name: "telegram", owner: "42"}
	d.launch("telegram", tg)
	waitFor(t, "telegram up", func() bool { st, _ := d.channelStatus("telegram"); return st.State == channels.Connected })
	quietFor(t, "while paused", tg)
	d.paused.Store(false)
	waitFor(t, "the first hello", func() bool { return len(tg.Sent()) == 1 })
}

// A hello Telegram refuses (a bot the owner hasn't started yet) isn't
// counted as said: it is tried again later, and goes out once.
func TestFirstChannelTriesAgainWhenRefused(t *testing.T) {
	d := newFirstChannelDaemon(t)
	var refused atomic.Int32
	tg := &fakeTransport{name: "telegram", owner: "42"}
	tg.send = func(chatID, text string) error {
		if refused.Add(1) <= 2 {
			return errors.New("Forbidden: bot can't initiate conversation with a user")
		}
		tg.mu.Lock()
		tg.sent = append(tg.sent, chatID+": "+text)
		tg.mu.Unlock()
		return nil
	}
	d.launch("telegram", tg)
	waitFor(t, "the hello at the third try", func() bool { return len(tg.Sent()) == 1 })
	quietFor(t, "once it went", tg)
	if n := refused.Load(); n != 3 {
		t.Fatalf("%d tries", n)
	}
}

// After pairing WhatsApp the twin's first words there are its only hello:
// introduce reports that it has just said it there, and not elsewhere.
func TestFirstChannelIsWhatsAppsHelloAfterPairing(t *testing.T) {
	ctx := context.Background()
	d := newFirstChannelDaemon(t)
	wa := &fakeTransport{name: "whatsapp", owner: "61400000000@s.whatsapp.net"}
	d.launch("whatsapp", wa)
	waitFor(t, "the first hello", func() bool { return len(wa.Sent()) == 1 })
	if !d.introduce(ctx, "whatsapp") {
		t.Fatal("not counted as just said on WhatsApp")
	}
	if d.introduce(ctx, "telegram") {
		t.Fatal("counted as just said on Telegram")
	}
	if n := len(wa.Sent()); n != 1 {
		t.Fatalf("said %d times", n)
	}
}
