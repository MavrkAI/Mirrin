package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/memory"
	memskill "github.com/MavrkAI/Mirrin/internal/skills/memory"
	"github.com/MavrkAI/Mirrin/internal/skills/reminders"
)

// When the owner tells the twin something with a date in it on this Mac
// ("Mum's birthday is on the 12th"), the "Noted · Undo" under the reply
// also asks "Remind me on the 11th?". One tap sets the reminder, with no
// model turn: the date is read by rule (reminders.Remind). It is offered
// only then and there, and only for a plain fact learned in a chat on this
// Mac: never for a private one, nor one from mail, the web or anyone else.
// Nothing is set without the tap.
//
// The reminder brings back, as it goes out, what else the twin knows about
// the same person (a gift idea), leaving out anything private; a birthday's
// or an anniversary's comes round again every year. Forgetting the fact
// (Undo, "forget that", the memory page) removes its reminders too.

// factRemindersKey is the kv record of which reminders were set from which
// fact.
const factRemindersKey = memory.FactRemindersKey

// offerFor is how long a fact's "Remind me…?" can be tapped.
const offerFor = time.Hour

// factLink is one fact's reminders, the next one to come last, and the day
// of the occasion that one is for.
type factLink struct {
	Fact      int64   `json:"fact"`
	Reminders []int64 `json:"reminders"`
	Day       string  `json:"day"` // 2006-01-02
	Yearly    bool    `json:"yearly,omitempty"`
}

// factRemindMu keeps the kv record whole between a tap, a reminder going
// out and a forget.
var factRemindMu sync.Mutex

// watchFactReminders has reminders set from a fact bring back what else
// the twin knows, come round again, and go with the fact.
func (d *Daemon) watchFactReminders() {
	if d.beat != nil {
		d.beat.Related = d.factRelated
		d.beat.Fired = d.factReminderFired
	}
	d.store.OnForgot(func() { d.dropForgottenFactReminders(context.Background()) })
}

// dateOffer is the reminder a fact just kept may offer on the screen.
func (d *Daemon) dateOffer(f memory.Fact) (reminders.Offer, bool) {
	if privateFact(f) || savedPage(f) || !notedChats[f.Source] {
		return reminders.Offer{}, false
	}
	return reminders.Remind(f.Content, clock().In(d.location()))
}

// RemindFact sets the reminder a noted fact offered (api.FactReminder),
// once, and says when it will come.
func (d *Daemon) RemindFact(ctx context.Context, id int64) (string, error) {
	f, err := d.store.FactByID(ctx, id)
	if errors.Is(err, memory.ErrNoFact) {
		return "", api.ErrNoOffer
	}
	if err != nil {
		return "", err
	}
	if f.CreatedAt.IsZero() || clock().Sub(f.CreatedAt) >= offerFor {
		return "", api.ErrNoOffer
	}
	o, ok := d.dateOffer(f)
	if !ok {
		return "", api.ErrNoOffer
	}
	factRemindMu.Lock()
	defer factRemindMu.Unlock()
	links := d.factLinks(ctx)
	if slices.ContainsFunc(links, func(l factLink) bool { return l.Fact == f.ID }) {
		return "That reminder is already set.", nil
	}
	rid, err := d.store.AddReminder(ctx, f.Source, o.Due, o.Text)
	if err != nil {
		return "", err
	}
	links = append(links, factLink{Fact: f.ID, Reminders: []int64{rid}, Day: o.Occasion.Day.Format(time.DateOnly), Yearly: o.Occasion.Yearly})
	if err := d.saveFactLinks(ctx, links); err != nil {
		_ = d.store.DropReminder(ctx, rid)
		return "", err
	}
	d.store.Audit(ctx, "reminder.set", f.Source, fmt.Sprintf("#%d from fact #%d with a tap, due %s", rid, f.ID, o.Due.UTC().Format(time.RFC3339)))
	return o.Done, nil
}

var _ api.FactReminder = (*Daemon)(nil)

func (d *Daemon) factLinks(ctx context.Context) []factLink {
	v, err := d.store.Get(ctx, factRemindersKey)
	if err != nil || v == "" {
		return nil
	}
	var links []factLink
	_ = json.Unmarshal([]byte(v), &links)
	return links
}

func (d *Daemon) saveFactLinks(ctx context.Context, links []factLink) error {
	if len(links) == 0 {
		return d.store.Unset(ctx, factRemindersKey)
	}
	b, err := json.Marshal(links)
	if err != nil {
		return err
	}
	return d.store.Set(ctx, factRemindersKey, string(b))
}

// linkFor is the link whose next reminder is r, and its place.
func linkFor(links []factLink, r memory.Reminder) (int, bool) {
	i := slices.IndexFunc(links, func(l factLink) bool { return len(l.Reminders) > 0 && l.Reminders[len(l.Reminders)-1] == r.ID })
	return i, i >= 0
}

// dropForgottenFactReminders removes every reminder set from a fact that is
// no longer in memory, gone out or not, so its wording goes with it.
func (d *Daemon) dropForgottenFactReminders(ctx context.Context) {
	factRemindMu.Lock()
	defer factRemindMu.Unlock()
	links := d.factLinks(ctx)
	kept := links[:0:0]
	for _, l := range links {
		if _, err := d.store.FactByID(ctx, l.Fact); !errors.Is(err, memory.ErrNoFact) {
			kept = append(kept, l)
			continue
		}
		for _, rid := range l.Reminders {
			if err := d.store.DropReminder(ctx, rid); err != nil {
				d.log.Warn("reminder from a forgotten fact not removed", "id", rid, "err", err)
			}
		}
	}
	if len(kept) != len(links) {
		_ = d.saveFactLinks(ctx, kept)
	}
}

// factReminderFired sets next year's reminder for a birthday's or an
// anniversary's that has just gone out.
func (d *Daemon) factReminderFired(ctx context.Context, r memory.Reminder) {
	factRemindMu.Lock()
	defer factRemindMu.Unlock()
	links := d.factLinks(ctx)
	i, ok := linkFor(links, r)
	if !ok || !links[i].Yearly {
		return
	}
	f, err := d.store.FactByID(ctx, links[i].Fact)
	if err != nil {
		return
	}
	loc := d.location()
	day, err := time.ParseInLocation(time.DateOnly, links[i].Day, loc)
	if err != nil {
		return
	}
	o, ok := reminders.Occasion{Day: day, Yearly: true}.NextYear(f.Content, clock().In(loc))
	if !ok {
		return
	}
	rid, err := d.store.AddReminder(ctx, r.ChatKey, o.Due, o.Text)
	if err != nil {
		d.log.Warn("next year's reminder not set", "fact", f.ID, "err", err)
		return
	}
	links[i].Reminders = append(links[i].Reminders, rid)
	links[i].Day = o.Occasion.Day.Format(time.DateOnly)
	if err := d.saveFactLinks(ctx, links); err != nil {
		_ = d.store.DropReminder(ctx, rid)
	}
}

// relatedMax is how many other facts a reminder brings back.
const relatedMax = 2

// factRelated is what a reminder set from a fact brings back as it goes
// out: up to two other things the owner told the twin about the same
// person or thing ("You also told me: Mum loves orchids."). Only what the
// owner said themselves, and never a private fact or a saved page: a
// reminder is read out loud.
func (d *Daemon) factRelated(ctx context.Context, r memory.Reminder) string {
	factRemindMu.Lock()
	links := d.factLinks(ctx)
	factRemindMu.Unlock()
	i, ok := linkFor(links, r)
	if !ok {
		return ""
	}
	f, err := d.store.FactByID(ctx, links[i].Fact)
	if err != nil {
		return ""
	}
	d.cmu.RLock()
	owner := strings.ToLower(firstName(d.cfg.User.Name))
	d.cmu.RUnlock()
	terms := aboutWhom(f.Content, owner)
	if len(terms) == 0 {
		return ""
	}
	found, err := d.store.Recall(ctx, strings.Join(terms, " "), 20)
	if err != nil {
		return ""
	}
	var lines []string
	for _, o := range found {
		if o.ID == f.ID || privateFact(o) || savedPage(o) || !d.ownerStated(o.Source) || !mentionsAny(o.Content, terms) {
			continue
		}
		lines = append(lines, strings.TrimRight(strings.TrimSpace(o.Content), ".")+".")
		if len(lines) == relatedMax {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\nYou also told me: " + strings.Join(lines, " ")
}

// healthFactRe adds to sensitiveFactRe the health words a dated fact is
// likely to carry ("My biopsy results are on the 12th").
var healthFactRe = regexp.MustCompile(`(?i)\b(biopsy|scans?|cancer|tumou?rs?|chemo\w*|radiotherapy|mri|x-rays?|operation|illness|sick|depress\w*|anxiety|smear|blood tests?)\b`)

// privateFact reports whether a fact is private by its subject or by what
// it says: a reminder is read out loud and shown on the wall screen, so
// the date offer and what a reminder brings back leave it out.
func privateFact(f memory.Fact) bool {
	return memskill.Sensitive(f.Subject) || sensitiveFactRe.MatchString(f.Content) || healthFactRe.MatchString(f.Content)
}

// savedPagePrefix starts every fact memskill.SavePage keeps.
const savedPagePrefix = "The user saved a web page on "

// savedPage reports whether a fact is a web page the owner saved: its date
// is the day it was saved or the page's own, never one to be reminded of.
func savedPage(f memory.Fact) bool { return strings.HasPrefix(f.Content, savedPagePrefix) }

var (
	reWho      = regexp.MustCompile(`^(.*?)\b(?:birthday|bday|b-day|anniversary|was born|born|is|are|on|at|tomorrow)\b`)
	whoSkip    = map[string]bool{"my": true, "our": true, "the": true, "a": true, "an": true, "his": true, "her": true, "their": true, "your": true, "i": true, "we": true, "me": true, "and": true, "of": true}
	reWordOnly = regexp.MustCompile(`[^\p{L}\p{N}-]+`)
)

// aboutWhom is who or what a dated fact is about, as the words before the
// occasion ("Mum's birthday…" is about "mum"), leaving out the owner
// themselves: everything is about them.
func aboutWhom(fact, owner string) []string {
	s := strings.ToLower(strings.ReplaceAll(fact, "’", "'"))
	m := reWho.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	var out []string
	for _, w := range strings.Fields(m[1]) {
		w = strings.TrimSuffix(w, "'s")
		w = strings.Trim(reWordOnly.ReplaceAllString(w, ""), "-")
		if len([]rune(w)) < 2 || whoSkip[w] || w == owner {
			continue
		}
		out = append(out, w)
		if len(out) == 3 {
			break
		}
	}
	return out
}

// mentionsAny reports whether text has one of terms as a word of its own.
func mentionsAny(text string, terms []string) bool {
	words := strings.Fields(strings.ToLower(reWordOnly.ReplaceAllString(strings.ReplaceAll(text, "'s", ""), " ")))
	for _, t := range terms {
		if slices.Contains(words, t) {
			return true
		}
	}
	return false
}
