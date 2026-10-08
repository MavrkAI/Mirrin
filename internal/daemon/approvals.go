package daemon

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tasks"
)

// How approvals reach a decision.
//
// Every approval belongs to a home conversation: the chat it was raised in,
// or the chat that started the background run (task, protocol, watcher) that
// raised it. "yes 12" decides #12 there, on the presence screen, or in the
// chat the home's messages go to when it can't be reached itself. A bare
// "yes" or "no" decides only when the twin's latest message to the owner
// asked about exactly one approval, a short while ago, and that approval is
// recent too; when that isn't so but something is plausibly meant, the twin
// asks which one, showing each request as stored. Older approvals never
// capture a bare yes unless the twin has just shown them that way. Requests
// made for someone else in a shared chat need "yes N". An approved action
// runs to the end even if whoever approved it goes away; deciding is a claim,
// so an approval is carried out once however many times it is answered; and
// a task's step goes back through its task so the board and the owner hear
// how it went.

// askFresh is how long a question stays open for a bare yes or no, and how
// recent an approval must be to be decided by one.
const askFresh = 30 * time.Minute

// approvalTimeout bounds an approved action and the model's follow-up.
var approvalTimeout = 15 * time.Minute

// approvalClock starts an approved action's time limit. A variable so tests
// can see when it starts.
var approvalClock = func(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, approvalTimeout)
}

// approvalTurnWait runs just before an approval waits for its chat's turn.
// A variable so tests can see it get there.
var approvalTurnWait = func() {}

// clock is the time, replaceable in tests.
var clock = time.Now

// reScratch matches the suffix a background run adds to its home chat's key
// ("whatsapp:X#task-09271", "...#protocol-20260927-150405", a scheduled
// run's "...#protocol-20260927-150405-3", a call summary's
// "...#call-20260927-150405.000", a follow-up's "...#followup-42", a bill
// read from the mail's "...#watch-bills-20260927-150405-1"), and not
// a room that merely has a similar name ("irc:#watch-party"). A live call's
// own conversation ("voice:phone#call-<id>") is not a run of any chat: see
// isCall.
var reScratch = regexp.MustCompile(`#(?:task-\d+|phone-\d+|followup-\d+|(?:protocol|watch|watch-bills|patterns|nudge|portrait|firstlook|call)-\d{8}-\d{6}(?:\.\d{3})?(?:-\d+)?)$`)

// homeKey is the conversation a chat key belongs to: itself, or the chat a
// background run was started from.
func homeKey(key string) string {
	for {
		loc := reScratch.FindStringIndex(key)
		if loc == nil {
			return key
		}
		key = key[:loc[0]]
	}
}

func channelOf(key string) string {
	name, _ := channels.SplitKey(key)
	return name
}

// reach is the conversation a message for chatKey is delivered to: its own
// when its channel is running, otherwise the owner's chat (see Send). A live
// call's requests reach the owner's chat: only the twin and the person on
// the line are in the call.
func (d *Daemon) reach(chatKey string) string {
	if _, ok := d.channel(channelOf(chatKey)); ok && !isCall(chatKey) {
		return chatKey
	}
	if owner := d.ownerChatKey(); owner != "" {
		return owner
	}
	return chatKey
}

// answerable reports whether the owner may decide ap from conversation key:
// its home chat; the presence screen, which shows every request; or, when
// the home can't be reached (a task started from the terminal, a protocol
// run from voice mode) or its channel couldn't deliver (forward.go), the
// chat its messages were delivered to instead. A channel whose sender can
// be forged (an IRC nick, a From: header) only answers its own. A request
// raised on the screen is answered there (a task's step aside), so no two
// chats ever wait on each other's turn.
func (d *Daemon) answerable(key string, ap memory.Approval) bool {
	home := homeKey(ap.ChatKey)
	if home == key || channelOf(key) == "screen" {
		return true
	}
	if forgeable(channelOf(key)) {
		return false
	}
	if channelOf(home) == "screen" {
		if _, task := d.tasks.ByKey(ap.ChatKey); !task {
			return false
		}
	}
	if d.reach(home) == key {
		return true
	}
	return slices.ContainsFunc(d.fwd.from(key), func(k string) bool { return homeKey(k) == home })
}

// isCall reports whether key is a live phone call's conversation (phone.go):
// only the twin and the person on the line are in it, so its requests are
// answered where the owner is, and nothing about them is said on the call's
// "voice" channel, which would read it aloud in the room.
func isCall(key string) bool {
	return key == phoneChat || strings.HasPrefix(key, phoneChat+"#call-")
}

// ownersOwnChat reports whether only the owner talks in key: their own chat
// on a messaging channel, or a local front end (terminal, voice, API). Only
// such a chat takes "yes N" for someone else's request, which is never raised
// in one, so no two chats ever wait on each other's turn.
func (d *Daemon) ownersOwnChat(key string) bool {
	if !channels.IsMessaging(key) {
		return true
	}
	name, id := channels.SplitKey(key)
	ch, ok := d.channel(name)
	return ok && id != "" && id == ch.OwnerChatID()
}

// forgeable reports whether anyone could claim to be the owner on a channel.
func forgeable(channel string) bool { return channel == "irc" || channel == "mail" }

// forSomeoneElse reports whether someone other than the owner asked for
// approval id (strangers.go), including in the turn that follows a decision
// for them: only "yes N" decides it, never a bare yes. The mark is stored,
// so it holds whichever conversation or turn the owner answers from.
func (d *Daemon) forSomeoneElse(ctx context.Context, id int64) bool {
	_, ok := d.agent.ApprovalRequester(ctx, id)
	return ok
}

// The twin asks about an approval by number the way it is told to: the
// words quoted ("reply "yes 12" or "no 12""), after "reply" or "say", or as
// a yes/no pair. A number in passing ("there are no 2 seats") is not an ask.
var (
	reAskQuoted = regexp.MustCompile("(?i)[\"“'‘*_`]\\s*yes\\s*#?(\\d+)\\s*[\"”'’*_`]")
	reAskVerb   = regexp.MustCompile(`(?i)\b(?:reply|replying|say|saying|answer|respond|type|text|send)\b[^.!?\n]{0,24}?\byes\s*#?(\d+)\b`)
	reAskPair   = regexp.MustCompile(`(?i)\b(yes|no)\s*#?(\d+)\b`)
)

// askedIDs lists the approval numbers a message asks the owner to answer.
func askedIDs(text string) []int64 {
	var ids []int64
	add := func(s string) {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, re := range []*regexp.Regexp{reAskQuoted, reAskVerb} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			add(m[1])
		}
	}
	pairs := reAskPair.FindAllStringSubmatch(text, -1)
	said := map[string]bool{}
	for _, m := range pairs {
		said[strings.ToLower(m[1])+m[2]] = true
	}
	for _, m := range pairs {
		if said["yes"+m[2]] && said["no"+m[2]] {
			add(m[2])
		}
	}
	return ids
}

// noteRaised records approvals raised in chatKey, this conversation or one
// of its background runs, until the twin tells the owner about them.
func (c *conversation) noteRaised(chatKey string, ids ...int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.raised == nil {
		c.raised = map[string][]int64{}
	}
	c.raised[chatKey] = append(c.raised[chatKey], ids...)
}

// takeRaised removes and returns what chatKey raised.
func (c *conversation) takeRaised(chatKey string) []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := c.raised[chatKey]
	delete(c.raised, chatKey)
	return ids
}

// noteReply records the reply to a turn in this conversation: the daemon's
// own check, if it wrote one (willAsk), or else what noteSpoke finds.
func (c *conversation) noteReply(turnKey, text string, toOwner bool) {
	c.mu.Lock()
	own, owned, lean := c.own, c.owned, c.ownLean
	c.own, c.owned, c.ownLean = nil, false, 0
	c.prior, c.priorLean = nil, 0 // the turn that could answer it in words is over
	if owned && toOwner {
		// The daemon wrote this reply and showed each request as stored.
		delete(c.raised, turnKey)
		c.asked, c.askedAt, c.vouched, c.askedLean = own, clock(), true, lean
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	c.noteSpoke(turnKey, text, toOwner)
}

// noteSpoke records the twin's latest message in this conversation. The
// reply to a turn announces the approvals raised in that turn (turnKey is
// where it ran). A message the twin sends on its own (turnKey "") can't be
// tied to the run that raised anything, so it asks only about approvals it
// quotes by number, as task updates do; others are left to "yes N" or a
// check. toOwner is false for a reply to someone else in a shared chat: what
// that turn raised then needs "yes N", and the owner has not just been asked
// anything.
func (c *conversation) noteSpoke(turnKey, text string, toOwner bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if toOwner {
		c.offer = nil // a yes now answers what was said since, not "stop asking?"
	}
	if !toOwner {
		c.theirs = append(c.theirs, c.raised[turnKey]...)
		if n := len(c.theirs); n > 32 {
			c.theirs = c.theirs[n-32:]
		}
		delete(c.raised, turnKey)
		if strings.TrimSpace(text) != "" {
			c.asked, c.vouched, c.askedLean = nil, false, 0
		}
		return
	}
	var ids []int64
	if turnKey != "" {
		ids = c.raised[turnKey]
		delete(c.raised, turnKey)
	} else {
		for k := range c.raised {
			if k != homeKey(k) {
				delete(c.raised, k)
			}
		}
	}
	for _, id := range askedIDs(text) {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	c.asked, c.askedAt, c.vouched, c.askedLean = ids, clock(), false, 0
}

// willAsk marks the reply the daemon is writing in this turn as asking about ids.
func (c *conversation) willAsk(ids ...int64) {
	c.mu.Lock()
	c.own, c.owned = ids, true
	c.mu.Unlock()
}

// willLean notes that the check the daemon is writing follows the owner's
// yes (or no), so a reply that only picks one ("the landlord one") carries
// that yes or no with it.
func (c *conversation) willLean(approve bool) {
	c.mu.Lock()
	c.ownLean = lean(approve)
	c.mu.Unlock()
}

func lean(approve bool) int8 {
	if approve {
		return 1
	}
	return -1
}

// moveOn closes the question because the owner's message isn't a plain yes
// or no. For the rest of this turn it is kept as the question that message
// may still answer in its own words (resolve_approval).
func (c *conversation) moveOn() {
	c.mu.Lock()
	c.prior, c.priorAt, c.priorLean = c.asked, c.askedAt, c.askedLean
	c.asked, c.vouched, c.own, c.owned, c.askedLean, c.ownLean = nil, false, nil, false, 0, 0
	c.mu.Unlock()
}

// priorAsk is what the twin had asked when the owner's latest message
// arrived, if that message wasn't a plain yes or no (moveOn).
func (c *conversation) priorAsk() (ids []int64, at time.Time, lean int8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.prior), c.priorAt, c.priorLean
}

// dropQuestion closes the twin's last question while something new is on its
// way to the owner (noticed then records what that asks), leaving alone the
// check a running turn may be writing (willAsk): a yes sent meanwhile is not
// an answer to what came before.
func (c *conversation) dropQuestion() {
	c.mu.Lock()
	c.asked, c.vouched, c.askedLean, c.offer = nil, false, 0, nil
	c.mu.Unlock()
}

// closeAsk closes the question: the owner has moved on, or the twin's
// latest words asked nothing.
func (c *conversation) closeAsk() {
	c.mu.Lock()
	c.asked, c.vouched, c.own, c.owned, c.askedLean, c.ownLean, c.offer = nil, false, nil, false, 0, 0, nil
	c.mu.Unlock()
}

func (c *conversation) lastAsk() (ids []int64, at time.Time, vouched bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked), c.askedAt, c.vouched
}

// isTheirs reports whether id was raised for someone other than the owner.
func (c *conversation) isTheirs(id int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.theirs, id)
}

// adopt hands approvals raised in another conversation's run (a background
// run's, or a home answered from elsewhere) to this conversation's current
// turn, whose reply will tell the owner about them.
func (d *Daemon) adopt(from, to string) {
	if ids := d.conv(homeKey(from)).takeRaised(from); len(ids) > 0 {
		d.conv(to).noteRaised(to, ids...)
	}
}

// parseReply reads a yes or no, allowing the owner to address the twin by name.
func (d *Daemon) parseReply(text string) (approvals.Reply, bool) {
	d.cmu.RLock() // UpdateConfig swaps both, and a mailbox worker may be asking
	p, name := d.persona, d.cfg.Name
	d.cmu.RUnlock()
	names := append([]string{p.Name, p.Spoken(), p.WakeWord, name}, p.WakeAliases...)
	return approvals.ParseReply(text, names...)
}

// recent reports whether an approval is young enough for a bare yes.
func recent(ap memory.Approval) bool { return clock().Sub(ap.CreatedAt) <= askFresh }

// decision handles the owner's yes or no to an approval from a chat. handled
// is false when the message isn't one: it then belongs to the conversation.
// The caller holds the conversation's turn.
func (d *Daemon) decision(ctx context.Context, in channels.Inbound, onDelta func(string)) (reply string, handled bool, err error) {
	if !in.IsOwner {
		return "", false, nil
	}
	key := in.Key()
	c := d.conv(key)
	ctx = withDecider(ctx, peerDecider(ctx, chatDecider(in))) // devices.go: the paired device, if one
	if reply, ok, err := d.alwaysReply(ctx, c, in, onDelta); ok {
		return reply, true, err
	}
	r, ok := d.parseReply(in.Text)
	if !ok {
		// "Yes always 12" where always can't be had is a yes to #12 all the same.
		r, ok = parseAlways(in.Text, d.twinNames())
	}
	if !ok {
		// Not a plain yes or no. It may still answer the twin's question in
		// its own words ("the landlord one, go ahead"): the model can settle
		// that with resolve_approval, checked against what was asked.
		c.moveOn()
		return "", false, nil
	}
	if r.ID != 0 {
		ap, err := d.store.GetApproval(ctx, r.ID)
		if err != nil {
			return fmt.Sprintf("I can't find approval #%d. Say /pending to see what's waiting.", r.ID), true, nil
		}
		if reply, refused := d.refuseAloud(ctx, in, *ap, r.Approve); refused { // voicehooks.go
			return reply, true, nil
		}
		if !d.answerable(key, *ap) {
			// Someone else's request waits on the owner, who may answer it
			// from their own chat (strangers.go): not from a chat whose
			// sender can be forged, and not from a shared chat, whose turn
			// another chat's answer could be waiting on in turn.
			if !forgeable(channelOf(key)) && d.ownersOwnChat(key) {
				if reply, ok := d.strangerDecision(ctx, ap, r.Approve); ok {
					return reply, true, nil
				}
			}
			return fmt.Sprintf("#%d was asked in another conversation, so I won't take a yes or no for it here. Answer it there, or from the presence screen.", r.ID), true, nil
		}
		reply, err := d.decideInChat(ctx, in, ap, r.Approve, onDelta)
		return reply, true, err
	}

	asked, at, vouched := c.lastAsk()
	var live, others []memory.Approval
	for _, id := range asked {
		ap, err := d.store.GetApproval(ctx, id)
		switch {
		case err != nil || ap.Status != "pending":
		case d.lapsed(*ap):
			d.lapse(d.base(ctx), ap) // too old to take a yes, however it is asked
		case d.forSomeoneElse(ctx, ap.ID):
			others = append(others, *ap) // only "yes N" decides someone else's request
		case d.answerable(key, *ap):
			live = append(live, *ap)
		}
	}
	switch {
	case len(live) > 1:
		return d.whichOne(c, in, live, r.Approve), true, nil
	case len(live) == 1:
		// It must be an answer: sent after the question, while it is open,
		// about something recent or something just shown as stored.
		ap := live[0]
		if clock().Sub(at) <= askFresh && !arrivedAt(ctx).Before(at) && (vouched || recent(ap)) {
			reply, err := d.decideInChat(ctx, in, &ap, r.Approve, onDelta)
			return reply, true, err
		}
		return d.confirm(c, in, ap, r.Approve), true, nil
	}
	// The twin's latest words asked only about someone else's request: say
	// how to answer it, rather than leave a bare yes to the model.
	if len(others) > 0 && clock().Sub(at) <= askFresh && !arrivedAt(ctx).Before(at) {
		return d.whose(ctx, in, others, r.Approve), true, nil
	}
	// The twin's latest words asked about no approval. If a task has just
	// asked the owner something, this is its answer.
	if d.taskWaiting(key) != nil {
		return "", false, nil
	}
	// If something recent is waiting the owner may mean it: ask rather than
	// guess. Anything older is not what a bare yes is about.
	var fresh []memory.Approval
	for _, ap := range d.pendingFor(ctx, key) {
		if recent(ap) && !c.isTheirs(ap.ID) && !d.forSomeoneElse(ctx, ap.ID) {
			fresh = append(fresh, ap)
		}
	}
	switch len(fresh) {
	case 0:
		c.closeAsk()
		return "", false, nil
	case 1:
		return d.confirm(c, in, fresh[0], r.Approve), true, nil
	}
	return d.whichOne(c, in, fresh, r.Approve), true, nil
}

// spoken reports whether the reply will be read aloud.
func spoken(in channels.Inbound) bool { return in.Channel == "voice" }

func yesNo(approve bool) string {
	if approve {
		return "yes"
	}
	return "no"
}

// confirm checks which approval a bare yes or no was for, showing it as
// stored; the same answer again then decides it.
func (d *Daemon) confirm(c *conversation, in channels.Inbound, ap memory.Approval, approve bool) string {
	if reply, refused := d.refuseAloud(context.Background(), in, ap, approve); refused { // voicehooks.go
		return reply
	}
	c.willAsk(ap.ID)
	c.willLean(approve)
	word, next := yesNo(approve), "go ahead"
	if !approve {
		next = "drop it"
	}
	if spoken(in) {
		return fmt.Sprintf("Just to be sure: %s to number %d, %s? Say %s to %s, or tell me what you meant.", word, ap.ID, label(ap), word, next)
	}
	if detail, ok := detailed(ap); ok {
		// A command, a script or a file: shown whole, as stored, since the
		// same answer again decides it.
		return fmt.Sprintf("Just to be sure: %s to #%d?\n%s\nReply \"%s %d\" and I'll %s, or tell me what you meant.", word, ap.ID, detail, word, ap.ID, next)
	}
	return fmt.Sprintf("Just to be sure: %s to #%d, %s? Reply \"%s %d\" and I'll %s, or tell me what you meant.", word, ap.ID, label(ap), word, ap.ID, next)
}

// detailed returns an approval's summary when it has more than a first line
// (a tool that writes its own, showing a command, a script or a file), which
// label would cut to its heading.
func detailed(ap memory.Approval) (string, bool) {
	s := strings.TrimSpace(ap.Summary)
	return s, strings.Contains(s, "\n")
}

// whose answers a bare yes or no when the twin's latest message asked only
// about requests made by someone else: those need the number.
func (d *Daemon) whose(ctx context.Context, in channels.Inbound, aps []memory.Approval, approve bool) string {
	word := yesNo(approve)
	var b strings.Builder
	for i, ap := range aps {
		who, _ := d.agent.ApprovalRequester(ctx, ap.ID)
		if i > 0 {
			b.WriteString(" ")
		}
		if spoken(in) {
			fmt.Fprintf(&b, "Number %d is %s's request, so I need the number: say %s %d.", ap.ID, who, word, ap.ID)
			continue
		}
		fmt.Fprintf(&b, "#%d is %q's request, so I need the number: reply \"%s %d\".", ap.ID, who, word, ap.ID)
	}
	return b.String()
}

// whichOne asks the owner to pick between approvals.
func (d *Daemon) whichOne(c *conversation, in channels.Inbound, aps []memory.Approval, approve bool) string {
	slices.SortFunc(aps, func(a, b memory.Approval) int { return cmp.Compare(a.ID, b.ID) })
	ids := make([]int64, len(aps))
	for i, ap := range aps {
		ids[i] = ap.ID
	}
	c.willAsk(ids...)
	c.willLean(approve) // "the landlord one" then answers with this yes or no
	word := yesNo(approve)
	if spoken(in) {
		parts := make([]string, len(aps))
		for i, ap := range aps {
			parts[i] = fmt.Sprintf("number %d, %s", ap.ID, label(ap))
		}
		parts[0] = "N" + parts[0][1:]
		last := len(parts) - 1
		return fmt.Sprintf("Which one? %s; or %s. Say %s and the number.", strings.Join(parts[:last], "; "), parts[last], word)
	}
	var b strings.Builder
	b.WriteString("Which one?")
	for _, ap := range aps {
		if detail, ok := detailed(ap); ok {
			fmt.Fprintf(&b, "\n#%d %s", ap.ID, detail) // a command or script is shown whole
			continue
		}
		fmt.Fprintf(&b, "\n#%d %s", ap.ID, label(ap))
	}
	if len(aps) == 2 {
		fmt.Fprintf(&b, "\nReply \"%s %d\" or \"%s %d\".", word, aps[0].ID, word, aps[1].ID)
	} else {
		fmt.Fprintf(&b, "\nReply with the number, like \"%s %d\".", word, aps[0].ID)
	}
	return b.String()
}

// label names an approval in a sentence: its summary's first line, or for a
// bare "tool(arg=value)" summary the tool and its plain arguments ("send: to
// boss"), leaving out lists and objects a person can't read at a glance.
func label(ap memory.Approval) string {
	s := strings.TrimSpace(ap.Summary)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if !bareSummary(ap) {
		return brief(s)
	}
	var in map[string]any
	_ = json.Unmarshal(ap.Input, &in)
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []string
	for _, k := range keys {
		var v string
		switch x := in[k].(type) {
		case string:
			v = strings.Join(strings.Fields(x), " ")
		case float64, bool:
			v = fmt.Sprint(x)
		}
		if v == "" {
			continue
		}
		if r := []rune(v); len(r) > 30 {
			v = string(r[:30]) + "…"
		}
		args = append(args, strings.ReplaceAll(k, "_", " ")+" "+v)
		if len(args) == 3 {
			break
		}
	}
	name := strings.ReplaceAll(ap.Tool, "_", " ")
	if len(args) == 0 {
		return name
	}
	return brief(name + ": " + strings.Join(args, ", "))
}

// bareSummary reports whether an approval's summary is only its call,
// "tool" or "tool(arg=value, …)", rather than words the tool wrote.
func bareSummary(ap memory.Approval) bool {
	s := strings.TrimSpace(ap.Summary) // whole: an argument may run over lines
	return s == ap.Tool || strings.HasPrefix(s, ap.Tool+"(") && strings.HasSuffix(s, ")")
}

func brief(s string) string {
	if len(s) <= 90 {
		return s
	}
	cut := 90
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// decideInChat carries out the owner's decision from the chat whose turn the
// caller holds, and returns what to tell them.
func (d *Daemon) decideInChat(ctx context.Context, in channels.Inbound, ap *memory.Approval, approve bool, onDelta func(string)) (string, error) {
	if reply, refused := d.refuseAloud(ctx, in, *ap, approve); refused { // voicehooks.go
		return reply, nil
	}
	key := in.Key()
	reply, err := d.resolve(ctx, key, ap, approve, onDelta)
	if err != nil {
		var np notPending
		if errors.As(err, &np) {
			return sentence(np.msg), nil
		}
		return "", err
	}
	if ap.ChatKey != key {
		// It ran in another conversation: a background run's, or a home
		// answered from here. Keep this one's history whole, let this reply
		// ask about anything that run raised next, and tell the home.
		d.adopt(ap.ChatKey, key)
		for _, m := range []llm.Message{llm.Text(llm.RoleUser, in.Text), llm.Text(llm.RoleAssistant, reply)} {
			// Kept even if a stop cut this turn short as the action ended.
			if err := d.store.AppendMessage(context.WithoutCancel(ctx), key, m); err != nil {
				d.log.Warn("approval: record", "chat", key, "err", err)
			}
		}
		d.tellHome(ctx, ap, reply, key)
	}
	return reply, nil
}

// DecideApproval approves or denies from the presence screen or orb and tells
// the chat it came from.
func (d *Daemon) DecideApproval(ctx context.Context, id int64, approve bool) (string, error) {
	return d.DecideApprovalBy(ctx, id, approve, screenDecider(ctx)) // devices.go: the paired device, if one
}

// Decider is who decided an approval, and how. It is kept with the
// approval, written to the audit log ("approval.granted #12 by Akshay's
// iPhone [d1] (passkey, relay r2, 203.0.113.9)") and passed to OnApproval.
type Decider struct {
	DeviceID   string
	DeviceName string // "Akshay's iPhone"; the owner when empty
	Method     string // screen, passkey, channel, cli, voice, reply
	Via        string // the channel it came over, or the relay a device used
	IP         string
}

// String is how the decider is kept and audited: "Akshay's iPhone [d1]
// (passkey, relay r2, 203.0.113.9)". The device's id is kept too, so a
// decision can later be tied to the device that made it.
func (b Decider) String() string {
	name := b.DeviceName
	if name == "" {
		name = "the owner"
	}
	if b.DeviceID != "" {
		name += " [" + b.DeviceID + "]"
	}
	var how []string
	for _, s := range []string{b.Method, b.Via, b.IP} {
		if s != "" {
			how = append(how, s)
		}
	}
	if len(how) == 0 {
		return name
	}
	return name + " (" + strings.Join(how, ", ") + ")"
}

type deciderKey struct{}

// withDecider notes on ctx who is deciding what the request decides.
func withDecider(ctx context.Context, b Decider) context.Context {
	return context.WithValue(ctx, deciderKey{}, b)
}

func deciderFrom(ctx context.Context) Decider {
	b, _ := ctx.Value(deciderKey{}).(Decider)
	return b
}

// chatDecider is the owner deciding from a conversation.
func chatDecider(in channels.Inbound) Decider {
	if channels.IsMessaging(in.Key()) {
		return Decider{Method: "channel", Via: in.Channel}
	}
	return Decider{Method: in.Channel}
}

// DecideApprovalBy is DecideApproval by a particular device or person (by):
// the approval, the audit log and OnApproval all say who decided.
func (d *Daemon) DecideApprovalBy(ctx context.Context, id int64, approve bool, by Decider) (string, error) {
	ap, err := d.store.GetApproval(ctx, id)
	if err != nil {
		return "", fmt.Errorf("there is no approval #%d", id)
	}
	p := d.bus.Begin("thinking")
	defer p.End()
	reply, err := d.decideElsewhere(withDecider(ctx, by), ap, approve)
	if err != nil {
		return "", err
	}
	d.bus.Publish(events.Event{Kind: "said", Text: reply})
	return reply, nil
}

// decideElsewhere carries out a decision made outside the approval's own
// conversation, whose turn nobody here holds, and tells that chat how it
// went. The screen's DecideApproval shows it on the orb; the owner's chat
// (strangerDecision) is already mid-turn, and its own reply says it.
func (d *Daemon) decideElsewhere(ctx context.Context, ap *memory.Approval, approve bool) (string, error) {
	reply, err := d.resolve(ctx, "", ap, approve, nil)
	if err != nil {
		return "", err
	}
	d.tellHome(ctx, ap, reply, "")
	return reply, nil
}

// tellHome lets the chat an approval came from hear how it went, when it was
// decided somewhere else (from; "" for the screen's cards). Only a live
// channel is told, and only that chat: the screen and API clients have their
// answer already, nothing goes to the owner's phone in their place (so it
// never falls back the way Send does), and nothing about a call is read out
// in the room. A task's step is reported by its task.
func (d *Daemon) tellHome(ctx context.Context, ap *memory.Approval, reply, from string) {
	if _, ok := d.tasks.ByKey(ap.ChatKey); ok {
		return
	}
	home := homeKey(ap.ChatKey)
	if home == from {
		return
	}
	c := d.conv(home)
	name, chatID := channels.SplitKey(home)
	ch, live := d.channel(name)
	if name == "" || !live || isCall(home) {
		c.takeRaised(ap.ChatKey) // shown where it was decided, not asked here
		return
	}
	c.noteSpoke(ap.ChatKey, reply, true)
	ctx = d.base(ctx)
	if ap.ChatKey != home {
		d.record(ctx, home, llm.Text(llm.RoleAssistant, reply))
	}
	if err := d.deliver(ctx, ch, home, chatID, reply); err != nil {
		d.log.Warn("approval: send", "chat", home, "err", err)
	}
}

// notPending is the error for deciding an approval that already has an outcome.
type notPending struct{ msg string }

func (e notPending) Error() string { return e.msg }

// resolve decides an approval. held is the conversation whose turn the
// caller already holds ("" for none). A task's step goes back through its
// task; anything else runs here, detached from the caller, so closing the
// page or losing the connection can't cut off an action already approved.
func (d *Daemon) resolve(ctx context.Context, held string, ap *memory.Approval, approve bool, onDelta func(string)) (string, error) {
	by := deciderFrom(ctx)
	if ap.Status == "pending" && d.lapsed(*ap) {
		d.lapse(d.base(ctx), ap) // waited too long: the world it was asked about has moved on
	}
	if ap.Status != "pending" {
		return "", notPending{statusLine(ap)}
	}
	if t, ok := d.tasks.ByKey(ap.ChatKey); ok {
		return d.resolveTaskStep(ctx, t, ap, approve, by)
	}
	turn, _ := ctx.Value(turnRunKey{}).(*running) // the owner's turn deciding it, if any
	// The turn first: the time limit is for the action, not for waiting on
	// a turn already running there.
	c, release := d.acquire(ap.ChatKey) // a background run's scratch one is let go after
	defer release()
	if ap.ChatKey != held {
		approvalTurnWait()
		c.begin()
		defer d.end(ap.ChatKey, c)
	}
	ctx, cancel := approvalClock(d.base(ctx))
	defer cancel()
	dec, err := d.claim(ctx, ap, approve, by)
	if err != nil {
		return "", err
	}
	// Detached from whoever approved it, but not from the owner's "stop":
	// said where it runs, in its home chat (a background run's action is
	// the home's), or in the chat whose turn decided it.
	where := []*conversation{c, d.conv(homeKey(ap.ChatKey))}
	if held != "" {
		where = append(where, d.conv(held))
	}
	if turn != nil {
		where = append(where, turn.where...)
	}
	ctx, run := track(ctx, fmt.Sprintf("carrying out #%d (%s)", dec.ID, label(*dec)), false, true, ap.ChatKey, where...)
	if turn != nil {
		turn.carrying.Store(run)
	}
	reply, err := d.agent.Carry(ctx, dec, onDelta)
	cut := ctx.Err() != nil
	if untrack(run) && cut {
		d.markStopped(context.WithoutCancel(ctx), ap.ChatKey)
		reply, err = stoppedLine(ap.ChatKey, run), nil
	}
	d.carried(dec)
	return reply, err
}

// claim records the owner's decision. Of two decisions for one approval
// (a double click, the orb and the screen, a repeated "yes 4") only the
// first gets through; the other learns how it was decided.
func (d *Daemon) claim(ctx context.Context, ap *memory.Approval, approve bool, by Decider) (*memory.Approval, error) {
	dec, err := d.agent.DecideBy(ctx, ap.ChatKey, ap.ID, approve, by.String()) // screens and the orb hear through OnApproval
	if err != nil {
		if cur, gerr := d.store.GetApproval(ctx, ap.ID); gerr == nil && cur.Status != "pending" {
			return nil, notPending{statusLine(cur)}
		}
		return nil, err
	}
	return dec, nil
}

// carried tidies up once a decided approval has been acted on.
func (d *Daemon) carried(ap *memory.Approval) {
	d.forgetPage(ap)
}

// resolveTaskStep hands an approval raised inside a background task back to
// the task manager, which runs it, keeps the board current and tells the
// owner how it went. The decision is claimed first, so answering twice
// (while the task waits for a free slot, say) can't start a second leg.
func (d *Daemon) resolveTaskStep(ctx context.Context, t *tasks.Task, ap *memory.Approval, approve bool, by Decider) (string, error) {
	cur := tasks.Task{Title: "That task", Status: tasks.Cancelled}
	for _, x := range d.tasks.List() {
		if x.Key == t.Key {
			cur = x
		}
	}
	ctx = d.base(ctx)
	if !cur.Open() {
		if err := d.agent.Settle(ctx, ap, "expired", fmt.Sprintf("its task %s", cur.Status)); err != nil {
			if now, gerr := d.store.GetApproval(ctx, ap.ID); gerr == nil && now.Status != "pending" {
				return "", notPending{statusLine(now)}
			}
			return "", err
		}
		return "", notPending{closedTask(cur, approve)}
	}
	dec, err := d.claim(ctx, ap, approve, by)
	if err != nil {
		return "", err
	}
	d.tasks.ResumeOr(ctx, t, func(c context.Context) (string, error) {
		reply, err := d.agent.Carry(c, dec, nil)
		d.carried(dec)
		return reply, err
	}, func() {
		// The task stopped while the step waited for its turn: nothing ran,
		// so it mustn't read as approved.
		d.agent.Revise(ctx, dec, "approved", "expired", "its task stopped before it could run")
		d.forgetPage(dec)
	})
	if approve {
		return "Going ahead with that for " + cur.Title + ".", nil
	}
	return "Understood; " + cur.Title + " will carry on without it.", nil
}

// closedTask says why a closed task's step won't be carried out.
func closedTask(t tasks.Task, approve bool) string {
	var state string
	switch t.Status {
	case tasks.Done:
		state = "has already finished"
	case tasks.Failed:
		state = "stopped after a problem"
	case tasks.Cancelled:
		state = "was cancelled"
	default:
		state = "isn't running any more"
	}
	if approve {
		return fmt.Sprintf("%s %s, so I haven't gone ahead with that; ask me again if you still want it", t.Title, state)
	}
	return fmt.Sprintf("%s %s, so it won't happen anyway", t.Title, state)
}

// base is the context approved work runs under: the daemon's own, so it ends
// at shutdown but not when a page closes or a phone locks.
func (d *Daemon) base(ctx context.Context) context.Context {
	if d.runCtx != nil {
		return d.runCtx
	}
	return context.WithoutCancel(ctx)
}

// statusLine says why an approval can't be decided again.
func statusLine(ap *memory.Approval) string {
	switch ap.Status {
	case "approved":
		return fmt.Sprintf("approval #%d was already approved", ap.ID)
	case "denied":
		return fmt.Sprintf("approval #%d was already turned down", ap.ID)
	case "superseded":
		return fmt.Sprintf("#%d was replaced by a newer request; say /pending to see what's waiting", ap.ID)
	case "expired":
		return fmt.Sprintf("#%d has lapsed; ask me again if you still want it", ap.ID)
	}
	return fmt.Sprintf("approval #%d is %s", ap.ID, ap.Status)
}

// sentence turns an error message into something to say.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	s = string(r)
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// pendingFor lists the approvals waiting that the owner can answer from a
// conversation: its own and its background runs', and those it stands in for.
func (d *Daemon) pendingFor(ctx context.Context, key string) []memory.Approval {
	all, err := d.store.AllPendingApprovals(ctx)
	if err != nil {
		return nil
	}
	var out []memory.Approval
	for _, ap := range all {
		if d.answerable(key, ap) && !d.lapsed(ap) {
			out = append(out, ap)
		}
	}
	return out
}

// noticed records a message the twin sent on its own in the conversation
// it was delivered to (where, from route), after any turn running there, so
// a reply there has context and a "yes" there answers what it asked. record
// is what the history keeps of text.
func (d *Daemon) noticed(ctx context.Context, where, text, record string) {
	d.conv(homeKey(where)).noteSpoke("", text, true)
	d.conv(homeKey(where)).spokeOwn()
	d.record(ctx, where, llm.Text(llm.RoleAssistant, record))
	d.noteNotice(ctx, where, record) // notice_replies.go: what "stop sending these" would answer
}

// approvalNews is the data of an "approval" event: one was raised or decided.
type approvalNews struct {
	ID      int64  `json:"id"`
	Status  string `json:"status"` // pending, approved, denied, superseded, expired
	Tool    string `json:"tool,omitempty"`
	Summary string `json:"summary,omitempty"`
	Chat    string `json:"chat,omitempty"`
	Risk    string `json:"risk,omitempty"` // read, write or dangerous, as the owner is asked
	By      string `json:"by,omitempty"`   // who decided, and how
	Why     string `json:"why,omitempty"`  // why it was replaced, or not done
}

// publishApproval tells screens and the orb about a step in an approval's
// life; why is the twin's reason when it settled it itself.
func (d *Daemon) publishApproval(ap *memory.Approval, status, why string) {
	text := fmt.Sprintf("#%d %s", ap.ID, status)
	switch status {
	case "pending":
		text = fmt.Sprintf("#%d needs you: %s", ap.ID, ap.Summary)
	case "expired":
		if why == "" {
			why = "things had changed"
		}
		text = fmt.Sprintf("#%d not done: %s", ap.ID, why)
	}
	d.bus.Publish(events.Event{Kind: "approval", Text: text, Data: approvalNews{ID: ap.ID, Status: status, Tool: ap.Tool, Summary: ap.Summary, Chat: ap.ChatKey, Risk: ap.Risk.String(), By: ap.DecidedBy, Why: why}})
}

// wireApprovals connects the agent's approval hooks to the daemon, and gives
// the model the tools that settle what the owner says in their own words.
func (d *Daemon) wireApprovals() {
	d.agent.OnApproval(d.approvalEvent)
	d.agent.CheckApproved = d.checkApproved
	d.agent.Performed = d.performed // react.go: a nod once an approved step has worked
	d.agent.Tools().Register(d.answerTools()...)
}

// approvalEvent hears every step of every approval, straight from where it
// happens (agent.OnApproval): screens and the orb hear through the bus, and
// a request that is settled drops its page note.
func (d *Daemon) approvalEvent(_ context.Context, e agent.ApprovalEvent) {
	ap := memory.Approval{ID: e.ID, ChatKey: e.ChatKey, Tool: e.Tool, Input: e.Input, Summary: e.Summary, Status: e.Status,
		CreatedAt: e.CreatedAt, Risk: e.Risk, InputHash: e.InputHash, DecidedBy: e.By}
	switch e.Status {
	case "pending":
		d.approvalRaised(ap)
		return
	case "denied", "superseded", "expired":
		d.forgetPage(&ap)
		d.briefingSettled(e) // briefing_offer.go: a no to the first hello's offer
	}
	d.publishApproval(&ap, e.Status, e.Why)
}

// approvalRaised runs as soon as the agent queues an approval: an older
// request for the same call is replaced (so one yes covers it), the page a
// browser action is aimed at is noted, and screens and the orb hear at once.
func (d *Daemon) approvalRaised(ap memory.Approval) {
	ctx := context.Background()
	if olds, err := d.store.AllPendingApprovals(ctx); err == nil {
		who, _ := d.agent.ApprovalRequester(ctx, ap.ID) // "" for the owner
		for _, old := range olds {
			if old.ID == ap.ID || old.Tool != ap.Tool || !sameJSON(old.Input, ap.Input) || !sameAsker(old.ChatKey, ap.ChatKey) {
				continue
			}
			// Only the same person's request is replaced: in a shared chat the
			// owner's and a stranger's identical requests are two requests.
			if was, _ := d.agent.ApprovalRequester(ctx, old.ID); was != who {
				continue
			}
			_ = d.agent.Settle(ctx, &old, "superseded", fmt.Sprintf("replaced by #%d", ap.ID))
		}
	}
	d.notePage(ctx, ap)
	d.conv(homeKey(ap.ChatKey)).noteRaised(ap.ChatKey, ap.ID)
	d.publishApproval(&ap, "pending", "")
	d.raisedFor(ctx, ap) // react.go: a glance towards Needs
	if isCall(ap.ChatKey) {
		go d.callAsked(ap) // not on the call's turn: the line is waiting for an answer
	}
}

// sameAsker reports whether two requests come from the same asker, so the
// later replaces the earlier: the same conversation, or two routine runs
// (protocols, the watcher) from the same home, whose identical requests
// would otherwise both wait and, both approved, do the thing twice. A
// background task is its own asker: another task's request never replaces
// one it is waiting on.
func sameAsker(a, b string) bool {
	if a == b {
		return true
	}
	if homeKey(a) != homeKey(b) || inTaskRun(a) || inTaskRun(b) {
		return false
	}
	return a != homeKey(a) && b != homeKey(b)
}

// inTaskRun reports whether key is a background task's conversation, or a run inside one.
func inTaskRun(key string) bool { return reTaskRun.MatchString(key) }

var reTaskRun = regexp.MustCompile(`[^:]#task-\d+(?:#|$)`) // not an IRC room called "#task-…"

func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// pageKey is where the page a browser approval was asked on is kept.
func pageKey(id int64) string { return "approval.page." + strconv.FormatInt(id, 10) }

// actsOnCurrentPage reports whether an approved call works on whatever page
// the shared browser is showing: its refs and selectors only mean something
// there. A call that opens its own URL first is not tied to it.
func actsOnCurrentPage(ap memory.Approval) bool {
	if ap.Tool != "browser_act" {
		return false
	}
	var in struct{ URL string }
	_ = json.Unmarshal(ap.Input, &in)
	return strings.TrimSpace(in.URL) == ""
}

// notePage records the page a browser action was asked on.
func (d *Daemon) notePage(ctx context.Context, ap memory.Approval) {
	if d.pageURL == nil || !actsOnCurrentPage(ap) {
		return
	}
	if u := d.pageURL(); u != "" {
		_ = d.store.Set(ctx, pageKey(ap.ID), u)
	}
}

// forgetPage drops the page note once an approval is settled.
func (d *Daemon) forgetPage(ap *memory.Approval) {
	if actsOnCurrentPage(*ap) {
		_ = d.store.Unset(context.Background(), pageKey(ap.ID))
	}
}

// checkApproved runs just before an approved call: a browser action goes
// ahead only on the page it was asked on. The browser is shared by every
// chat, task and protocol, so by the time the owner says yes it may be
// somewhere else, and "click 12" would press something they never saw.
func (d *Daemon) checkApproved(ctx context.Context, ap memory.Approval) error {
	if d.pageURL == nil || !actsOnCurrentPage(ap) {
		return nil
	}
	want, _ := d.store.Get(ctx, pageKey(ap.ID))
	if want == "" {
		return d.checkElements(ctx, ap)
	}
	now := d.pageURL()
	if samePage(want, now) {
		return d.checkElements(ctx, ap)
	}
	if now == "" {
		return fmt.Errorf("the browser was closed after this was asked, so nothing was clicked or typed; it was on %s", want)
	}
	return fmt.Errorf("the browser has moved on since this was asked (it was on %s, now %s), so nothing was clicked or typed", want, now)
}

// samePage compares page addresses, ignoring the #fragment.
func samePage(a, b string) bool {
	cut := func(s string) string {
		if i := strings.IndexByte(s, '#'); i >= 0 {
			s = s[:i]
		}
		return strings.TrimSuffix(s, "/")
	}
	return a != "" && b != "" && cut(a) == cut(b)
}
