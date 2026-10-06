// Package heartbeat is what makes Mirrin alive: a scheduler that fires
// reminders, scheduled protocols and periodic checks without being asked.
package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/robfig/cron/v3"

	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// Runner executes a task in a chat and returns the text to deliver.
type Runner func(ctx context.Context, chatKey, task string) (string, error)

// Sender delivers text to a chat key.
type Sender func(ctx context.Context, chatKey, text string) error

// Heartbeat drives time-based behaviour. It keeps time by the wall clock:
// timers run on a clock that stops while a laptop sleeps, so it looks at the
// time at least every tick and runs whatever fell due meanwhile, once.
type Heartbeat struct {
	store *memory.Store
	run   Runner
	send  Sender
	owner func() string // chat key for proactive messages
	log   *slog.Logger
	zone  *Zone

	mu         sync.Mutex
	jobs       []*job           // scheduled protocols and built-in jobs
	started    time.Time        // anything due before this was missed while the twin wasn't running
	lastLook   time.Time        // the scheduler's previous look at the clock
	waited     time.Duration    // how long it meant to wait after that look
	sleeps     []span           // recent stretches the computer slept through
	pausedAt   time.Time        // when the owner paused the twin; zero while it runs
	skipped    map[string]*skip // protocols that came due while paused, by name
	resumed    bool             // resumed since the last look: tell the owner what waited
	zoneSeen   bool             // the zone has been compared with the one stored
	zoneKnown  string           // the system's zone the owner last heard about (zoneKey)
	zoneLogged string           // the system's zone at the last look, so a move is logged once
	zoneRetry  time.Time        // a zone change that couldn't be told waits until then
	loaded     bool             // protocols have been loaded since start
	aliveSaved time.Time        // when the last look was stored (aliveKey)
	poke       chan struct{}    // wakes the scheduler early (a reload, a resume)
	now        func() time.Time // the wall clock; tests set it
	tick       time.Duration    // the longest wait between looks (schedule.go)
	first      time.Duration    // the wait before the first look
	spawn      func(func())     // runs a due job; tests run it inline
	checking   map[int64]bool   // follow-ups being looked into now (followup.go)
	paused     atomic.Bool
	runs       atomic.Int64 // numbers protocol runs so each gets its own conversation

	// Record adds a note the twin leaves in a conversation. The daemon sets
	// it so a note never lands in the middle of a turn running there (between
	// a tool call and its result); without it the note is appended directly.
	Record func(ctx context.Context, chatKey string, m llm.Message)
	// OnZone hears that the time zone the twin keeps has changed, so what
	// reads times in it (the reminders tool) can follow.
	OnZone func(loc *time.Location)
	// Theirs reports whether a chat is someone else's (a group, a person the
	// twin answers for the owner), not the owner's own. A follow-up promised
	// there doesn't look on its own (followup.go). Without it, none is.
	Theirs func(chatKey string) bool
	// Preamble adds to a briefing's task what the twin held back overnight
	// (the daemon's held.go). done is called once the briefing has gone
	// out, so what it carried isn't sent again; it may be nil.
	Preamble func(ctx context.Context, p protocols.Protocol) (text string, done func())
}

// SetPaused stops protocols and reminders from firing until resumed. On
// resume the owner gets one message about what came due meanwhile.
func (h *Heartbeat) SetPaused(p bool) {
	if p {
		h.PauseSince(h.now())
		return
	}
	if !h.paused.Swap(false) {
		return
	}
	h.mu.Lock()
	h.resumed = true
	h.mu.Unlock()
	h.wake()
}

// PauseSince pauses the heartbeat as of t: a pause restored after a restart
// keeps the time it began.
func (h *Heartbeat) PauseSince(t time.Time) {
	h.mu.Lock()
	if !h.paused.Load() || h.pausedAt.IsZero() || t.Before(h.pausedAt) {
		h.pausedAt = t
	}
	h.paused.Store(true)
	h.mu.Unlock()
}

// Location is the time zone the twin keeps: the system's, followed as it
// changes, or the one the owner pinned in their settings.
func (h *Heartbeat) Location() *time.Location { return h.zone.Location() }

// ZoneSetting lets the owner's timezone setting be re-read as it changes
// (a settings change while the twin runs). Call it before Start.
func (h *Heartbeat) ZoneSetting(f func() string) { h.zone.setSetting(f) }

// New builds a heartbeat. A nil or time.Local loc follows the system's time
// zone as it changes (a laptop that travels); any other is kept as pinned.
func New(store *memory.Store, run Runner, send Sender, owner func() string, loc *time.Location, log *slog.Logger) *Heartbeat {
	if log == nil {
		log = slog.Default()
	}
	h := &Heartbeat{store: store, run: run, send: send, owner: owner, log: log, zone: zoneFor(loc),
		skipped: map[string]*skip{}, poke: make(chan struct{}, 1), now: time.Now, tick: tick, first: firstLook,
		spawn: func(f func()) { go f() }}
	h.started = h.now()
	return h
}

// Start begins ticking. It returns immediately.
func (h *Heartbeat) Start(ctx context.Context) {
	h.mu.Lock()
	h.started = h.now()
	h.mu.Unlock()
	h.restoreSkipped(ctx)
	go h.loop(ctx)
	go h.keepBackups(ctx)
}

// CheckSchedule reports whether spec is a schedule the heartbeat can run,
// in plain words: cron's own message (the fields it parsed) stays out of
// what the owner and the model read.
func CheckSchedule(spec string) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	if _, err := cron.ParseStandard(spec); err != nil {
		msg := fmt.Sprintf("bad schedule %q: use five fields, minute hour day month weekday, e.g. \"0 7 * * 1-5\"", spec)
		if len(strings.Fields(spec)) == 6 {
			msg += "; it has 6, so drop the seconds"
		}
		return errors.New(msg)
	}
	return nil
}

// LoadProtocols (re)schedules every protocol with a cron expression. A bad
// schedule skips only that protocol; the error names every one skipped. A
// protocol whose schedule is unchanged keeps its place, so a reload never
// runs it twice or misses it.
func (h *Heartbeat) LoadProtocols(ps []protocols.Protocol) error {
	var errs []error
	var fresh []*job
	for _, p := range ps {
		if !p.IsEnabled() || strings.TrimSpace(p.Schedule) == "" {
			continue
		}
		if err := CheckSchedule(p.Schedule); err != nil {
			errs = append(errs, fmt.Errorf("protocol %q: %w", p.Name, err))
			continue
		}
		sched, err := cron.ParseStandard(p.Schedule)
		if err != nil {
			h.log.Warn("protocol schedule", "name", p.Name, "err", err)
			errs = append(errs, fmt.Errorf("protocol %q: bad schedule %q", p.Name, p.Schedule))
			continue
		}
		p := p
		fresh = append(fresh, &job{name: p.Name, spec: p.Schedule, sched: sched, proto: &p})
	}
	now := h.now()
	h.mu.Lock()
	first := !h.loaded
	h.loaded = true
	h.mu.Unlock()
	// At start, schedules pick up from the scheduler's last look before the
	// twin stopped: a run that fell due while it was off is still owed. One
	// added or changed later starts from now.
	since := now
	if first {
		if t := h.lastAlive(now); !t.IsZero() {
			since = t
		}
	}
	h.mu.Lock()
	old := map[string]*job{}
	var keep []*job
	for _, j := range h.jobs {
		if j.proto == nil {
			keep = append(keep, j) // built-in jobs survive reloads
		} else {
			old[j.key()] = j
		}
	}
	for _, j := range fresh {
		if o, ok := old[j.key()]; ok {
			j.last = o.last // unchanged: it keeps its place, even if it ran a moment ago
		} else {
			j.last = since
		}
	}
	h.jobs = append(keep, fresh...)
	h.mu.Unlock()
	for _, j := range fresh {
		h.log.Info("scheduled protocol", "name", j.name, "cron", j.spec)
	}
	h.wake()
	return errors.Join(errs...)
}

// AddJob schedules a built-in task on a cron expression (survives protocol
// reloads). One that fell due while the computer slept runs once on waking,
// however late; one due while paused is skipped. It suits quiet upkeep.
func (h *Heartbeat) AddJob(spec string, fn func(ctx context.Context)) {
	h.AddJobWithin(spec, 0, fn)
}

// AddJobWithin is AddJob for a task the owner hears from (a nudge): a run
// more than within late (the lid shut past it) is skipped, not made up, so
// the 17:00 note never arrives at 01:00. Zero means any lateness.
func (h *Heartbeat) AddJobWithin(spec string, within time.Duration, fn func(ctx context.Context)) {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		h.log.Warn("job schedule", "spec", spec, "err", err)
		return
	}
	j := &job{spec: spec, sched: sched, within: within, last: h.now(), fn: func() {
		if h.paused.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		fn(ctx)
	}}
	h.mu.Lock()
	h.jobs = append(h.jobs, j)
	h.mu.Unlock()
	h.wake()
}

// JobSchedules lists the built-in jobs' schedules, in the order they were
// added: what upkeep the twin keeps on the wall clock.
func (h *Heartbeat) JobSchedules() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, j := range h.jobs {
		if j.proto == nil {
			out = append(out, j.spec)
		}
	}
	return out
}

// NextRun is when a protocol next runs on its schedule, in the zone the
// twin keeps: zero when it is off or runs only when asked.
func (h *Heartbeat) NextRun(p protocols.Protocol) time.Time {
	if !p.IsEnabled() {
		return time.Time{}
	}
	return protocols.NextRun(p.Schedule, h.now().In(h.Location()))
}

// skipAsked reports whether the owner asked to skip this scheduled run of a
// protocol (protocols.SkipKey holds the date of the run to skip). That run
// uses it up; one left from a run that never came (the twin was off) is
// cleared, and a run the owner asks for never looks.
func (h *Heartbeat) skipAsked(ctx context.Context, name string, due time.Time) bool {
	key := protocols.SkipKey(name)
	day, _ := h.store.Get(ctx, key)
	if day == "" || day > due.In(h.Location()).Format(time.DateOnly) {
		return false
	}
	_ = h.store.Unset(ctx, key)
	if day < due.In(h.Location()).Format(time.DateOnly) {
		return false
	}
	h.log.Info("skipped a scheduled protocol run, as asked", "name", name)
	h.store.Audit(ctx, "protocol.skipped", "", name+": skipped this once, as asked")
	return true
}

// RunProtocol executes a protocol now, as asked from the tray or the API, and
// delivers the result to the owner. When it can't run (a variable is still
// unset) or fails, the owner is told why.
func (h *Heartbeat) RunProtocol(ctx context.Context, p protocols.Protocol) {
	h.runProtocol(ctx, p, false)
}

// runProtocol runs a protocol. A scheduled run reports a skip or failure at
// most once a day per protocol, so a routine that keeps failing doesn't nag;
// a run the owner asked for always answers.
func (h *Heartbeat) runProtocol(ctx context.Context, p protocols.Protocol, scheduled bool) {
	h.runLate(ctx, p, scheduled, nil)
}

// runLate is runProtocol for a run that is late (late is nil when it's on
// time): the model hears how late, so a run that makes no sense any more (a
// wake-up call at noon) can stay quiet, and what it sends says it's late.
func (h *Heartbeat) runLate(ctx context.Context, p protocols.Protocol, scheduled bool, late *lateRun) {
	if h.paused.Load() {
		h.log.Info("paused; skipping protocol", "name", p.Name)
		return
	}
	chatKey := h.owner()
	if chatKey == "" {
		h.log.Warn("no owner chat; skipping protocol", "name", p.Name)
		return
	}
	if missing := Unfilled(p); len(missing) > 0 {
		h.log.Warn("protocol skipped: variables not set", "name", p.Name, "missing", strings.Join(missing, ", "))
		h.store.Audit(ctx, "protocol.skipped", chatKey, p.Name+": needs "+strings.Join(missing, ", "))
		// Values are read when protocols load, so the fix needs a /reload.
		h.tell(ctx, chatKey, "skipped", p, scheduled, fmt.Sprintf("%s didn't run: it still needs %s. Add %s under %q in %s, then say /reload.",
			capitalize(p.Name), describeVars(p, missing), pronoun(missing), p.Name, varsFile(p)))
		return
	}
	h.store.Audit(ctx, "protocol.run", chatKey, p.Name)
	rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// A scratch conversation of its own, so a scheduled run never interleaves
	// with a live chat or with another protocol due the same minute.
	runKey := fmt.Sprintf("%s#protocol-%s-%d", chatKey, time.Now().Format("20060102-150405"), h.runs.Add(1))
	task := fmt.Sprintf("Protocol %q: %s", p.Name, p.Prompt)
	if late != nil {
		task += "\n\n" + h.lateTask(*late)
	}
	var done func() // what the briefing carries, let go once it is sent
	if h.Preamble != nil && slices.Contains(p.Tags, "briefing") {
		var more string
		if more, done = h.Preamble(ctx, p); more != "" {
			task += "\n\n" + more
		}
	}
	out, err := h.run(rctx, runKey, task)
	if err != nil {
		h.log.Error("protocol failed", "name", p.Name, "err", err)
		h.store.Audit(ctx, "protocol.failed", chatKey, p.Name+": "+err.Error())
		next := ""
		if strings.TrimSpace(p.Schedule) != "" {
			next = " I'll try again next time it's due."
		}
		h.tell(ctx, chatKey, "failed", p, scheduled, fmt.Sprintf("%s didn't run this time: %s.%s", capitalize(p.Name), reason(err), next))
		return
	}
	if strings.Contains(out, "NOTHING_TO_REPORT") {
		return
	}
	if late != nil {
		out = h.lateNote(*late) + "\n\n" + out
	}
	// Only the result says which routine it is: a note that it didn't run
	// (tell) is a plain notice, never "ready".
	ctx = events.WithSource(ctx, events.Source{Kind: "protocol", Name: p.Name, Briefing: slices.Contains(p.Tags, "briefing")})
	if err := h.send(ctx, chatKey, out); err != nil {
		h.log.Error("protocol send failed", "name", p.Name, "err", err)
	} else if done != nil {
		done()
	}
}

// tell messages the owner about a protocol. For a scheduled run it stays
// quiet when the same kind of notice already went out today.
func (h *Heartbeat) tell(ctx context.Context, chatKey, kind string, p protocols.Protocol, scheduled bool, text string) {
	// The run's own context may have timed out; the notice still goes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	key := "protocol.notice." + kind + "." + strings.ToLower(p.Name)
	today := h.now().In(h.Location()).Format("2006-01-02")
	if last, _ := h.store.Get(ctx, key); scheduled && last == today {
		return
	}
	if err := h.send(ctx, chatKey, text); err != nil {
		h.log.Warn("protocol notice", "name", p.Name, "err", err)
		return
	}
	_ = h.store.Set(ctx, key, today)
}

var reVar = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

// Unfilled lists the {{variables}} still in a protocol's prompt: ones the
// user hasn't set and that have no default.
func Unfilled(p protocols.Protocol) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reVar.FindAllStringSubmatch(p.Prompt, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// NeedsVars says, in plain words, what a protocol still needs before it can
// run: nil when every {{variable}} in its prompt is set. A run asked for in
// chat is refused with it, as a scheduled one is skipped, rather than sending
// the model a prompt with holes in it.
func NeedsVars(p protocols.Protocol) error {
	missing := Unfilled(p)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s can't run yet: it still needs %s. Add %s under %q in %s, say /reload, then try again",
		capitalize(p.Name), describeVars(p, missing), pronoun(missing), p.Name, varsFile(p))
}

// describeVars names missing variables with their descriptions.
func describeVars(p protocols.Protocol, names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n
		if d := strings.TrimSuffix(strings.TrimSpace(p.Vars[n].Description), "."); d != "" {
			_, n := utf8.DecodeRuneInString(d)
			parts[i] += " (" + strings.ToLower(d[:n]) + d[n:] + ")"
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func pronoun(names []string) string {
	if len(names) == 1 {
		return "it"
	}
	return "them"
}

// varsFile is where the user sets a protocol's variables.
func varsFile(p protocols.Protocol) string {
	if p.Source == "" {
		return "vars.yaml in your protocols folder"
	}
	dir := filepath.Dir(p.Source)
	if p.Pack != "" {
		sep := string(filepath.Separator)
		if i := strings.LastIndex(dir, sep+"packs"+sep); i >= 0 {
			dir = dir[:i]
		}
	}
	path := protocols.VarsPath(dir)
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		path = "~" + path[len(home):]
	}
	return path
}

var reStatus = regexp.MustCompile(`\b([45]\d\d)\b`)

// reason says in plain words why a run failed; the details stay in the log.
func reason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "it took longer than ten minutes, so I stopped it"
	case errors.Is(err, context.Canceled):
		return "it was interrupted"
	}
	// Already put in words for the owner: no key yet (a twin started
	// without one), or a model error the daemon explained.
	var km *llm.KeyMissingError
	var ue *llm.UserError
	if errors.As(err, &km) || errors.As(err, &ue) {
		m, _ := llm.Describe(err)
		return strings.TrimSuffix(strings.TrimSpace(m), ".")
	}
	if m := reStatus.FindStringSubmatch(err.Error()); m != nil {
		switch {
		case m[1] == "401" || m[1] == "403":
			return "the model service didn't accept the API key"
		case m[1] == "429":
			return "the model service asked me to slow down"
		case m[1][0] == '5':
			return "the model service was busy or down"
		}
	}
	return "something went wrong on my side (the details are in the log)"
}

// capitalize starts s with a capital, a whole letter at a time: a name
// in any script ("朝のまとめ", "élan check") comes through whole.
func capitalize(s string) string {
	_, n := utf8.DecodeRuneInString(s)
	return strings.ToUpper(s[:n]) + s[n:]
}

// reminderBackoff is how long to wait after each failed delivery; once it
// runs out, the reminder is given up with a note.
var reminderBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// fireReminders delivers the reminders that are due. One that is late says
// when it was due and why it's late (the computer slept, say). Several late
// ones for the same chat, as after a night with the lid shut, go as one
// message listing each with its time, rather than a burst; so do ones being
// retried after the channel was down, each keeping its own retry count.
func (h *Heartbeat) fireReminders(ctx context.Context) {
	if h.paused.Load() {
		return
	}
	now := h.now()
	due, err := h.store.DueReminders(ctx, now)
	if err != nil {
		h.log.Error("reminders", "err", err)
		return
	}
	late := map[string][]memory.Reminder{}
	var order []string
	for _, r := range due {
		if r.Kind == memory.KindCheck || now.Sub(r.DueAt) <= lateAfter { // a follow-up looks, however late
			h.fireOne(ctx, r, now)
			continue
		}
		k := memory.LiveKey(r.ChatKey)
		if _, ok := late[k]; !ok {
			order = append(order, k)
		}
		late[k] = append(late[k], r)
	}
	for _, k := range order {
		if rs := late[k]; len(rs) == 1 {
			h.fireOne(ctx, rs[0], now)
		} else {
			h.fireTogether(ctx, k, rs, now)
		}
	}
}

// fireOne delivers one reminder. A follow-up looks into what it is about
// instead (followup.go).
func (h *Heartbeat) fireOne(ctx context.Context, r memory.Reminder, now time.Time) {
	if r.Kind == memory.KindCheck {
		h.startCheck(ctx, r)
		return
	}
	msg := "Reminder: " + r.Text
	if now.Sub(r.DueAt) > lateAfter {
		why := ""
		if c := h.cause(r.DueAt); c != whyLate {
			why = " — " + causeText(c)
		}
		msg = fmt.Sprintf("Reminder (due %s%s): %s", h.when(r.DueAt, now), why, r.Text)
	}
	key, err := h.deliver(events.WithSource(ctx, events.Source{Kind: "reminder"}), r.ChatKey, msg)
	if err != nil {
		h.undelivered(ctx, r, key, err, now)
		return
	}
	_ = h.store.MarkFired(ctx, r.ID)
	h.store.Audit(ctx, "reminder.fired", key, r.Text)
}

// fireTogether delivers late reminders for one chat as a single message.
func (h *Heartbeat) fireTogether(ctx context.Context, chatKey string, rs []memory.Reminder, now time.Time) {
	causes := make([]why, len(rs))
	for i, r := range rs {
		causes[i] = h.cause(r.DueAt)
	}
	text := h.while(same(causes)) + "\n" + h.reminderLines(rs, now)
	key, err := h.deliver(events.WithSource(ctx, events.Source{Kind: "reminder"}), chatKey, text)
	for _, r := range rs {
		if err != nil {
			h.undelivered(ctx, r, key, err, now)
			continue
		}
		_ = h.store.MarkFired(ctx, r.ID)
		h.store.Audit(ctx, "reminder.fired", key, r.Text)
	}
}

// undelivered retries a reminder that didn't go out after a pause, and
// gives it up with a note once the retries run out.
func (h *Heartbeat) undelivered(ctx context.Context, r memory.Reminder, key string, err error, now time.Time) {
	if r.Attempts < len(reminderBackoff) {
		next := now.Add(reminderBackoff[r.Attempts])
		_ = h.store.RetryReminder(ctx, r.ID, r.Attempts+1, next)
		h.log.Warn("reminder not delivered; will retry", "id", r.ID, "retry_at", next.Format(time.Kitchen), "err", err)
		return
	}
	_ = h.store.GiveUpReminder(ctx, r.ID)
	h.log.Error("reminder not delivered; giving up", "id", r.ID, "err", err)
	h.store.Audit(ctx, "reminder.failed", key, fmt.Sprintf("#%d %s: %v", r.ID, r.Text, err))
	// Leave a note where the owner talks to the twin, so it knows the
	// reminder never arrived and can own up next time.
	note := fmt.Sprintf("(I couldn't deliver the reminder %q that was due %s: I kept trying for over an hour, but %s wasn't reachable.)", r.Text, h.when(r.DueAt, now), channelName(key))
	if h.Record != nil {
		h.Record(ctx, key, llm.Text(llm.RoleAssistant, note))
	} else {
		_ = h.store.AppendMessage(ctx, key, llm.Text(llm.RoleAssistant, note))
	}
}

// deliver sends to the chat a reminder belongs to, then to wherever the owner
// is reachable. It returns the key it delivered to (or last tried).
func (h *Heartbeat) deliver(ctx context.Context, chatKey, text string) (string, error) {
	key := memory.LiveKey(chatKey)
	err := h.sendWithin(ctx, key, text)
	if err == nil {
		return key, nil
	}
	if owner := h.owner(); owner != "" && owner != key {
		if err2 := h.sendWithin(ctx, owner, text); err2 == nil {
			return owner, nil
		}
	}
	return key, err
}

// sendTimeout bounds one delivery from the scheduler's own loop.
var sendTimeout = 2 * time.Minute

// sendWithin is send, given up after sendTimeout: the scheduler delivers
// reminders and notes itself, and a channel that hangs (a transport
// reconnecting, a stuck osascript) must not hold up every routine behind it.
// A send that ignores its context finishes on its own, but no longer waited on.
func (h *Heartbeat) sendWithin(ctx context.Context, key, text string) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.send(ctx, key, text) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%s didn't take the message in time: %w", channelName(key), ctx.Err())
	}
}

// when formats a due time: the clock alone today, with the day otherwise.
func (h *Heartbeat) when(t, now time.Time) string {
	loc := h.Location()
	t, now = t.In(loc), now.In(loc)
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return "at " + t.Format("15:04")
	}
	return t.Format("Mon 2 Jan 15:04")
}

func channelName(key string) string {
	if i := strings.IndexByte(key, ':'); i > 0 {
		return key[:i]
	}
	return "the chat"
}

// keepBackups copies memory.db into data/backups once a day (checked hourly,
// so a laptop asleep at night still gets one), keeping a week of copies.
func (h *Heartbeat) keepBackups(ctx context.Context) {
	t := time.NewTimer(time.Minute) // let startup settle first
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if path, err := h.store.BackupIfDue(ctx, 24*time.Hour, 7); err != nil {
			h.log.Error("memory backup failed", "err", err)
			h.store.Audit(ctx, "memory.backup_failed", "", err.Error())
		} else if path != "" {
			h.log.Info("memory backed up", "path", path)
		}
		t.Reset(time.Hour)
	}
}
