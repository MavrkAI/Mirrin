package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// gatedChat holds every send until released, counting how many are waiting.
type gatedChat struct {
	release chan struct{}

	mu        sync.Mutex
	now, most int
	sent      []string
}

func (g *gatedChat) Name() string                                  { return "telegram" }
func (g *gatedChat) Start(context.Context, channels.Handler) error { return nil }
func (g *gatedChat) OwnerChatID() string                           { return "42" }
func (g *gatedChat) Send(_ context.Context, chatID, _ string) error {
	g.mu.Lock()
	g.now++
	g.most = max(g.most, g.now)
	g.mu.Unlock()
	<-g.release
	g.mu.Lock()
	g.now--
	g.sent = append(g.sent, chatID)
	g.mu.Unlock()
	return nil
}

func (g *gatedChat) state() (now, most int, sent []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now, g.most, append([]string(nil), g.sent...)
}

// The conversation mailbox that every messaging channel feeds limits other
// people: their turns share a few slots per channel, past a hundred waiting
// chats new strangers are turned away, and the owner is never kept waiting
// by either.
func TestStrangersShareAFewTurnsAndTheOwnerNeverWaits(t *testing.T) {
	d := newChannelsDaemon(t)
	d.paused.Store(true) // every turn replies at once, without a model
	g := &gatedChat{release: make(chan struct{})}
	d.channels = map[string]channels.Channel{"telegram": g}
	ctx := context.Background()

	for i := 0; i < chatsAtOnce; i++ {
		d.handle(ctx, channels.Inbound{Channel: "telegram", ChatID: fmt.Sprint(1000 + i), Text: "hi"})
	}
	waitFor(t, "a few strangers answered at once", func() bool { now, _, _ := g.state(); return now == othersAtOnce })
	d.handle(ctx, channels.Inbound{Channel: "telegram", ChatID: "spam", Text: "hi"})
	d.handle(ctx, channels.Inbound{Channel: "telegram", ChatID: "42", Text: "what's on today?", IsOwner: true})
	waitFor(t, "the owner answered straight away", func() bool { now, _, _ := g.state(); return now == othersAtOnce+1 })
	time.Sleep(50 * time.Millisecond)
	if _, most, _ := g.state(); most != othersAtOnce+1 {
		t.Fatalf("%d turns ran at once, want %d strangers and the owner", most, othersAtOnce)
	}
	close(g.release)
	waitFor(t, "everyone else answered", func() bool { _, _, sent := g.state(); return len(sent) == chatsAtOnce+1 })
	time.Sleep(50 * time.Millisecond)
	_, _, sent := g.state()
	if strings.Contains(strings.Join(sent, ","), "spam") || len(sent) != chatsAtOnce+1 {
		t.Fatalf("sent to %d chats, spam included: %v", len(sent), strings.Contains(strings.Join(sent, ","), "spam"))
	}
}

// holdChat is a transport whose sends to one chat wait until released. It
// records every send as it is tried, before any wait.
type holdChat struct {
	hold    string
	release chan struct{}

	mu    sync.Mutex
	tries []string // chatID|text
}

func (h *holdChat) Name() string                                  { return "telegram" }
func (h *holdChat) Start(context.Context, channels.Handler) error { return nil }
func (h *holdChat) OwnerChatID() string                           { return "42" }
func (h *holdChat) Send(_ context.Context, chatID, text string) error {
	h.mu.Lock()
	h.tries = append(h.tries, chatID+"|"+text)
	h.mu.Unlock()
	if chatID == h.hold {
		<-h.release
	}
	return nil
}

func (h *holdChat) to(chatID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, s := range h.tries {
		if id, text, _ := strings.Cut(s, "|"); id == chatID {
			out = append(out, text)
		}
	}
	return out
}

// quickly fails the test if fn (a transport's call into the daemon) doesn't
// return at once.
func quickly(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s blocked the transport's receive loop", what)
	}
}

// A turn that takes minutes must not stop the transport reading: the next
// message is taken in straight away, another chat is answered meanwhile, and
// the waiting message is answered once the turn is done.
func TestHandleNeverBlocksTheReceiveLoop(t *testing.T) {
	d := newChannelsDaemon(t)
	d.paused.Store(true) // every turn replies at once, without a model
	h := &holdChat{hold: "42", release: make(chan struct{})}
	d.channels = map[string]channels.Channel{"telegram": h}
	ctx := context.Background()
	quickly(t, "a busy chat's messages", func() {
		d.handle(ctx, channels.Inbound{Channel: "telegram", ChatID: "42", Text: "book the table", IsOwner: true})
		waitFor(t, "the first turn to be sending", func() bool { return len(h.to("42")) == 1 })
		d.handle(ctx, channels.Inbound{Channel: "telegram", ChatID: "42", Text: "also add milk", IsOwner: true})
		d.handle(ctx, channels.Inbound{Channel: "telegram", ChatID: "7", Text: "hello"})
	})
	waitFor(t, "the other chat answered while the first is busy", func() bool { return len(h.to("7")) == 1 })
	if n := len(h.to("42")); n != 1 {
		t.Fatalf("the waiting message was answered before the running turn ended: %d sends", n)
	}
	close(h.release)
	waitFor(t, "the waiting message answered after the turn", func() bool { return len(h.to("42")) == 2 })
}

// Past the inbox limit a message is dropped without the transport's receive
// loop ever sending (Signal can only finish a send by reading on that loop).
// The owner hears so once per backlog; a stranger's flood is not echoed back.
func TestInboxFullNeverSendsFromTheReceiveLoop(t *testing.T) {
	d := newChannelsDaemon(t)
	d.paused.Store(true)
	h := &holdChat{hold: "42", release: make(chan struct{})}
	d.channels = map[string]channels.Channel{"telegram": h}
	ctx := context.Background()
	owner := channels.Inbound{Channel: "telegram", ChatID: "42", Text: "hi", IsOwner: true}
	d.handle(ctx, owner)
	waitFor(t, "the first turn to be sending", func() bool { return len(h.to("42")) == 1 })
	quickly(t, "an overflowing inbox", func() {
		for i := 0; i < inboxLimit+10; i++ {
			d.handle(ctx, owner)
		}
	})
	stillWorking := func() int {
		n := 0
		for _, s := range h.to("42") {
			if strings.Contains(s, "still working") {
				n++
			}
		}
		return n
	}
	waitFor(t, "the owner told their message was dropped", func() bool { return stillWorking() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := stillWorking(); n != 1 {
		t.Fatalf("the owner was told %d times for one backlog", n)
	}

	// A stranger flooding a shared chat fills only their share of it: the
	// owner's next message still gets in, and nothing is sent to the chat
	// for what was dropped.
	h2 := &holdChat{hold: "-100", release: make(chan struct{})}
	d.channels["telegram"] = h2
	bob := channels.Inbound{Channel: "telegram", ChatID: "-100", Sender: "bob", Text: "spam"}
	d.handle(ctx, bob)
	waitFor(t, "bob's first turn to be sending", func() bool { return len(h2.to("-100")) == 1 })
	quickly(t, "a stranger's flood", func() {
		for i := 0; i < inboxLimit+10; i++ {
			d.handle(ctx, bob)
		}
		d.handle(ctx, channels.Inbound{Channel: "telegram", ChatID: "-100", Sender: "tony", Text: "hello", IsOwner: true})
	})
	c := d.conv("telegram:-100")
	c.mu.Lock()
	owners := waiting(c.inbox, true)
	c.mu.Unlock()
	if owners != 1 {
		t.Fatalf("the owner's message in the shared chat was dropped behind a stranger's flood")
	}
	time.Sleep(50 * time.Millisecond)
	if got := h2.to("-100"); len(got) != 1 {
		t.Fatalf("a stranger's flood was answered in the chat: %q", got)
	}
	close(h2.release)
	close(h.release)
	waitFor(t, "the shared chat's backlog answered", func() bool { return len(h2.to("-100")) >= 3 })
}
