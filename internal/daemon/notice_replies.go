package daemon

import (
	"context"
	"crypto/sha256"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/heartbeat"
	"github.com/MavrkAI/Mirrin/internal/jev"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// Short replies to the twin's own notes, read by Jev when they aren't in
// the exact lists: "sure go ahead" or "nah leave it" to the travel
// welcome's offer, and "stop sending these" right after the Sunday weekly
// note or a meeting brief, where "these" only means something next to the
// note it answers. The exact rules always run first; this runs only when
// Jev is on, only for the owner in their own chat, and only while the note
// is the twin's last word there. Anything less than a confident answer, or
// any failure, leaves the message to the model, as before.
//
// What is sent is the owner's few words and a line saying what they
// answer: the travel offer with the friend's name taken out, or for a note
// only a description of its kind, never the note itself.

const (
	noticeUse      = "notice-replies" // its line in the usage ledger
	noticeMaxChars = 200
	noticeMaxWords = 30
	weeklyReplyFor = 36 * time.Hour // how long after the weekly note a reply still answers it
	briefReplyFor  = 3 * time.Hour  // and after a meeting brief
)

// lastNotice is the weekly note or brief last delivered in a chat: its
// kind, when, and a digest of what the chat's history keeps of it. It lives
// in memory only; after a restart a reply is left to the model.
type lastNotice struct {
	kind string // "weekly" or "brief"
	at   time.Time
	sum  [sha256.Size]byte
}

// noteNotice remembers a weekly note or brief just delivered at where
// (record is what the chat's history keeps of it). Any other notice there
// replaces it.
func (d *Daemon) noteNotice(ctx context.Context, where, record string) {
	src, _ := events.SourceFrom(ctx)
	key := homeKey(where)
	d.jev.mu.Lock()
	defer d.jev.mu.Unlock()
	if src.Kind != "weekly" && src.Kind != "brief" {
		delete(d.jev.notices, key)
		return
	}
	if d.jev.notices == nil {
		d.jev.notices = map[string]lastNotice{}
	}
	d.jev.notices[key] = lastNotice{kind: src.Kind, at: clock(), sum: sha256.Sum256([]byte(strings.TrimSpace(record)))}
}

// noticeReply answers the owner's short reply to the travel offer, the
// weekly note or a meeting brief when Jev reads it confidently. ok is false,
// with nothing sent, unless every local check passes first.
func (d *Daemon) noticeReply(ctx context.Context, in channels.Inbound) (string, bool) {
	c := d.Config()
	if !c.JevOn() {
		return "", false
	}
	key := in.Key()
	if !in.IsOwner || forgeable(in.Channel) || memory.IsScratch(key) || !d.ownersOwnChat(key) || d.openQuestion(key) {
		return "", false
	}
	text := strings.TrimSpace(in.Text)
	if n := utf8.RuneCountInString(text); n < 1 || n > noticeMaxChars || len(strings.Fields(text)) > noticeMaxWords {
		return "", false
	}
	if o, ok := d.travelOfferOpen(ctx, key); ok {
		return d.travelReplyByJev(ctx, key, text, o)
	}
	if kind, ok := d.noteAnswered(ctx, key, c.Watch.Briefs()); ok {
		return d.noteReplyByJev(ctx, key, text, kind)
	}
	return "", false
}

// noteAnswered is the kind of note ("weekly" or "brief") a reply in key
// would answer: one delivered there recently, still the twin's last word,
// and still switched on.
func (d *Daemon) noteAnswered(ctx context.Context, key string, briefsOn bool) (string, bool) {
	d.jev.mu.Lock()
	n, ok := d.jev.notices[homeKey(key)]
	d.jev.mu.Unlock()
	if !ok {
		return "", false
	}
	age := clock().Sub(n.at)
	switch n.kind {
	case "weekly":
		if off, _ := d.store.Get(ctx, keyWeeklyOff); off == "1" || age > weeklyReplyFor {
			return "", false
		}
	case "brief":
		if !briefsOn || age > briefReplyFor {
			return "", false
		}
	default:
		return "", false
	}
	last, ok := d.lastAssistant(ctx, key)
	if !ok || sha256.Sum256([]byte(strings.TrimSpace(last))) != n.sum {
		return "", false
	}
	return n.kind, true
}

// lastAssistant is the twin's latest message in key.
func (d *Daemon) lastAssistant(ctx context.Context, key string) (string, bool) {
	h, err := d.store.History(ctx, key, 8)
	if err != nil {
		return "", false
	}
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == llm.RoleAssistant {
			return h[i].PlainText(), true
		}
	}
	return "", false
}

// The travel offer's question and how sure Jev must be to act on each answer.
var travelQuestion = jev.Choice{
	Instructions: "The twin offered the owner `twin_offered`. What does `owner_replied` say to that offer?",
	Options: []jev.Opt{
		{Name: "accept", Rubric: "A clear yes to the offer as made, with no change or condition ('sure go ahead', 'that'd be great', 'go for it')."},
		{Name: "accept_changed", Rubric: "A yes that changes or adds something: another day or time, a condition, 'yes but…'."},
		{Name: "decline", Rubric: "A no to this offer ('nah leave it', 'not this time', 'I'll sort it myself')."},
		{Name: "stop_all", Rubric: "Asks the twin to stop making offers like this at all ('stop sending these', 'don't do this when I travel')."},
		{Name: "ack", Rubric: "Only acknowledges or thanks, without a yes or no ('ok', 'thanks', 'nice')."},
		{Name: "other", Rubric: "Anything else: a question, or a message about something else."},
	},
}

// travelSettles maps a confident answer to the offer to settleTravelOffer's
// kind. accept_changed, ack and other aren't here: they stay with the model.
var travelSettles = map[string]struct {
	kind    string
	p, conf float64
}{
	"accept":   {"yes", 0.9, 0.7},
	"decline":  {"no", 0.85, 0.6},
	"stop_all": {"stop", 0.9, 0.7},
}

// travelReplyByJev settles the open travel offer o when Jev reads the
// owner's reply as a clear yes, no or stop.
func (d *Daemon) travelReplyByJev(ctx context.Context, key, text string, o heartbeat.TravelOffer) (string, bool) {
	state := map[string]string{"twin_offered": withoutName(o.Ask, o.Who), "owner_replied": text}
	res, ok := d.judge(ctx, noticeUse, state, map[string]jev.Question{"answer": travelQuestion})
	if !ok {
		return "", false
	}
	a := res.Answers["answer"]
	s, ok := travelSettles[a.Choice]
	if !ok || a.P(a.Choice) < s.p || a.Confidence < s.conf {
		return "", false
	}
	reply, ok := d.settleTravelOffer(ctx, key, text, s.kind, o)
	if ok {
		d.store.Audit(ctx, "travel.offer", key, "answered "+s.kind+" (jev)")
	}
	return reply, ok
}

// withoutName is ask with every mention of who, in full or by first name
// and in any case, replaced by "[a friend]".
func withoutName(ask, who string) string {
	names := []string{strings.TrimSpace(who)}
	if f := strings.Fields(who); len(f) > 1 {
		names = append(names, f[0])
	}
	for _, n := range names {
		if n == "" {
			continue
		}
		ask = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(n)).ReplaceAllLiteralString(ask, "[a friend]")
	}
	return ask
}

// noteQuestion asks whether a reply to a note asks to stop such notes.
var noteQuestion = jev.Choice{
	Instructions: "The twin just sent the owner `twin_sent`. Does `owner_replied` ask to stop getting these notes?",
	Options: []jev.Opt{
		{Name: "stop_these", Rubric: "Asks to stop getting these notes altogether ('stop sending these', 'I don't need these', 'no more of this please')."},
		{Name: "change_them", Rubric: "Wants them changed: less often, shorter, or only some of them ('only for client meetings')."},
		{Name: "about_this_one", Rubric: "Responds to what this note said: thanks, a question, a correction or a request."},
		{Name: "other", Rubric: "Anything else."},
	},
}

// noteSent describes each kind of note to Jev, in place of the note.
var noteSent = map[string]string{
	"weekly": "its Sunday note about what it handled for the owner this week",
	"brief":  "a short note before a meeting about the people in it",
}

// noteReplyByJev turns the weekly note or meeting briefs off when Jev reads
// the owner's reply to one as a clear "stop sending these".
func (d *Daemon) noteReplyByJev(ctx context.Context, key, text, kind string) (string, bool) {
	state := map[string]string{"twin_sent": noteSent[kind], "owner_replied": text}
	res, ok := d.judge(ctx, noticeUse, state, map[string]jev.Question{"answer": noteQuestion})
	if !ok {
		return "", false
	}
	a := res.Answers["answer"]
	if a.Choice != "stop_these" || a.P("stop_these") < 0.9 || a.Confidence < 0.7 {
		return "", false
	}
	var reply string
	switch kind {
	case "weekly":
		reply = d.weeklyOff(ctx)
		d.store.Audit(ctx, "weekly.off", key, "asked to stop (jev)")
	default:
		r, err := d.briefsOff()
		if err != nil {
			reply = briefSwitchFailed
			break
		}
		reply = r
		d.store.Audit(ctx, "briefs.off", key, "asked to stop (jev)")
	}
	d.jev.mu.Lock()
	delete(d.jev.notices, homeKey(key))
	d.jev.mu.Unlock()
	d.recordTurn(ctx, key, text, reply)
	return reply, true
}
