package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/protocols"
)

const (
	// tick is the longest the scheduler waits between looks at the clock.
	// Go's timers run on a clock that stops while a Mac sleeps, so a timer
	// set at night for 7:00 would go off hours late, with whatever else was
	// due, all at once. Looking at the wall clock at least this often means a
	// routine is at most a tick late once the lid opens, and runs once,
	// however long it slept.
	tick = 20 * time.Second
	// firstLook is how long after starting the scheduler first looks, so the
	// channels can connect before anything is sent.
	firstLook = 20 * time.Second

	// lateAfter is how late a run or a reminder is before it says so.
	lateAfter = 10 * time.Minute
	// catchUpWithin is how late a missed run can still be made up. Older
	// ones are listed for the owner in one message instead of all running.
	catchUpWithin = 12 * time.Hour
	// sleepGap is how far the wall clock may run ahead of the monotonic one
	// between two looks before it counts as the computer having slept.
	sleepGap = 90 * time.Second
	// stallGap: a wait that took this much longer than asked was a sleep
	// (or a suspended process), even where the monotonic clock kept going.
	stallGap = 5 * time.Minute
	// maxCount caps counting the runs a long gap held. Past it the last run
	// due is found by halving (lastDue), and the count is "many".
	maxCount = 1000
	// manyRuns stands for more runs than maxCount.
	manyRuns = maxCount + 1
)

// job is one schedule: a protocol, or one of the twin's own jobs.
type job struct {
	name   string // the protocol's name; "" for a built-in job
	spec   string
	sched  cron.Schedule
	proto  *protocols.Protocol // nil for a built-in job
	fn     func()              // a built-in job
	within time.Duration       // a built-in job later than this is skipped; 0 catches up at any lateness
	last   time.Time           // every run due at or before this is done (or skipped)
}

func (j *job) key() string { return strings.ToLower(j.name) + "\x00" + j.spec }

// span is a stretch of time the computer slept through.
type span struct{ from, to time.Time }

// why is the reason something is late.
type why int

const (
	whyLate    why = iota // for no reason the heartbeat knows (a channel was down, say)
	whyAsleep             // the computer was asleep
	whyStopped            // the twin wasn't running
	whyPaused             // the owner had paused the twin
)

// lateRun is a protocol run made late: when it was last due, how many runs
// the gap held (they are made up with one), and why it's late.
type lateRun struct {
	due    time.Time
	missed int
	why    why
}

// skip is a protocol that didn't run: it came due while the twin was
// paused, or so long ago it's not worth making up.
type skip struct {
	Name  string    `json:"name"`
	Count int       `json:"count"`
	Last  time.Time `json:"last"`
}

const (
	skippedKey = "heartbeat.skipped.v1"
	// aliveKey is when the scheduler last looked at the clock, kept about
	// once a minute: what fell due after it was missed while the twin wasn't
	// running.
	aliveKey = memory.KeyAlive
)

// wake makes the scheduler look again soon (a reload, a resume).
func (h *Heartbeat) wake() {
	select {
	case h.poke <- struct{}{}:
	default:
	}
}

func (h *Heartbeat) loop(ctx context.Context) {
	t := time.NewTimer(h.first)
	defer t.Stop()
	looked := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.poke:
			if !looked {
				continue // nothing before the first look: the channels are connecting
			}
		}
		looked = true
		h.look(ctx)
		t.Reset(h.nextWait())
	}
}

// look is one look at the clock: notice a sleep or a move to another time
// zone, tell the owner what waited while the twin was paused, run whatever
// fell due, and deliver the reminders that are due.
func (h *Heartbeat) look(ctx context.Context) {
	now := h.now()
	h.noticeGap(now)
	h.followZone(ctx)
	if h.takeResumed() {
		h.resume(ctx, now)
	}
	ran := h.runDue(ctx, now)
	h.fireReminders(ctx)
	h.markAlive(ctx, now, ran)
}

// nextWait is how long to wait before the next look: until the next run is
// due, but never longer than a tick.
func (h *Heartbeat) nextWait() time.Duration {
	now := h.now()
	loc := h.Location()
	wait := h.tick
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, j := range h.jobs {
		if next := j.sched.Next(j.last.In(loc)); !next.IsZero() && next.Sub(now) < wait {
			wait = next.Sub(now)
		}
	}
	if wait < 10*time.Millisecond {
		wait = 10 * time.Millisecond
	}
	h.waited = now.Round(0).Sub(h.lastLook.Round(0)) + wait
	return wait
}

// noticeGap compares this look with the last. The wall clock running well
// ahead of the monotonic one (which stops during sleep), or a wait that took
// minutes longer than asked, means the computer slept; the stretch is kept
// so what fell due in it can say so. A clock set back restarts the schedules
// from now.
func (h *Heartbeat) noticeGap(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev, waited := h.lastLook, h.waited
	h.lastLook = now
	if prev.IsZero() {
		return
	}
	wall := now.Round(0).Sub(prev.Round(0))
	mono := now.Sub(prev) // the wall clock too, when either has no monotonic reading
	switch {
	case wall < -sleepGap:
		for _, j := range h.jobs {
			if j.last.After(now) {
				j.last = now
			}
		}
		h.log.Warn("the clock went back", "by", (-wall).Round(time.Second))
	case wall-mono > sleepGap || wall > waited+stallGap:
		h.sleeps = append(h.sleeps, span{prev, now})
		for len(h.sleeps) > 0 && (len(h.sleeps) > 16 || now.Sub(h.sleeps[0].to) > 8*24*time.Hour) {
			h.sleeps = h.sleeps[1:]
		}
		h.log.Info("woke up", "slept", wall.Round(time.Minute))
	}
}

// cause says why something due at t is late.
func (h *Heartbeat) cause(t time.Time) why {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sleeps {
		if t.After(s.from) && !t.After(s.to) {
			return whyAsleep
		}
	}
	if !h.started.IsZero() && t.Before(h.started) {
		return whyStopped
	}
	return whyLate
}

// runDue runs every job that fell due since it last ran, once, however many
// runs the gap held. While paused a protocol's run is noted for the owner
// instead; a protocol missed so long ago that running it now makes no sense
// is listed for the owner rather than run. It reports whether anything was due.
func (h *Heartbeat) runDue(ctx context.Context, now time.Time) bool {
	loc := h.Location()
	type ready struct {
		j      *job
		latest time.Time
		n      int
	}
	var due []ready
	h.mu.Lock()
	for _, j := range h.jobs {
		first := j.sched.Next(j.last.In(loc))
		if first.IsZero() || first.After(now) {
			continue
		}
		latest, n := first, 1
		for {
			next := j.sched.Next(latest)
			if next.IsZero() || next.After(now) {
				break
			}
			if n == maxCount {
				latest, n = lastDue(j.sched, latest, now.In(loc)), manyRuns
				break
			}
			latest, n = next, n+1
		}
		j.last = now
		due = append(due, ready{j, latest, n})
	}
	paused := h.paused.Load()
	h.mu.Unlock()

	var missed []skip
	var whys []why
	for _, d := range due {
		if d.j.proto == nil {
			// A built-in job that messages the owner (a nudge) is skipped
			// when it's too late to make sense; the rest catch up quietly.
			if !paused && (d.j.within == 0 || now.Sub(d.latest) <= d.j.within) {
				h.spawn(d.j.fn)
			}
			continue
		}
		if h.skipAsked(ctx, d.j.name, d.latest) { // heartbeat.go: "skip the next one"
			continue
		}
		if paused {
			h.noteSkipped(ctx, d.j.name, d.n, d.latest)
			continue
		}
		p := *d.j.proto
		switch lateBy := now.Sub(d.latest); {
		case lateBy <= lateAfter:
			h.spawn(func() { h.runLate(context.Background(), p, true, nil) })
		case lateBy <= catchUpWithin:
			lr := &lateRun{due: d.latest, missed: d.n, why: h.cause(d.latest)}
			h.log.Info("running a protocol late", "name", p.Name, "due", d.latest.Format(time.RFC3339), "missed", d.n)
			h.spawn(func() { h.runLate(context.Background(), p, true, lr) })
		default:
			missed = append(missed, skip{Name: p.Name, Count: d.n, Last: d.latest})
			whys = append(whys, h.cause(d.latest))
		}
	}
	if len(missed) > 0 {
		h.tellMissed(ctx, same(whys), missed, now)
	}
	return len(due) > 0
}

// lastDue is the last run of s due at or before now, given after, a time
// whose next run is due by now. A long gap holds too many runs to count
// through one by one, so the stretch between the two is halved until it is
// a second long: at most one run fits in that, and it is the one.
func lastDue(s cron.Schedule, after, now time.Time) time.Time {
	lo, hi := after, now
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if next := s.Next(mid); !next.IsZero() && !next.After(now) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return s.Next(lo)
}

// timesText says how many times something was due: "3 times", "many times".
func timesText(n int) string {
	switch {
	case n == 1:
		return "once"
	case n > maxCount:
		return "many times"
	}
	return fmt.Sprintf("%d times", n)
}

// lastAlive is when the scheduler last looked at the clock before this
// start, as stored; zero if never (a first start, or an upgrade).
func (h *Heartbeat) lastAlive(now time.Time) time.Time {
	v, _ := h.store.Get(context.Background(), aliveKey)
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil || t.After(now) {
		return time.Time{}
	}
	return t
}

// markAlive stores when the scheduler looked: about once a minute, and at
// once after a run, so a restart never runs it again.
func (h *Heartbeat) markAlive(ctx context.Context, now time.Time, ran bool) {
	h.mu.Lock()
	due := ran || h.aliveSaved.IsZero() || now.Sub(h.aliveSaved) >= time.Minute || now.Before(h.aliveSaved)
	if due {
		h.aliveSaved = now
	}
	h.mu.Unlock()
	if due {
		_ = h.store.Set(ctx, aliveKey, now.Round(0).UTC().Format(time.RFC3339Nano))
	}
}

// noteSkipped adds a run skipped while paused to what the owner hears about
// on resume. It is kept in the store, so a restart while paused keeps it.
func (h *Heartbeat) noteSkipped(ctx context.Context, name string, n int, last time.Time) {
	h.mu.Lock()
	s := h.skipped[strings.ToLower(name)]
	if s == nil {
		s = &skip{Name: name}
		h.skipped[strings.ToLower(name)] = s
	}
	s.Count += n
	if last.After(s.Last) {
		s.Last = last
	}
	b, _ := json.Marshal(h.skipped)
	h.mu.Unlock()
	h.log.Info("paused; skipped protocol", "name", name)
	_ = h.store.Set(ctx, skippedKey, string(b))
}

// restoreSkipped reloads what a paused twin skipped before a restart.
func (h *Heartbeat) restoreSkipped(ctx context.Context) {
	v, _ := h.store.Get(ctx, skippedKey)
	if v == "" {
		return
	}
	if !h.paused.Load() {
		_ = h.store.Unset(ctx, skippedKey) // left from a pause that ended without a word
		return
	}
	m := map[string]*skip{}
	if json.Unmarshal([]byte(v), &m) != nil {
		return
	}
	h.mu.Lock()
	for k, s := range m {
		if s != nil {
			h.skipped[k] = s
		}
	}
	h.mu.Unlock()
}

func (h *Heartbeat) takeResumed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.resumed
	h.resumed = false
	return r
}

// resume tells the owner, in one message, what came due while the twin was
// paused: the reminders, each with its time, and the routines that didn't
// run. Nothing is run or sent in a burst. Reminders for other chats go to
// those chats the same way. A follow-up that fell due isn't listed: it
// looks once the twin runs again.
func (h *Heartbeat) resume(ctx context.Context, now time.Time) {
	h.mu.Lock()
	since := h.pausedAt
	h.pausedAt = time.Time{}
	skipped := make([]skip, 0, len(h.skipped))
	for _, s := range h.skipped {
		skipped = append(skipped, *s)
	}
	h.skipped = map[string]*skip{}
	h.mu.Unlock()
	_ = h.store.Unset(ctx, skippedKey)
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Last.Before(skipped[j].Last) })

	due, err := h.store.DueReminders(ctx, now)
	if err != nil {
		h.log.Error("reminders", "err", err)
	}
	byChat := map[string][]memory.Reminder{}
	var order []string
	for _, r := range due {
		if r.Kind == memory.KindCheck {
			continue // a follow-up isn't listed: it looks now, as it would have (fireReminders)
		}
		k := memory.LiveKey(r.ChatKey)
		if _, ok := byChat[k]; !ok {
			order = append(order, k)
		}
		byChat[k] = append(byChat[k], r)
	}
	head := "While I was paused:"
	if !since.IsZero() {
		head = fmt.Sprintf("While I was paused (since %s):", strings.TrimPrefix(h.when(since, now), "at "))
	}
	owner := h.owner()
	if len(skipped) > 0 || len(byChat[owner]) > 0 {
		if owner == "" {
			h.log.Warn("no owner chat; nobody told what was skipped while paused")
		} else {
			rs := byChat[owner]
			lines := []string{head}
			if len(rs) > 0 {
				lines = append(lines, h.reminderLines(rs, now))
			}
			if len(skipped) > 0 {
				lines = append(lines, h.skippedLines(skipped, now), runHint(skipped))
			}
			h.sendBacklog(ctx, owner, strings.Join(lines, "\n"), rs, now)
		}
	}
	for _, k := range order {
		if k != owner {
			h.sendBacklog(ctx, k, head+"\n"+h.reminderLines(byChat[k], now), byChat[k], now)
		}
	}
}

// sendBacklog delivers a summary and settles the reminders it lists.
func (h *Heartbeat) sendBacklog(ctx context.Context, chatKey, text string, rs []memory.Reminder, now time.Time) {
	key, err := h.deliver(ctx, chatKey, text)
	if err != nil {
		h.log.Warn("backlog not delivered", "chat", chatKey, "err", err)
	}
	for _, r := range rs {
		if err != nil {
			h.undelivered(ctx, r, key, err, now)
			continue
		}
		_ = h.store.MarkFired(ctx, r.ID)
		h.store.Audit(ctx, "reminder.fired", key, r.Text)
	}
}

// tellMissed lists for the owner routines that fell due so long ago (the lid
// shut all weekend) that running them now makes no sense.
func (h *Heartbeat) tellMissed(ctx context.Context, w why, missed []skip, now time.Time) {
	owner := h.owner()
	if owner == "" {
		h.log.Warn("no owner chat; nobody told about missed protocols")
		return
	}
	text := h.while(w) + "\n" + h.skippedLines(missed, now) + "\n" + runHint(missed)
	if _, err := h.deliver(ctx, owner, text); err != nil {
		h.log.Warn("missed protocols not told", "err", err)
	}
	for _, m := range missed {
		h.store.Audit(ctx, "protocol.missed", owner, fmt.Sprintf("%s: due %s, last %s", m.Name, timesText(m.Count), m.Last.Format(time.RFC3339)))
	}
}

// machine is what the twin runs on, as the owner would say it.
func machine() string {
	if runtime.GOOS == "darwin" {
		return "the Mac"
	}
	return "the computer"
}

// causeText is why something is late, in words: "the Mac was asleep".
func causeText(w why) string {
	switch w {
	case whyAsleep:
		return machine() + " was asleep"
	case whyStopped:
		return "I wasn't running"
	case whyPaused:
		return "I was paused"
	}
	return ""
}

// while heads a list of what came due for one reason.
func (h *Heartbeat) while(w why) string {
	if w == whyLate {
		return "These are late:"
	}
	return "While " + causeText(w) + ":"
}

// same is the reason shared by all of ws, or whyLate when they differ.
func same(ws []why) why {
	for _, w := range ws[1:] {
		if w != ws[0] {
			return whyLate
		}
	}
	return ws[0]
}

// reminderLines lists reminders, each with the time it was due.
func (h *Heartbeat) reminderLines(rs []memory.Reminder, now time.Time) string {
	lines := make([]string, len(rs))
	for i, r := range rs {
		lines[i] = fmt.Sprintf("• Reminder, due %s: %s", h.when(r.DueAt, now), r.Text)
	}
	return strings.Join(lines, "\n")
}

// skippedLines lists routines that didn't run.
func (h *Heartbeat) skippedLines(ss []skip, now time.Time) string {
	lines := make([]string, len(ss))
	for i, s := range ss {
		due := "due " + h.when(s.Last, now)
		if s.Count > 1 {
			due = fmt.Sprintf("due %s, the last %s", timesText(s.Count), h.when(s.Last, now))
		}
		lines[i] = fmt.Sprintf("• %s didn't run (%s)", capitalize(s.Name), due)
	}
	return strings.Join(lines, "\n")
}

// runHint says how to have a skipped routine run now.
func runHint(ss []skip) string {
	if len(ss) == 1 {
		return fmt.Sprintf("Say \"run %s\" if you still want it.", ss[0].Name)
	}
	return fmt.Sprintf("Say \"run %s\" (or another of them) if you still want one.", ss[len(ss)-1].Name)
}

// lateNote heads what a late run sends, saying why it's late.
func (h *Heartbeat) lateNote(l lateRun) string {
	now := h.now()
	due := h.when(l.due, now)
	once := ""
	if l.missed > 1 {
		due = fmt.Sprintf("%s, the last %s", timesText(l.missed), due)
		once = "; here it is once"
	}
	if c := causeText(l.why); c != "" {
		return fmt.Sprintf("(Late — %s when this was due %s%s.)", c, due, once)
	}
	return fmt.Sprintf("(Late — this was due %s%s.)", due, once)
}

// lateTask tells the model a run is late, so it can say nothing when the
// routine makes no sense this late.
func (h *Heartbeat) lateTask(l lateRun) string {
	now := h.now()
	why := ""
	if c := causeText(l.why); c != "" {
		why = ", but " + c
	}
	return fmt.Sprintf("[This run is late: it was due %s%s, and it's %s now. Do it as it makes sense at this hour. If it no longer makes sense this late (a wake-up call, say), reply exactly NOTHING_TO_REPORT.]",
		h.when(l.due, now), why, now.In(h.Location()).Format("15:04"))
}
