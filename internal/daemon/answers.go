package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/skills/reminders"
	"github.com/MavrkAI/Mirrin/internal/tasks"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// What the owner says in their own words, rather than "yes 12", still gets
// where it is meant to go:
//
//   - resolve_approval: "the landlord one, go ahead" after the twin asked
//     which. The model names the approval and the decision; the daemon holds
//     it to the owner's literal message (a yes or no, not a question, about
//     what the twin had just asked, from a chat that may answer it; after
//     "which one?", a few words that plainly pick that one), and a request
//     that can't easily be undone still needs "yes N".
//   - answer_task: the answer to a background task's question, from any of
//     the owner's chats, however long after it asked. The task gets the
//     owner's own words; the model's reading goes along only as a marked,
//     capped hint.
//   - A task's question is answered by the owner's next message in the chat
//     it reached them in, not only the one that started the task.

// answerTools are the tools that pass on what the owner says in their own
// words, the one that takes back an "always", and the one that changes how
// the twin addresses them.
func (d *Daemon) answerTools() []tools.Tool {
	return []tools.Tool{
		ownersOnly{tools.New("resolve_approval",
			"Settle a request you asked the user to approve when their latest message answers it in their own words instead of \"yes N\" or \"no N\" (\"the landlord one, go ahead\", \"yep, send it\", \"no, not that one\"). Only for what you just asked them about. Their exact words are checked, so call it only when they clearly said yes or no; if they asked something or were unclear, ask them instead. Something that can't easily be undone (a payment, a command) still needs their \"yes N\".",
			tools.Schema(map[string]tools.Prop{
				"id":       {Type: "integer", Description: "The approval's number", Required: true},
				"decision": {Type: "string", Description: "What the user said", Enum: []string{"approve", "deny"}, Required: true},
			}), tools.RiskRead, d.resolveApprovalTool), d},
		ownersOnly{tools.New("answer_task",
			"Pass the user's answer to a background task that asked them a question (list_tasks shows what each is waiting for), when their latest message answers it: from any of their chats, however long ago the task asked. Their exact words go to the task.",
			tools.Schema(map[string]tools.Prop{
				"id":     {Type: "string", Description: "The task's id", Required: true},
				"answer": {Type: "string", Description: "Their answer as you understand it, if their message says more than that"},
			}), tools.RiskRead, d.answerTaskTool), d},
		ownersOnly{d.askBeforeTool(), d},
		ownersOnly{d.setAddressTool(), d}, // address.go
	}
}

// ownersOnly is a tool that acts on the owner's own words. In anyone else's
// turn it is refused before anyone could be asked to approve it.
type ownersOnly struct {
	*tools.Func
	d *Daemon
}

func (o ownersOnly) Check(ctx context.Context, call tools.Call) error {
	if o.d.ownerWords(ctx, call.ChatKey) == "" {
		return errors.New("only the owner can settle this, in their own words")
	}
	return nil
}

// ownerWords is the owner's own latest message in a conversation: what the
// turn running there answers. It is "" when that turn didn't start with the
// owner's own words (a message from someone else, a system-framed run) or
// the conversation is a background run.
func (d *Daemon) ownerWords(ctx context.Context, key string) string {
	if memory.IsScratch(key) {
		return ""
	}
	h, err := d.store.History(ctx, key, 80)
	if err != nil {
		return ""
	}
	for i := len(h) - 1; i >= 0; i-- {
		m := h[i]
		if m.Role != llm.RoleUser || slices.ContainsFunc(m.Blocks, func(b llm.Block) bool { return b.Type == llm.BlockToolResult }) {
			continue
		}
		t := strings.TrimSpace(m.PlainText())
		if strings.HasPrefix(t, "[") { // someone else's message, or the system's
			return ""
		}
		return t
	}
	return ""
}

func (d *Daemon) resolveApprovalTool(ctx context.Context, call tools.Call) (string, error) {
	var in struct {
		ID       int64  `json:"id"`
		Decision string `json:"decision"`
	}
	if err := tools.Decode(call, &in); err != nil {
		return "", err
	}
	approve := strings.EqualFold(in.Decision, "approve")
	if !approve && !strings.EqualFold(in.Decision, "deny") {
		return "", errors.New(`decision must be "approve" or "deny"`)
	}
	if api.ApprovalsPaused(ctx) { // reachrelay.go: the certificate alarm holds remote decisions
		return "", errors.New(alarmPausedTool)
	}
	if api.PeerFrom(ctx).Lacks(devices.Approve) { // devices.go deviceCantApprove, for words it can't parse
		return "", errors.New("the user is on a device paired to talk, not to answer requests, so their words can't settle it; they can answer from a device that may approve, or in their own chat app")
	}
	key := call.ChatKey
	words := d.ownerWords(ctx, key)
	if words == "" {
		return "", errors.New("the latest message here isn't the user's own words, so it can't settle anything; ask them")
	}
	ap, err := d.store.GetApproval(ctx, in.ID)
	if err != nil {
		return "", fmt.Errorf("there is no approval #%d", in.ID)
	}
	if ap.Status == "pending" && d.lapsed(*ap) {
		d.lapse(d.base(ctx), ap)
	}
	if ap.Status != "pending" {
		return "", errors.New(sentence(statusLine(ap)))
	}
	ask := fmt.Sprintf("ask them to reply \"yes %d\" or \"no %d\"", ap.ID, ap.ID)
	if approve && channels.IsVoiceNote(words) {
		// A transcript is only a guess at what was said (media.go): it
		// never approves anything. A no may still go through.
		return "", fmt.Errorf("their answer came as a voice note, and a transcript is only a guess at what was said, so it can't approve #%d; %s by typing it", ap.ID, ask)
	}
	if d.forSomeoneElse(ctx, ap.ID) {
		return "", fmt.Errorf("#%d is someone else's request: only the user's own \"yes %d\" or \"no %d\" decides it", ap.ID, ap.ID, ap.ID)
	}
	if !d.answerable(key, *ap) {
		return "", fmt.Errorf("#%d was asked in another conversation, so it is answered there or on the presence screen", ap.ID)
	}
	c := d.conv(key)
	asked, at, lean := c.priorAsk()
	if !slices.Contains(asked, ap.ID) || clock().Sub(at) > askFresh || arrivedAt(ctx).Before(at) {
		return "", fmt.Errorf("#%d isn't what you had just asked them about, so their words can't settle it; %s", ap.ID, ask)
	}
	if strings.Contains(words, "?") {
		return "", fmt.Errorf("their message asks something rather than answering; answer them, then %s", ask)
	}
	said, clear := d.leaning(words)
	if !clear && lean != 0 {
		// After a yes and "which one?", a reply that only picks one carries
		// that yes ("the landlord one"). Only a reply that plainly picks
		// this one: not a hesitation ("hold on, let me think"), nor
		// anything that could as well be about another.
		if !d.picks(ctx, words, *ap, asked) {
			return "", fmt.Errorf("their message doesn't clearly pick #%d; %s", ap.ID, ask)
		}
		said, clear = lean > 0, true
	}
	switch {
	case !clear:
		return "", fmt.Errorf("their message doesn't clearly say yes or no to #%d; %s", ap.ID, ask)
	case said != approve:
		return "", fmt.Errorf("their message reads as a %s, not a %s; %s", yesNo(said), yesNo(approve), ask)
	}
	for _, n := range d.namedRequests(ctx, words) {
		if n != ap.ID {
			return "", fmt.Errorf("they named #%d, not #%d; %s", n, ap.ID, ask)
		}
	}
	if reply, ok := d.refuseRemoteYes(ctx, *ap, approve); ok { // stepup.go: as "yes N" from that device
		return "", errors.New(reply)
	}
	if approve && d.dangerous(ctx, *ap) {
		return "", fmt.Errorf("#%d can't easily be undone, so it needs their own \"yes %d\"; ask them for it", ap.ID, ap.ID)
	}
	by := peerDecider(ctx, Decider{Method: "reply", Via: channelOf(key)}) // devices.go: the paired device, if one
	if t, ok := d.tasks.ByKey(ap.ChatKey); ok {
		reply, err := d.resolveTaskStep(ctx, t, ap, approve, by)
		return d.settledText(reply, err)
	}
	if ap.ChatKey == key {
		// Asked in this very conversation: decided and done here, in this turn.
		dec, err := d.claim(ctx, ap, approve, by)
		if err != nil {
			return d.settledText("", err)
		}
		out := d.agent.Perform(ctx, dec)
		d.carried(dec)
		return out, nil
	}
	// A background run's, or a home answered from here: carried out there,
	// as "yes N" from this chat would be.
	reply, err := d.resolve(withDecider(ctx, by), key, ap, approve, nil)
	if err != nil {
		return d.settledText("", err)
	}
	d.adopt(ap.ChatKey, key)
	d.tellHome(ctx, ap, reply, key)
	return "Settled. Where it was asked, that led to: " + reply + "\nTell the user in a line.", nil
}

// settledText is what the model hears after settling a request.
func (d *Daemon) settledText(reply string, err error) (string, error) {
	var np notPending
	if errors.As(err, &np) {
		return "", errors.New(sentence(np.msg))
	}
	if err != nil {
		return "", err
	}
	return reply + "\nTell the user in a line.", nil
}

// leaning reads whether the owner's words say yes or no: clear only when
// they hold a yes and no no ("the landlord one, go ahead"), or the reverse.
func (d *Daemon) leaning(text string) (approve, clear bool) {
	names := d.twinNames()
	words := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",.;:!?()\"“”-—–", r)
	})
	yes, no := false, false
	for i := 0; i < len(words); {
		n := 0
		for try := min(3, len(words)-i); try > 0; try-- {
			if r, ok := approvals.ParseReply(strings.Join(words[i:i+try], " "), names...); ok {
				if r.Approve {
					yes = true
				} else {
					no = true
				}
				n = try
				break
			}
		}
		i += max(n, 1)
	}
	return yes, yes != no
}

// picks reports whether the owner's words, which say neither yes nor no,
// single out ap among the requests the twin had asked about: a few words,
// no hesitation, and a word of ap's own ("the boss one" for "send: to
// boss") that none of the others share, or ap's number.
func (d *Daemon) picks(ctx context.Context, words string, ap memory.Approval, asked []int64) bool {
	said := plainWords(words)
	if len(said) == 0 || len(said) > 6 {
		return false
	}
	for _, w := range said {
		if hesitant[w] {
			return false
		}
	}
	if slices.Contains(d.namedRequests(ctx, words), ap.ID) {
		return true
	}
	own := requestWords(ap)
	for _, id := range asked {
		if id == ap.ID {
			continue
		}
		if other, err := d.store.GetApproval(ctx, id); err == nil {
			for w := range requestWords(*other) {
				delete(own, w)
			}
		}
	}
	for _, w := range said {
		for o := range own {
			if w == o || len(o) >= 4 && strings.HasPrefix(w, o) { // "landlords", "boss's"
				return true
			}
		}
	}
	return false
}

// hesitant words mean the owner hasn't decided, or means none of them.
var hesitant = map[string]bool{
	"wait": true, "hold": true, "hang": true, "later": true, "think": true, "thinking": true, "neither": true,
	"none": true, "nor": true, "not": true, "maybe": true, "perhaps": true, "unsure": true, "hmm": true,
	"hm": true, "dunno": true, "idk": true, "either": true, "both": true, "all": true, "stop": true,
	"pause": true, "tomorrow": true, "sec": true, "moment": true, "minute": true,
}

// requestWords are the words that tell a request apart: from how it is
// shown ("send: to boss") and the values it holds, less the filler.
func requestWords(ap memory.Approval) map[string]bool {
	text := label(ap) + " " + ap.Summary
	var in map[string]any
	if json.Unmarshal(ap.Input, &in) == nil {
		for _, v := range in {
			if s, ok := v.(string); ok {
				text += " " + s
			}
		}
	}
	out := map[string]bool{}
	for _, w := range plainWords(text) {
		if len([]rune(w)) >= 3 && !filler[w] {
			out[w] = true
		}
	}
	return out
}

// filler words say nothing about which request is meant.
var filler = map[string]bool{
	"the": true, "one": true, "and": true, "for": true, "that": true, "this": true, "with": true,
	"from": true, "about": true, "please": true, "yes": true, "okay": true, "then": true, "just": true,
	"its": true, "his": true, "her": true, "their": true, "our": true, "your": true, "you": true,
}

var reNumber = regexp.MustCompile(`(?:^|[^\w.])#?(\d+)\b`)

// namedRequests lists the waiting requests the owner's words name by number.
func (d *Daemon) namedRequests(ctx context.Context, text string) []int64 {
	var out []int64
	for _, m := range reNumber.FindAllStringSubmatch(text, -1) {
		id, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || slices.Contains(out, id) {
			continue
		}
		if ap, err := d.store.GetApproval(ctx, id); err == nil && ap.Status == "pending" {
			out = append(out, id)
		}
	}
	return out
}

func (d *Daemon) answerTaskTool(ctx context.Context, call tools.Call) (string, error) {
	var in struct {
		ID     string `json:"id"`
		Answer string `json:"answer"`
	}
	if err := tools.Decode(call, &in); err != nil {
		return "", err
	}
	key := call.ChatKey
	words := d.ownerWords(ctx, key)
	if words == "" {
		return "", errors.New("the latest message here isn't the user's own words, so it can't answer a task; ask them")
	}
	t, ok := d.taskByID(in.ID)
	if !ok {
		return "", fmt.Errorf("there is no task %s; list_tasks shows them", in.ID)
	}
	if forgeable(channelOf(key)) && homeKey(t.Owner) != key {
		return "", fmt.Errorf("an answer by %s can't be checked for a task started elsewhere; the user can answer it from the chat it asked in", channelLabel(channelOf(key)))
	}
	if t.Status == tasks.WaitingUser && !t.AskedAt.IsZero() && arrivedAt(ctx).Before(t.AskedAt) {
		return "", fmt.Errorf("their latest message came before %s asked its question, so it can't be the answer; tell them what it asks: %q", t.Title, t.Question)
	}
	answer, run := words, d.base(ctx)
	switch {
	case channels.IsPhotoNote(words) && agent.HasPhotos(ctx):
		run = agent.CopyPhotos(run, ctx) // the picture goes to the task too
	case channels.IsPhotoNote(words):
		// The picture isn't on this turn: the task only gets the words, and
		// is told so rather than left to guess (media.go).
		answer = words + "\n" + photoForTask
	}
	if a := strings.TrimSpace(in.Answer); a != "" && !strings.EqualFold(a, words) {
		// The chat model's reading is only a hint: the owner's words decide.
		answer = fmt.Sprintf("%s\n(The chat model read this as: %q. Where that differs from their words, go by their words.)", answer, truncate(strings.Join(strings.Fields(a), " "), 200))
	}
	got, err := d.tasks.Answer(run, t.ID, answer)
	if err != nil {
		return "", err
	}
	d.store.Audit(ctx, "task.answered", key, fmt.Sprintf("%s %s", got.ID, truncate(words, 200)))
	return fmt.Sprintf("Passed their answer to %s (task %s); it carries on in the background and will report back. Tell the user in a few words.", got.Title, got.ID), nil
}

// taskByID finds a task as it stands.
func (d *Daemon) taskByID(id string) (tasks.Task, bool) {
	for _, t := range d.tasks.List() {
		if t.ID == strings.TrimSpace(id) {
			return t, true
		}
	}
	return tasks.Task{}, false
}

// taskWaiting is the one task whose question the owner's next message in
// key answers: asked in the last half hour, of key itself or of a chat key
// stands in for (standsIn).
func (d *Daemon) taskWaiting(key string) *tasks.Task {
	return d.tasks.WaitingFor(func(owner string) bool { return d.standsIn(key, owner) }, askFresh)
}

// standsIn reports whether the owner's messages in key answer for chat
// owner: its own, or where owner's messages reach the owner because its
// channel isn't running (a task started from the terminal asks on the
// phone) or couldn't deliver (forward.go). Not a channel whose sender can be
// forged, and not the presence screen, where typing is not an answer to
// whatever asked last.
func (d *Daemon) standsIn(key, owner string) bool {
	if owner == key {
		return true
	}
	if forgeable(channelOf(key)) || channelOf(key) == "screen" {
		return false
	}
	if d.reach(owner) == key {
		return true
	}
	return slices.ContainsFunc(d.fwd.from(key), func(k string) bool { return homeKey(k) == homeKey(owner) })
}

// answerWaitingTask passes the owner's message to the task that has just
// asked them something in this chat (or one it stands in for), if any.
func (d *Daemon) answerWaitingTask(ctx context.Context, in channels.Inbound) (string, bool) {
	if !in.IsOwner || agent.NotAnAnswer(in.Text) {
		return "", false // small talk or a question of their own: a normal turn, which can still answer_task
	}
	t := d.taskWaiting(in.Key())
	if t == nil {
		return "", false
	}
	// A photo with the answer goes to the task's run (media.go keeps it).
	got, err := d.tasks.Answer(agent.CopyPhotos(d.base(ctx), ctx), t.ID, strings.TrimSpace(in.Text))
	if err != nil {
		return "", false // answered a moment ago, from elsewhere: this is conversation
	}
	if spoken(in) {
		// Out loud, a person just says "Right." and gets on with it.
		return taskNods[taskNod.Add(1)%uint32(len(taskNods))], true
	}
	return "Thanks. Carrying on with " + got.Title + ".", true
}

// taskNods are said, in turn, when a spoken answer goes to a waiting task.
var taskNods = []string{"Right.", "On it.", "Got it.", "Sure."}

var taskNod atomic.Uint32

// A reminder that has just gone out is answered in a word, in the owner's
// own chat: "done" ticks it off ("Ticked."), "later" puts it back an hour,
// and "tonight", "tomorrow 9" or "in 20 minutes" put it back till then
// ("Back at 4:00 pm."). The third time the same one is put back, and only
// then, the twin offers to take it on or drop it. Anything else, and
// anything once the twin has said more since, is conversation.

// reminderReplyFor is how long after a reminder goes out a word in its chat
// is about it.
const reminderReplyFor = 30 * time.Minute

// tickWords tick a reminder off: the whole message, lowercased.
var tickWords = map[string]bool{"done": true, "did it": true, "done it": true, "all done": true, "sorted": true, "ticked": true}

// answerReminder ticks off or puts back the reminder that was the twin's
// last word in this chat, when the owner's whole message says done or when.
func (d *Daemon) answerReminder(ctx context.Context, in channels.Inbound) (string, bool) {
	key := in.Key()
	if !in.IsOwner || forgeable(in.Channel) || memory.IsScratch(key) || !d.ownersOwnChat(key) {
		return "", false // not someone else, nor a group the owner is in
	}
	said := strings.Join(strings.Fields(strings.TrimFunc(in.Text, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})), " ")
	words := strings.ToLower(said)
	if words == "" || len(words) > 40 {
		return "", false
	}
	now := clock()
	r, ok := d.store.LastFired(ctx, key, reminderReplyFor)
	if !ok {
		// Set somewhere else (by voice, on the screen) and sent here: the
		// conversation's last word says whether it was this one.
		r, ok = d.store.LastFired(ctx, "", reminderReplyFor)
	}
	if !ok || r.Kind != "remind" || now.Sub(r.FiredAt) > reminderReplyFor || !d.remindedLast(ctx, key, r) || d.openQuestion(key) {
		return "", false
	}
	var reply string
	if tickWords[words] {
		if err := d.store.MarkDone(ctx, r.ID); err != nil {
			return "", false
		}
		d.store.Audit(ctx, "reminder.done", key, fmt.Sprintf("#%d %s", r.ID, r.Text))
		reply = "Ticked."
	} else {
		due := now.Add(time.Hour)
		if words != "later" {
			t, err := reminders.ParseWhen(said, now, d.location())
			if err != nil || !t.After(now) {
				return "", false
			}
			due = t
		}
		if err := d.store.Snooze(ctx, r.ID, due); err != nil {
			return "", false
		}
		d.store.Audit(ctx, "reminder.snoozed", key, fmt.Sprintf("#%d %s, until %s", r.ID, r.Text, due.UTC().Format(time.RFC3339)))
		reply = "Back at " + backAt(due, now, d.location()) + "."
		if r.Snoozes+1 >= 3 && d.slippingOnce(ctx, r.ID) {
			reply += " This one keeps slipping: shall I take it on, or drop it?"
		}
	}
	// Kept in the conversation, so the model knows what was settled.
	for _, m := range []llm.Message{llm.Text(llm.RoleUser, in.Text), llm.Text(llm.RoleAssistant, reply)} {
		if err := d.store.AppendMessage(context.WithoutCancel(ctx), key, m); err != nil {
			d.log.Warn("reminder: record", "chat", key, "err", err)
		}
	}
	return reply, true
}

// remindedLast reports whether the twin's latest message in key is r going
// out on its own, as the heartbeat words it ("Reminder: …").
func (d *Daemon) remindedLast(ctx context.Context, key string, r memory.Reminder) bool {
	h, err := d.store.History(ctx, key, 8)
	if err != nil {
		return false
	}
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == llm.RoleAssistant {
			t := strings.TrimSpace(h[i].PlainText())
			return strings.HasPrefix(t, "Reminder") && strings.HasSuffix(t, strings.TrimSpace(r.Text))
		}
	}
	return false
}

// openQuestion reports whether the twin was waiting on the owner here when
// their message came: an approval it asked about, or a task's question.
func (d *Daemon) openQuestion(key string) bool {
	c := d.conv(key)
	prior, _, _ := c.priorAsk()
	asked, _, _ := c.lastAsk()
	return len(prior) > 0 || len(asked) > 0 || d.taskWaiting(key) != nil
}

// slippingOnce reports whether this is the first time the twin may offer to
// take on or drop a reminder that keeps being put back, and notes that it has.
func (d *Daemon) slippingOnce(ctx context.Context, id int64) bool {
	k := fmt.Sprintf("reminder.slipping.%d", id)
	if v, err := d.store.Get(ctx, k); err != nil || v != "" {
		return false
	}
	return d.store.Set(ctx, k, "1") == nil
}

// backAt says when a reminder put back is due: "4:00 pm" today, "9:00 am
// tomorrow", or "9:00 am on Mon 5 Oct" after that.
func backAt(due, now time.Time, loc *time.Location) string {
	due, now = due.In(loc), now.In(loc)
	at := due.Format("3:04 pm")
	y, m, dd := now.Date()
	tomorrow := time.Date(y, m, dd+1, 0, 0, 0, 0, loc)
	switch {
	case due.Before(tomorrow):
		return at
	case due.Before(tomorrow.AddDate(0, 0, 1)):
		return at + " tomorrow"
	}
	return at + " on " + due.Format("Mon 2 Jan")
}

// TickReminder ticks a reminder off from a screen's tick (api.ReminderTicker).
func (d *Daemon) TickReminder(ctx context.Context, id int64) error {
	if err := d.store.MarkDone(ctx, id); err != nil {
		if errors.Is(err, memory.ErrNoReminder) {
			return api.ErrNoReminder
		}
		return err
	}
	d.store.Audit(ctx, "reminder.done", "screen", fmt.Sprintf("#%d", id))
	return nil
}

var _ api.ReminderTicker = (*Daemon)(nil)
