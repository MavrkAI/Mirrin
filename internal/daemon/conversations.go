package daemon

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// conversation is the daemon's state for one chat: one turn at a time, a
// mailbox for messages that arrive meanwhile, proactive messages held back
// until the turn ends, which approvals the twin has just asked about, and
// what a "stop" would cut short.
type conversation struct {
	turn sync.Mutex // held for a whole turn

	mu         sync.Mutex
	busy       bool          // a turn is running
	held       []llm.Message // proactive messages to record once it ends
	inbox      []queued      // channel messages waiting for their turn
	draining   bool          // a worker is answering the inbox
	overflowed bool          // the inbox filled and the owner was told; cleared as it drains

	// raised holds approvals raised since the twin last spoke, by the
	// conversation that raised them: this one, or one of its background runs.
	raised  map[string][]int64
	asked   []int64 // the approvals the twin's latest message to the owner asked about
	askedAt time.Time
	vouched bool    // that message was the daemon's own, showing each request as stored
	own     []int64 // what the reply the daemon is writing asks about
	owned   bool
	theirs  []int64 // raised for someone else in a shared chat: never decided by a bare yes

	// A "which one?" or "just to be sure" follows the owner's yes (1) or no
	// (-1): a reply that only picks one carries it (resolve_approval).
	askedLean, ownLean int8
	// prior is what the twin had asked when the owner's latest message
	// wasn't a plain yes or no (moveOn): for the turn answering it, the
	// question that message may answer in its own words.
	prior     []int64
	priorAt   time.Time
	priorLean int8
	offer     *alwaysOffer // "stop asking about this?", waiting for the owner's yes (alwaysallow.go)
	runs      []*running   // turns and approved actions under way here, which a stop cuts short (interrupt.go)

	refs int // runs holding it through acquire; guarded by locksMu
}

type queued struct {
	ctx context.Context
	in  channels.Inbound
	at  time.Time // when it arrived
	pre *preheard // an owner's voice note heard while their turn runs (voicestop.go)
}

type arrivedKey struct{}

// withArrival notes when the message being handled reached the daemon.
func withArrival(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, arrivedKey{}, at)
}

// arrivedAt is when the message being handled reached the daemon: an answer
// can only be to a question asked before it.
func arrivedAt(ctx context.Context) time.Time {
	if at, ok := ctx.Value(arrivedKey{}).(time.Time); ok {
		return at
	}
	return clock()
}

// inboxLimit caps a conversation's backlog, the owner's messages and other
// people's each (in a shared chat a crowd can't crowd the owner out); past
// it a message is dropped, and the owner is asked to resend.
const inboxLimit = 64

// waiting counts the owner's messages in an inbox, or everyone else's.
func waiting(inbox []queued, owner bool) int {
	n := 0
	for _, q := range inbox {
		if q.in.IsOwner == owner {
			n++
		}
	}
	return n
}

// Other people's turns share a few slots per channel, and only so many chats
// on a channel can have messages waiting before strangers are turned away:
// each turn can use tools for minutes, so a crowd (or one spammer with many
// accounts) mustn't set off dozens at once. The owner never waits for a slot
// and is never turned away.
const (
	othersAtOnce = 3
	chatsAtOnce  = 100
)

// mailboxes counts, per channel, the chats with messages waiting, and holds
// the slots other people's turns share.
type mailboxes struct {
	mu      sync.Mutex
	waiting map[string]int
	slots   map[string]chan struct{}
}

// open counts one more chat waiting on channel, unless it is someone other
// than the owner and too many are waiting already.
func (m *mailboxes) open(channel string, owner bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !owner && m.waiting[channel] >= chatsAtOnce {
		return false
	}
	if m.waiting == nil {
		m.waiting = map[string]int{}
	}
	m.waiting[channel]++
	return true
}

// close counts a chat on channel as no longer waiting.
func (m *mailboxes) close(channel string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waiting[channel]--; m.waiting[channel] <= 0 {
		delete(m.waiting, channel)
	}
}

// turn waits for a slot for a turn on channel (the owner's start at once) and
// returns its release, or nil if ctx ends first.
func (m *mailboxes) turn(ctx context.Context, channel string, owner bool) func() {
	if owner {
		return func() {}
	}
	m.mu.Lock()
	if m.slots == nil {
		m.slots = map[string]chan struct{}{}
	}
	slot := m.slots[channel]
	if slot == nil {
		slot = make(chan struct{}, othersAtOnce)
		m.slots[channel] = slot
	}
	m.mu.Unlock()
	select {
	case slot <- struct{}{}:
		return func() { <-slot }
	case <-ctx.Done():
		return nil
	}
}

// locksMu serialises taking a conversation (conv, acquire) with letting an
// idle scratch one go (release), so nobody is handed one being dropped.
var locksMu sync.Mutex

func (d *Daemon) conv(key string) *conversation {
	locksMu.Lock()
	defer locksMu.Unlock()
	c, _ := d.locks.LoadOrStore(key, &conversation{})
	return c.(*conversation)
}

// acquire is conv for a run that knows when it ends: background work in a
// scratch conversation ("…#protocol-…", "…#task-…", "…#watch-…"). The
// release lets go of the conversation once nothing uses it and it holds
// nothing, so thousands of scheduled runs don't each leave one behind.
func (d *Daemon) acquire(key string) (*conversation, func()) {
	locksMu.Lock()
	v, _ := d.locks.LoadOrStore(key, &conversation{})
	c := v.(*conversation)
	c.refs++
	locksMu.Unlock()
	var once sync.Once
	return c, func() { once.Do(func() { d.releaseConv(key, c) }) }
}

func (d *Daemon) releaseConv(key string, c *conversation) {
	locksMu.Lock()
	defer locksMu.Unlock()
	if c.refs--; c.refs > 0 || !memory.IsScratch(key) || !c.idle() {
		return
	}
	d.locks.CompareAndDelete(key, c)
}

// idle reports whether a conversation holds nothing worth keeping: no turn,
// nothing waiting, nothing asked or offered.
func (c *conversation) idle() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.busy && !c.draining && len(c.held) == 0 && len(c.inbox) == 0 && len(c.runs) == 0 &&
		len(c.raised) == 0 && len(c.asked) == 0 && len(c.own) == 0 && len(c.theirs) == 0 &&
		len(c.prior) == 0 && c.offer == nil
}

// begin takes the conversation for a turn.
func (c *conversation) begin() {
	c.turn.Lock()
	c.mu.Lock()
	c.busy = true
	c.mu.Unlock()
}

// end releases it, first recording any proactive messages that arrived during
// the turn, after the turn's own messages.
func (d *Daemon) end(key string, c *conversation) {
	c.mu.Lock()
	for _, m := range c.held {
		if err := d.store.AppendMessage(context.Background(), key, m); err != nil {
			d.log.Warn("notify: record", "chat", key, "err", err)
		}
	}
	c.held, c.busy = nil, false
	c.mu.Unlock()
	c.turn.Unlock()
}

// record adds a message the twin sent on its own (a reminder, a task update)
// to a conversation. While a turn is running there it is held back until the
// turn ends: written mid-turn it would land between a tool call and its
// result and break the model's tool loop.
func (d *Daemon) record(ctx context.Context, key string, m llm.Message) {
	c, release := d.acquire(key) // a task's scratch one is let go after
	defer release()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		c.held = append(c.held, m)
		return
	}
	if err := d.store.AppendMessage(ctx, key, m); err != nil {
		d.log.Warn("notify: record", "chat", key, "err", err)
	}
}

// handle is every channel's entry point. Streaming channels (voice, the
// terminal) answer in line, as the person is waiting on the spot. Messaging
// channels drop the message in its conversation's mailbox and return at once,
// so a long turn never stalls the transport (whose staleness checks would
// then throw the next message away), and messages that arrive meanwhile are
// answered in order when it ends.
func (d *Daemon) handle(ctx context.Context, in channels.Inbound) {
	d.noteHeard(in) // proactive.go: whether the owner is in the room
	if ch, ok := d.channel(in.Channel); ok {
		if st, ok := ch.(channels.Streamer); ok {
			d.handleStreaming(ctx, in, st)
			return
		}
	}
	d.post(ctx, in)
}

// post queues a channel message and makes sure a worker is answering. It
// never sends anything itself: it runs on the transport's receive loop, and
// some transports (Signal) can only finish a send by reading on that loop.
func (d *Daemon) post(ctx context.Context, in channels.Inbound) {
	if d.stopPosted(ctx, in) {
		return // "stop" doesn't wait behind the turn it stops
	}
	c := d.conv(in.Key())
	var pre *preheard
	if d.mayBeSpokenStop(in) {
		pre = &preheard{done: make(chan struct{})}
	}
	c.mu.Lock()
	if pre != nil && len(c.runs) == 0 {
		pre = nil // nothing of theirs to stop here: it is heard in its turn
	}
	if waiting(c.inbox, in.IsOwner) >= inboxLimit {
		// Past the limit a message is dropped. The owner hears so once per
		// backlog, from another goroutine; anyone else's is only logged, so
		// a flood is never echoed back into the chat.
		tell := in.IsOwner && !c.overflowed
		if in.IsOwner {
			c.overflowed = true
		}
		c.mu.Unlock()
		d.log.Warn("inbox full; message dropped", "chat", in.Key())
		if tell {
			go func() {
				ctx := context.WithValue(context.WithoutCancel(ctx), answeringKey{}, in)
				if err := d.Send(ctx, in.Key(), "I'm still working through your earlier messages. Send that one again in a minute and I'll pick it up."); err != nil {
					d.log.Error("send", "chat", in.Key(), "err", err)
				}
			}()
		}
		return
	}
	start := !c.draining
	if start && !d.mail.open(in.Channel, in.IsOwner) {
		c.mu.Unlock()
		d.log.Warn("too many chats waiting; ignoring this one", "chat", in.Key())
		return
	}
	c.inbox = append(c.inbox, queued{ctx: ctx, in: in, at: clock(), pre: pre})
	c.draining = true
	c.mu.Unlock()
	if pre != nil {
		go d.hearAhead(ctx, c, in, pre) // a spoken "stop" mustn't wait behind the turn it stops
	}
	if start {
		go d.drain(c, in.Channel)
	}
}

// drain answers a conversation's mailbox until it is empty. Plain messages
// that piled up behind a long turn are one train of thought ("also add milk",
// "and eggs"), so they are answered together; the message that starts the
// mailbox, commands, yes/no replies and attachments are always taken on
// their own.
func (d *Daemon) drain(c *conversation, channel string) {
	defer d.mail.close(channel)
	for first := true; ; first = false {
		c.mu.Lock()
		if len(c.inbox) == 0 {
			c.draining = false
			c.mu.Unlock()
			return
		}
		q, n := c.inbox[0], 1
		if !first && d.plain(q.in) {
			for n < len(c.inbox) && d.plain(c.inbox[n].in) && c.inbox[n].in.Sender == q.in.Sender && c.inbox[n].in.IsOwner == q.in.IsOwner {
				q.in.Text += "\n" + c.inbox[n].in.Text
				n++
			}
		}
		c.inbox = c.inbox[n:]
		if waiting(c.inbox, true) < inboxLimit {
			c.overflowed = false // room again: a later overflow is told again
		}
		run := c.queuedTurn(q.in) // off the mailbox, but a "stop" still finds it (interrupt.go)
		if q.pre != nil {
			q.pre.run = run // its own turn, which its stop doesn't cut
		}
		c.mu.Unlock()
		// The daemon's context, not the channel's: a channel reconnecting
		// mid-turn must not cancel work the owner already asked for.
		ctx := d.runCtx
		if ctx == nil {
			ctx = q.ctx
		}
		release := d.mail.turn(ctx, q.in.Channel, q.in.IsOwner)
		if release == nil {
			untrack(run)
			continue // shutting down
		}
		qctx := context.WithValue(withArrival(ctx, q.at), queuedRunKey{}, run)
		if q.pre != nil {
			qctx = context.WithValue(qctx, preheardKey{}, q.pre)
		}
		d.handleQueued(qctx, q.in)
		untrack(run) // if the message never became a turn (paused, an attachment it can't open)
		release()
	}
}

// plain reports whether a message is ordinary text rather than a command or
// a yes/no to an approval.
func (d *Daemon) plain(in channels.Inbound) bool {
	t := strings.TrimSpace(in.Text)
	if t == "" || strings.HasPrefix(t, "/") || in.Media != "" {
		return false
	}
	_, decision := d.parseReply(t)
	return !decision
}

// present runs a turn that arrived through the API (the terminal and voice
// clients, remote devices, the presence screen's composer) and mirrors it on
// the presence screen and orb, as handle does for channels.
func (d *Daemon) present(ctx context.Context, in channels.Inbound, ev agent.Events) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(in.Text), "/") {
		return d.message(ctx, in, ev) // a command ("/reload"), not conversation
	}
	// Every screen shows the exchange. One typed on a screen is marked with
	// the page it came from (api.ClientFrom), which shows its own lines as
	// they are typed and skips these; another paired screen shows them. A
	// screen that doesn't say which page it is isn't echoed, as before.
	// Each line says its channel too: a wall screen that only looks shows
	// only what was said out loud (api's screen_private.go).
	origin := api.ClientFrom(ctx)
	echo := in.Channel != "screen" || origin != ""
	from := map[string]string{"channel": in.Channel}
	if origin != "" {
		from["origin"] = origin
	}
	if reply, ok := d.stopPresented(ctx, in); ok { // not behind the turn it stops
		if echo {
			d.bus.Publish(events.Event{Kind: "heard", Text: in.Text, Data: from})
			d.bus.Publish(events.Event{Kind: "said", Text: reply, Data: from})
		}
		return reply, nil
	}
	if echo {
		d.bus.Publish(events.Event{Kind: "heard", Text: in.Text, Data: from})
	}
	p := d.bus.Begin("thinking")
	defer p.End()
	onTool := ev.OnTool
	ev.OnTool = func(tool, caption string) {
		if caption != "" {
			d.bus.Publish(events.Event{Kind: "note", Text: caption, Data: from})
		}
		if onTool != nil {
			onTool(tool, caption)
		}
	}
	reply, err := d.message(ctx, in, ev)
	if echo && reply != "" {
		d.bus.Publish(events.Event{Kind: "said", Text: reply, Data: from})
	}
	d.turnFailed(in, err) // react.go
	return reply, err
}
