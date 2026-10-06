package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// "Stop", "cancel that", "never mind": the owner can cut short what the twin
// is doing, on any channel, and is told what was stopped. A stop never waits
// behind the turn it is meant to stop: a messaging channel's is caught as it
// arrives (post), before it would queue, and so is one through the local API
// (present). It cuts short the turn running in that chat (or about to start
// there), with any approved action it set going, even one carried out in a
// background run's own conversation, and lets go of the owner's messages
// queued behind it. When nothing runs in that chat, a plain "stop" stops
// what the owner has running anywhere else (a long job started from the
// screen, say); "cancel that" or "never mind" is then just an answer to
// what the twin asked there, and a stop on IRC or by email, where anyone
// could claim to be the owner, reaches only its own chat. Anything the twin
// already did stays done, and it says so. Background tasks have their own
// "cancel task".

// running is a turn, or an approved action being carried out, in one or
// more conversations: an action is tracked where it runs, in its home chat
// and in the chat whose turn set it going, so "stop" said where the owner is
// reaches it.
type running struct {
	cancel    context.CancelFunc      // a queued turn's is set when it starts (guarded by its conversation's mu)
	doing     string                  // what it is about: the words it answers, or what it is doing
	said      bool                    // doing is someone's words ("book the table"), not a description
	owner     bool                    // the owner's own; someone else's turn is stopped only from its own chat
	at        string                  // the conversation it belongs to, for saying where it was
	where     []*conversation         // the conversations it is tracked in
	started   bool                    // a queued turn has started (converse); guarded like cancel
	stopped   atomic.Bool             // cut short by a stop
	keepAsks  atomic.Bool             // the stop came from a device that may not decide requests: what it asked stays waiting
	dropped   atomic.Int32            // the owner's messages queued behind it, let go with it
	withdrawn int                     // requests it had raised, turned down with it
	carrying  atomic.Pointer[running] // for a turn: the approved action it set going, if any
}

// label names what r was doing, for a sentence: the words it answered,
// quoted, or what it was doing ("carrying out #12 (send email: to boss)").
func (r *running) label() string {
	if r.said {
		return quoted(r.doing)
	}
	return r.doing
}

// track registers work starting in conversation at, and tracked in the
// conversations where (each once), so a stop said in any of them cuts it
// short.
func track(ctx context.Context, doing string, said, owner bool, at string, where ...*conversation) (context.Context, *running) {
	ctx, cancel := context.WithCancel(ctx)
	r := &running{cancel: cancel, doing: doing, said: said, owner: owner, at: at}
	for _, c := range where {
		if slices.Contains(r.where, c) {
			continue
		}
		r.where = append(r.where, c)
		c.mu.Lock()
		c.runs = append(c.runs, r)
		c.mu.Unlock()
	}
	return ctx, r
}

// queuedTurn registers a message a mailbox worker has just taken, before
// its turn starts: a "stop" sent meanwhile reaches it rather than finding
// nothing running. The caller holds c.mu; converse picks it up (startTurn).
func (c *conversation) queuedTurn(in channels.Inbound) *running {
	doing, said := in.Text, true
	if strings.TrimSpace(in.Text) == "" && in.Media != "" {
		// A voice note or photo on its own: its words are known only once
		// it is opened (startTurn), and a stop meanwhile says what it cut.
		doing, said = "opening your "+mediaName(in.Media), false
	}
	r := &running{cancel: func() {}, doing: doing, said: said, owner: in.IsOwner, at: in.Key(), where: []*conversation{c}}
	c.runs = append(c.runs, r)
	return r
}

// mediaName is an attachment kind as the owner would say it.
func mediaName(kind string) string {
	switch kind {
	case channels.Voice:
		return "voice note"
	case channels.Photo, channels.Video:
		return kind
	}
	return "file"
}

type queuedRunKey struct{}

// startTurn registers the turn answering in: the one its mailbox worker
// registered when it took the message (queuedTurn), or a new one.
func (c *conversation) startTurn(ctx context.Context, in channels.Inbound) (context.Context, *running) {
	q, _ := ctx.Value(queuedRunKey{}).(*running)
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if q != nil && !q.started && slices.Contains(c.runs, q) {
		q.cancel, q.started = cancel, true
		// What the turn answers, as it will be answered: a voice note's
		// transcript or a photo's caption (media.go), not the empty text
		// the mailbox took.
		q.doing, q.said = in.Text, true
		if q.stopped.Load() {
			cancel() // stopped before it started
		}
		return ctx, q
	}
	r := &running{cancel: cancel, doing: in.Text, said: true, owner: in.IsOwner, at: in.Key(), where: []*conversation{c}, started: true}
	c.runs = append(c.runs, r)
	return ctx, r
}

// untrack ends r and reports whether a stop cut it short.
func untrack(r *running) bool {
	var cancel context.CancelFunc
	for _, c := range r.where {
		c.mu.Lock()
		if i := slices.Index(c.runs, r); i >= 0 {
			c.runs = slices.Delete(c.runs, i, i+1)
		}
		if cancel == nil {
			cancel = r.cancel
		}
		c.mu.Unlock()
	}
	if cancel != nil {
		cancel()
	}
	return r.stopped.Load()
}

// stop cuts short what is running here (only the owner's own work when
// onlyOwner) and says what that was, and where. With dropQueued, the
// owner's messages waiting behind it go too: they were part of what is
// being stopped.
func (c *conversation) stop(onlyOwner, dropQueued bool) []stoppedWork {
	return c.halt(onlyOwner, dropQueued, false)
}

// halt is stop; with keepAsks, what the stopped work had asked the owner to
// approve stays waiting rather than being turned down: the stop came from a
// paired device that may talk but not decide requests (stopper), and a stop
// is not a way round that.
func (c *conversation) halt(onlyOwner, dropQueued, keepAsks bool) []stoppedWork {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cut []*running
	for _, r := range c.runs {
		if onlyOwner && !r.owner || !r.stopped.CompareAndSwap(false, true) {
			continue // not ours to stop, or already stopped (from another chat it is tracked in)
		}
		if keepAsks {
			r.keepAsks.Store(true) // before the turn wakes to its cancel (converse)
		}
		r.cancel()
		cut = append(cut, r)
	}
	out := make([]stoppedWork, len(cut))
	for i, r := range cut {
		out[i] = stoppedWork{r.at, r.label(), r}
	}
	if len(cut) == 0 || !dropQueued {
		return out
	}
	dropped := int32(c.dropQueuedLocked())
	for _, r := range cut {
		r.dropped.Store(dropped)
	}
	return out
}

// dropQueued lets go of the owner's messages waiting here and counts them.
func (c *conversation) dropQueued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropQueuedLocked()
}

func (c *conversation) dropQueuedLocked() int {
	kept, dropped := c.inbox[:0], 0
	for _, q := range c.inbox {
		if q.in.IsOwner {
			dropped++
			continue
		}
		kept = append(kept, q)
	}
	clear(c.inbox[len(kept):])
	c.inbox = kept
	return dropped
}

type turnRunKey struct{}

// converse answers a message (dispatch) so that the owner's stop can cut it
// short; a turn that was stopped says so, and what it had asked for is
// turned down. A stop that came once the work was done (an approved action
// finished, a reply written) changes nothing, and the reply says so.
func (d *Daemon) converse(ctx context.Context, c *conversation, in channels.Inbound, ev agent.Events) (string, error) {
	ctx, run := c.startTurn(ctx, in)
	var reply string
	var err error
	if ctx.Err() == nil { // a stop may have reached it while it waited its turn
		reply, err = d.dispatch(context.WithValue(ctx, turnRunKey{}, run), in, ev)
	}
	if !untrack(run) {
		return reply, err
	}
	inner := run.carrying.Load()
	innerCut := inner != nil && inner.stopped.Load()
	if !innerCut && err == nil && reply != "" {
		// The stop came as it finished, or once the approved action it set
		// going had finished: all of it happened.
		return reply + "\n\n" + tooLate(run.dropped.Load()), nil
	}
	if innerCut {
		run.doing, run.said = inner.doing, inner.said // "carrying out #12 (...)", not "yes 12"
	}
	key := in.Key()
	ctx = context.WithoutCancel(ctx)
	if !run.keepAsks.Load() {
		run.withdrawn = d.withdraw(ctx, c, key, in.Channel)
	}
	d.markStopped(ctx, key)
	return stoppedLine(key, run), nil
}

// withdraw turns down what a stopped turn had asked the owner to approve:
// the question never reached them, and "stop" means don't.
func (d *Daemon) withdraw(ctx context.Context, c *conversation, key, channel string) int {
	n := 0
	by := Decider{Method: "stop", Via: channel}.String()
	for _, id := range c.takeRaised(key) {
		if ap, err := d.store.GetApproval(ctx, id); err == nil && ap.Status == "pending" && ap.ChatKey == key {
			if _, err := d.agent.DecideBy(ctx, key, id, false, by); err == nil {
				n++
			}
		}
	}
	return n
}

// stopNote is what the conversation keeps of a stop.
const stopNote = "(Stopped here: the user said stop. Anything already done stays done.)"

// markStopped leaves the conversation whole after a stop: a tool call cut
// off gets its result, and the history says the turn was stopped, so the
// next turn neither trips over it nor carries on as if nothing happened.
func (d *Daemon) markStopped(ctx context.Context, key string) {
	h, err := d.store.History(ctx, key, 1)
	if err != nil || len(h) == 0 {
		return
	}
	last := h[0]
	if last.Role == llm.RoleAssistant {
		if last.PlainText() == stopNote {
			return // already said
		}
		results := llm.Message{Role: llm.RoleUser}
		for _, b := range last.Blocks {
			if b.Type == llm.BlockToolUse {
				results.Blocks = append(results.Blocks, llm.Block{Type: llm.BlockToolResult, ToolUseID: b.ToolUseID, IsError: true, Text: "Not finished: the user said stop."})
			}
		}
		if len(results.Blocks) == 0 {
			return // it had finished what it was saying
		}
		if err := d.store.AppendMessage(ctx, key, results); err != nil {
			d.log.Warn("stop: record", "chat", key, "err", err)
			return
		}
	}
	if err := d.store.AppendMessage(ctx, key, llm.Text(llm.RoleAssistant, stopNote)); err != nil {
		d.log.Warn("stop: record", "chat", key, "err", err)
	}
}

// stoppedLine is what a stopped turn tells its chat.
func stoppedLine(key string, r *running) string {
	if channelOf(key) == "voice" {
		return "OK, stopped."
	}
	var b strings.Builder
	was := r.doing
	if r.said {
		was = "on " + quoted(r.doing)
	}
	fmt.Fprintf(&b, "OK, I've stopped. I was %s; anything already done stays done.", was)
	switch r.withdrawn {
	case 0:
	case 1:
		b.WriteString(" I've dropped the request that was waiting for your OK, too.")
	default:
		fmt.Fprintf(&b, " I've dropped the %d requests that were waiting for your OK, too.", r.withdrawn)
	}
	switch dropped := r.dropped.Load(); dropped {
	case 0:
	case 1:
		b.WriteString(" I let go of the message you sent after it as well; send it again if you still want it.")
	default:
		fmt.Fprintf(&b, " I let go of the %d messages you sent after it as well; send them again if you still want them.", dropped)
	}
	return b.String()
}

// tooLate is what a reply adds when the stop came once it was all done;
// messages sent after it were still let go.
func tooLate(dropped int32) string {
	switch dropped {
	case 0:
		return "(That had already finished when you said stop.)"
	case 1:
		return "(That had already finished when you said stop. I let go of the message you sent after it; send it again if you still want it.)"
	}
	return fmt.Sprintf("(That had already finished when you said stop. I let go of the %d messages you sent after it; send them again if you still want them.)", dropped)
}

// quoted is a short, one-line quotation of what the owner asked for.
func quoted(s string) string {
	return "“" + brief(strings.Join(strings.Fields(s), " ")) + "”"
}

// stopPosted deals with the owner's stop from a messaging channel as it
// arrives, before it would wait behind the turn it is meant to stop. It
// runs on the transport's receive loop, so it never sends anything there
// itself. It reports whether the message needs nothing more.
func (d *Daemon) stopPosted(ctx context.Context, in channels.Inbound) bool {
	if !in.IsOwner {
		return false
	}
	stop, whole, firm := d.isStop(in.Text)
	if !stop {
		return false
	}
	key := in.Key()
	c := d.conv(key)
	if cut := c.stop(false, true); len(cut) > 0 {
		d.store.Audit(ctx, "turn.stopped", key, truncate(strings.Join(labels(cut), "; "), 300))
		return whole // the stopped turn says what stopped; anything more is answered after it
	}
	if !whole {
		return false
	}
	var reply string
	if n := c.dropQueued(); n > 0 {
		// Taken off the mailbox before its turn began: nothing was done.
		reply = "OK, I hadn't started on that yet, so I've let it go. Send it again if you still want it."
		if n > 1 {
			reply = fmt.Sprintf("OK, I hadn't started on those %d messages yet, so I've let them go. Send them again if you still want them.", n)
		}
		d.store.Audit(ctx, "turn.stopped", key, fmt.Sprintf("%d queued", n))
	} else {
		stopped := d.stopFrom(ctx, key, firm)
		if len(stopped) == 0 {
			return false // nothing to stop: it is just a message ("cancel that" may answer a question)
		}
		reply = stoppedElsewhere(stopped)
	}
	go func() {
		ctx := context.WithValue(context.WithoutCancel(ctx), answeringKey{}, in)
		d.record(ctx, key, llm.Text(llm.RoleUser, in.Text))
		d.record(ctx, key, llm.Text(llm.RoleAssistant, reply))
		if err := d.Send(ctx, key, reply); err != nil {
			d.log.Error("send", "chat", key, "err", err)
		}
	}()
	return true
}

// stopFrom is a stop said in chat key, where nothing is running, reaching
// the owner's work elsewhere. Only a plain stop ("stop", "abort") goes that
// far: "cancel that", "never mind" and "don't" are just as often the answer
// to what the twin asked in this chat, so they stay here. And only from a
// chat where it is surely the owner: on IRC or by email anyone could claim
// to be.
func (d *Daemon) stopFrom(ctx context.Context, key string, firm bool) []stoppedWork {
	if !firm || forgeable(channelOf(key)) {
		return nil
	}
	return d.stopElsewhere(ctx, key)
}

// labels lists what stopped work was doing.
func labels(ws []stoppedWork) []string {
	ws = named(ws)
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.doing
	}
	return out
}

// stopPresented is stopPosted for a message through the local API (the
// presence screen's composer, a terminal or voice client). It returns the
// reply when the message needs nothing more.
func (d *Daemon) stopPresented(ctx context.Context, in channels.Inbound) (string, bool) {
	if !in.IsOwner {
		return "", false
	}
	stop, whole, firm := d.isStop(in.Text)
	if !stop {
		return "", false
	}
	key := in.Key()
	if cut := d.conv(key).halt(false, false, stopper(ctx)); len(cut) > 0 {
		doing := labels(cut)
		d.store.Audit(ctx, "turn.stopped", key, truncate(strings.Join(doing, "; "), 300))
		if !whole {
			return "", false // the rest is answered once the stopped turn lets go
		}
		return fmt.Sprintf("OK, I've stopped %s. Anything already done stays done.", strings.Join(doing, "; ")), true
	}
	if !whole {
		return "", false
	}
	stopped := d.stopFrom(ctx, key, firm)
	if len(stopped) == 0 {
		return "", false
	}
	return stoppedElsewhere(stopped), true
}

// Interrupt stops everything the owner has running (turns and approved
// actions, on every channel) as their "stop" does, and says what that was.
// It is for front ends with a stop button, or a voice barge-in.
func (d *Daemon) Interrupt(ctx context.Context) (string, bool) {
	stopped := d.stopElsewhere(ctx, "")
	if len(stopped) == 0 {
		return "Nothing's running right now.", false
	}
	return stoppedElsewhere(stopped), true
}

// stopper reports whether a stop came through the API from a paired device
// that may not decide requests: it stops the work, but leaves what the work
// had asked the owner waiting for them (halt).
func stopper(ctx context.Context) bool {
	return cantApprove(ctx) // devices.go
}

// stoppedWork is something a stop cut short: where, and what it was doing.
type stoppedWork struct {
	key, doing string
	run        *running
}

// named leaves out a turn whose approved action was stopped with it: "yes
// 12" is told as what it was carrying out.
func named(ws []stoppedWork) []stoppedWork {
	out := ws[:0:0]
	for _, w := range ws {
		if in := w.run.carrying.Load(); in != nil && slices.ContainsFunc(ws, func(o stoppedWork) bool { return o.run == in }) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// stopElsewhere stops the owner's work in every conversation but except.
func (d *Daemon) stopElsewhere(ctx context.Context, except string) []stoppedWork {
	var out []stoppedWork
	keep := stopper(ctx)
	d.locks.Range(func(k, v any) bool {
		if k.(string) != except {
			out = append(out, v.(*conversation).halt(true, false, keep)...)
		}
		return true
	})
	out = named(out)
	slices.SortStableFunc(out, func(a, b stoppedWork) int { return strings.Compare(a.key, b.key) })
	for _, w := range out {
		d.store.Audit(ctx, "turn.stopped", w.key, truncate(w.doing, 300))
	}
	return out
}

// stoppedElsewhere tells the owner what their stop cut short in other chats.
func stoppedElsewhere(ws []stoppedWork) string {
	if len(ws) == 1 {
		return fmt.Sprintf("OK, I've stopped what I was doing %s: %s. Anything already done stays done.", where(ws[0].key), ws[0].doing)
	}
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = fmt.Sprintf("%s (%s)", w.doing, where(w.key))
	}
	return fmt.Sprintf("OK, I've stopped the %d things I was doing: %s. Anything already done stays done.", len(ws), strings.Join(parts, "; "))
}

// where names the place a conversation happens, for a sentence.
func where(key string) string {
	switch name := channelOf(homeKey(key)); name {
	case "screen":
		return "on the screen"
	case "voice":
		return "by voice"
	case "cli":
		return "in the terminal"
	default:
		return "on " + channelLabel(name)
	}
}

// isStop reports whether text tells the twin to stop: the whole message
// ("stop", "cancel that", "never mind, Mirrin"), or its first words
// before more to say ("Stop, not that flight!", whole false). firm is a
// plain stop ("stop", "abort"), which may reach work in other chats; "cancel
// that" or "never mind" is about this one.
func (d *Daemon) isStop(text string) (stop, whole, firm bool) {
	names := d.twinNames()
	if ok, firm := stopWords(text, names); ok {
		return true, true, firm
	}
	if i := strings.IndexAny(text, ",.!;:?—–"); i > 0 && strings.TrimSpace(strings.Trim(text[i:], ",.!;:?—– ")) != "" {
		if ok, firm := stopWords(text[:i], names); ok {
			return true, false, firm
		}
	}
	return false, false, false
}

// twinNames are the words the owner may address the twin by.
func (d *Daemon) twinNames() []string {
	d.cmu.RLock()
	p, name := d.persona, d.cfg.Name
	d.cmu.RUnlock()
	return append([]string{p.Name, p.Spoken(), p.WakeWord, name}, p.WakeAliases...)
}

// stopPhrases are what a stop says, once the padding is gone; true for a
// plain stop, which may reach work in other chats (isStop).
var stopPhrases = map[string]bool{
	"stop": true, "stop it": true, "stop that": true, "stop this": true, "stop everything": true,
	"stop what youre doing": true, "stop what you are doing": true, "halt": true, "abort": true,
	"cancel": false, "cancel it": false, "cancel that": false, "cancel this": false,
	"never mind": false, "nevermind": false, "nvm": false, "forget it": false, "forget that": false,
	"dont": false, "dont do it": false, "dont do that": false, "hold it": false, "enough": false,
}

// stopPadding may come with a stop without changing it: "no, stop!", "ok
// stop now please", "wait, cancel that".
var stopPadding = map[string]bool{
	"please": true, "pls": true, "ok": true, "okay": true, "oh": true, "hey": true, "actually": true,
	"wait": true, "no": true, "just": true, "now": true, "right": true, "sorry": true, "hang": true, "on": true,
}

func stopWords(text string, names []string) (stop, firm bool) {
	ignore := map[string]bool{}
	for _, n := range names {
		for _, w := range plainWords(n) {
			ignore[w] = true
		}
	}
	var words []string
	for _, w := range plainWords(text) {
		if stopPadding[w] || ignore[w] {
			continue
		}
		if n := len(words); n > 0 && words[n-1] == w {
			continue // "stop stop stop"
		}
		words = append(words, w)
	}
	if len(words) == 0 {
		return false, false
	}
	firm, stop = stopPhrases[strings.Join(words, " ")]
	return stop, firm
}

// plainWords lowercases text into words of letters and digits, keeping
// "don't" as one word ("dont").
func plainWords(text string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == '\'' || r == '’':
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}
