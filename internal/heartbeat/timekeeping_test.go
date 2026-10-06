package heartbeat

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// clock is a settable wall clock (no monotonic reading, as after a sleep the
// two disagree anyway).
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// at is a UTC time on 1 Oct 2030 (a Tuesday), plus days.
func at(days, hour, min int) time.Time {
	return time.Date(2030, 10, 1+days, hour, min, 0, 0, time.UTC)
}

// timed is a heartbeat on a clock the test moves, running due work inline,
// with a runner that records the tasks it was given.
func timed(t *testing.T, start time.Time) (*Heartbeat, *clock, *outbox, *[]string) {
	t.Helper()
	var tasks []string
	var mu sync.Mutex
	h, _, o := setup(t, func(_ context.Context, _, task string) (string, error) {
		mu.Lock()
		tasks = append(tasks, task)
		mu.Unlock()
		return "Good morning. Three meetings today.", nil
	}, "telegram:1")
	c := &clock{t: start}
	h.now = c.now
	h.started = start
	h.spawn = func(f func()) { f() }
	return h, c, o, &tasks
}

var brief = protocols.Protocol{Name: "morning briefing", Schedule: "0 7 * * *", Prompt: "brief me"}

// The regression: after a night with the lid shut the 07:00 brief used to
// arrive hours late with no word of it, or together with everything else.
// It runs once on waking and says why it's late.
func TestRoutineMissedWhileAsleepRunsOnceOnWakeAndSaysSo(t *testing.T) {
	ctx := context.Background()
	h, c, o, tasks := timed(t, at(0, 22, 0))
	if err := h.LoadProtocols([]protocols.Protocol{brief}); err != nil {
		t.Fatal(err)
	}
	h.look(ctx)
	h.nextWait()
	c.set(at(1, 9, 12)) // the lid opens
	h.look(ctx)
	got := o.messages()
	if len(got) != 1 {
		t.Fatalf("want the brief once, got %q", got)
	}
	want := "telegram:1 (Late — " + machine() + " was asleep when this was due at 07:00.)\n\nGood morning."
	if !strings.HasPrefix(got[0], want) {
		t.Fatalf("want %q, got %q", want, got[0])
	}
	if len(*tasks) != 1 || !strings.Contains((*tasks)[0], "This run is late: it was due at 07:00, but "+machine()+" was asleep, and it's 09:12 now") ||
		!strings.Contains((*tasks)[0], "NOTHING_TO_REPORT") {
		t.Fatalf("the model should hear the run is late: %q", *tasks)
	}
	c.set(at(1, 9, 13))
	h.look(ctx)
	if n := len(o.messages()); n != 1 {
		t.Fatalf("a missed run is made up once, got %d messages", n)
	}
}

func TestOnTimeRunHasNoLateNote(t *testing.T) {
	ctx := context.Background()
	h, c, o, tasks := timed(t, at(0, 6, 59))
	_ = h.LoadProtocols([]protocols.Protocol{brief})
	h.look(ctx)
	if len(o.messages()) != 0 {
		t.Fatal("ran before it was due")
	}
	c.set(at(0, 7, 0).Add(300 * time.Millisecond))
	h.look(ctx)
	if got := o.messages(); len(got) != 1 || strings.Contains(got[0], "Late") || strings.Contains((*tasks)[0], "late") {
		t.Fatalf("an on-time run should just run: %q %q", got, *tasks)
	}
}

func TestHourlyRoutineAfterLongSleepRunsOnceNotEightTimes(t *testing.T) {
	ctx := context.Background()
	h, c, o, _ := timed(t, at(0, 23, 30))
	_ = h.LoadProtocols([]protocols.Protocol{{Name: "inbox sweep", Schedule: "0 * * * *", Prompt: "sweep"}})
	h.look(ctx)
	c.set(at(1, 7, 40))
	h.look(ctx)
	got := o.messages()
	if len(got) != 1 || !strings.Contains(got[0], "due 8 times, the last at 07:00; here it is once.") {
		t.Fatalf("want one run covering the 8 missed, got %q", got)
	}
}

func TestRoutineMissedLongAgoIsListedNotRun(t *testing.T) {
	ctx := context.Background()
	h, c, o, tasks := timed(t, at(3, 15, 0)) // Friday afternoon
	_ = h.LoadProtocols([]protocols.Protocol{{Name: "weekly review", Schedule: "0 16 * * 5", Prompt: "review"}})
	h.look(ctx)
	c.set(at(6, 8, 30)) // Monday morning, the lid shut all weekend
	h.look(ctx)
	if len(*tasks) != 0 {
		t.Fatalf("a run 64 hours late should not run by itself: %q", *tasks)
	}
	got := o.messages()
	want := "telegram:1 While " + machine() + " was asleep:\n• Weekly review didn't run (due Fri 4 Oct 16:00)\nSay \"run weekly review\" if you still want it."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("want\n%q\ngot\n%q", want, got)
	}
}

// A run that fell due while the twin wasn't running at all (quit, or the
// computer off) is made up once when it starts, and only once.
func TestRoutineMissedWhileStoppedIsMadeUpOnceAtStart(t *testing.T) {
	ctx := context.Background()
	h, c, _, _ := timed(t, at(0, 22, 0))
	_ = h.LoadProtocols([]protocols.Protocol{brief})
	h.look(ctx)

	// The twin quits; it starts again at 08:10 the next day on the same store.
	h2 := New(h.store, h.run, nil, h.owner, time.UTC, nil)
	o2 := &outbox{down: map[string]bool{}}
	h2.send = o2.send
	h2.now, h2.spawn = c.now, func(f func()) { f() }
	c.set(at(1, 8, 10))
	h2.started = c.now()
	_ = h2.LoadProtocols([]protocols.Protocol{brief})
	h2.look(ctx)
	if got := o2.messages(); len(got) != 1 || !strings.HasPrefix(got[0], "telegram:1 (Late — I wasn't running when this was due at 07:00.)") {
		t.Fatalf("want one late run, got %q", got)
	}

	h3 := New(h.store, h.run, o2.send, h.owner, time.UTC, nil)
	h3.now, h3.spawn = c.now, func(f func()) { f() }
	c.set(at(1, 8, 20))
	h3.started = c.now()
	_ = h3.LoadProtocols([]protocols.Protocol{brief})
	h3.look(ctx)
	if n := len(o2.messages()); n != 1 {
		t.Fatalf("restarting again must not run it again, got %d", n)
	}
}

func TestRescheduledRoutineStartsFresh(t *testing.T) {
	ctx := context.Background()
	h, c, o, _ := timed(t, at(0, 6, 0))
	_ = h.LoadProtocols([]protocols.Protocol{brief})
	c.set(at(0, 8, 0))
	later := brief
	later.Schedule = "0 9 * * *" // moved from 07:00 to 09:00 at 08:00
	_ = h.LoadProtocols([]protocols.Protocol{later})
	h.look(ctx)
	if got := o.messages(); len(got) != 0 {
		t.Fatalf("a new schedule owes nothing from the old one: %q", got)
	}
	c.set(at(0, 9, 0))
	h.look(ctx)
	if got := o.messages(); len(got) != 1 {
		t.Fatalf("want the 09:00 run, got %q", got)
	}
}

func TestReloadNeitherRepeatsNorLosesARun(t *testing.T) {
	ctx := context.Background()
	h, c, o, _ := timed(t, at(0, 6, 59))
	_ = h.LoadProtocols([]protocols.Protocol{brief})
	c.set(at(0, 7, 0))
	h.look(ctx)
	_ = h.LoadProtocols([]protocols.Protocol{brief, {Name: "other", Schedule: "0 12 * * *", Prompt: "x"}})
	c.set(at(0, 7, 1))
	h.look(ctx)
	if n := len(o.messages()); n != 1 {
		t.Fatalf("a reload must not run the brief again, got %d", n)
	}
}

func TestClockSetBackStartsSchedulesFromNow(t *testing.T) {
	ctx := context.Background()
	h, c, o, _ := timed(t, at(0, 10, 0))
	_ = h.LoadProtocols([]protocols.Protocol{{Name: "half nine", Schedule: "30 9 * * *", Prompt: "x"}})
	h.look(ctx)
	c.set(at(0, 9, 0)) // someone set the clock back an hour
	h.look(ctx)
	c.set(at(0, 9, 30))
	h.look(ctx)
	if n := len(o.messages()); n != 1 {
		t.Fatalf("the clock says 09:30, so it runs; got %d messages", n)
	}
}

func TestBuiltInJobCatchesUpOnceQuietly(t *testing.T) {
	ctx := context.Background()
	h, c, _, _ := timed(t, at(0, 1, 0))
	ran := 0
	h.AddJob("15 3 * * *", func(context.Context) { ran++ })
	h.look(ctx)
	c.set(at(2, 9, 0)) // two nights asleep
	h.look(ctx)
	if ran != 1 {
		t.Fatalf("want one catch-up run, got %d", ran)
	}
}

// Reminders that fell due while the lid was shut arrive on waking, in one
// message, each with the time it was due; a single one says why it's late.
func TestRemindersDueDuringSleepArriveTogetherWithTheirTimes(t *testing.T) {
	ctx := context.Background()
	h, c, o, _ := timed(t, at(0, 23, 0))
	h.look(ctx)
	_, _ = h.store.AddReminder(ctx, "telegram:1", at(1, 7, 0), "take your pills")
	_, _ = h.store.AddReminder(ctx, "telegram:1", at(1, 8, 15), "call the dentist")
	_, _ = h.store.AddReminder(ctx, "whatsapp:2", at(1, 7, 30), "water the plants")
	c.set(at(1, 9, 0))
	h.look(ctx)
	got := o.messages()
	want := []string{
		"telegram:1 While " + machine() + " was asleep:\n• Reminder, due at 07:00: take your pills\n• Reminder, due at 08:15: call the dentist",
		"whatsapp:2 Reminder (due at 07:30 — " + machine() + " was asleep): water the plants",
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("want\n%q\ngot\n%q", want, got)
	}
	if due, _ := h.store.DueReminders(ctx, at(2, 0, 0)); len(due) != 0 {
		t.Fatalf("all three should be marked fired, %d left", len(due))
	}
}

// The regression: resuming used to fire every queued reminder at once, with
// no word about routines that were skipped.
func TestResumeSummarisesTheBacklogInOneMessage(t *testing.T) {
	ctx := context.Background()
	h, c, o, tasks := timed(t, at(0, 13, 0))
	_ = h.LoadProtocols([]protocols.Protocol{{Name: "afternoon check", Schedule: "0 14 * * *", Prompt: "check"}})
	h.look(ctx)
	h.SetPaused(true)
	_, _ = h.store.AddReminder(ctx, "telegram:1", at(0, 14, 30), "leave for the airport")
	_, _ = h.store.AddReminder(ctx, "telegram:1", at(0, 15, 0), "call mum")
	for _, m := range []int{0, 20, 40} {
		c.set(at(0, 14, m).Add(time.Minute))
		h.look(ctx)
	}
	c.set(at(0, 15, 5))
	h.look(ctx)
	if len(o.messages()) != 0 || len(*tasks) != 0 {
		t.Fatalf("nothing runs or goes out while paused: %q %q", o.messages(), *tasks)
	}
	h.SetPaused(false)
	c.set(at(0, 15, 6))
	h.look(ctx)
	got := o.messages()
	want := "telegram:1 While I was paused (since 13:00):\n• Reminder, due at 14:30: leave for the airport\n• Reminder, due at 15:00: call mum\n• Afternoon check didn't run (due at 14:00)\nSay \"run afternoon check\" if you still want it."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("want\n%q\ngot\n%q", want, got)
	}
	if len(*tasks) != 0 {
		t.Fatal("a routine skipped while paused is not run on resume")
	}
	c.set(at(0, 15, 7))
	h.look(ctx)
	if n := len(o.messages()); n != 1 {
		t.Fatalf("the reminders in the summary must not fire again, got %d messages", n)
	}
}

func TestSkippedWhilePausedSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	h, c, _, _ := timed(t, at(0, 13, 0))
	_ = h.LoadProtocols([]protocols.Protocol{{Name: "afternoon check", Schedule: "0 14 * * *", Prompt: "check"}})
	h.PauseSince(at(0, 13, 0))
	c.set(at(0, 14, 1))
	h.look(ctx)

	o2 := &outbox{down: map[string]bool{}}
	h2 := New(h.store, h.run, o2.send, h.owner, time.UTC, nil)
	h2.now, h2.spawn = c.now, func(f func()) { f() }
	h2.PauseSince(at(0, 13, 0)) // as the daemon restores it
	h2.restoreSkipped(ctx)
	h2.SetPaused(false)
	h2.look(ctx)
	if got := o2.messages(); len(got) != 1 || !strings.Contains(got[0], "Afternoon check didn't run (due at 14:00)") {
		t.Fatalf("want the skip remembered across the restart, got %q", got)
	}
}

func TestResumeWithNothingMissedSaysNothing(t *testing.T) {
	ctx := context.Background()
	h, c, o, _ := timed(t, at(0, 13, 0))
	h.SetPaused(true)
	c.set(at(0, 13, 10))
	h.SetPaused(false)
	h.look(ctx)
	if got := o.messages(); len(got) != 0 {
		t.Fatalf("nothing waited, so nothing to say: %q", got)
	}
}

// zoned is a heartbeat that follows a system zone the test changes.
func zoned(t *testing.T, sys *string) (*Heartbeat, *outbox) {
	t.Helper()
	h, _, o := setup(t, func(context.Context, string, string) (string, error) {
		return "Good morning. Three meetings today.", nil
	}, "telegram:1")
	var mu sync.Mutex
	h.zone = newZone(nil, func() string { mu.Lock(); defer mu.Unlock(); return *sys })
	h.now = func() time.Time { return at(0, 12, 0) }
	h.spawn = func(f func()) { f() }
	return h, o
}

// The regression: the zone was frozen at install, so in London "remind me
// at 9" meant 9 in Melbourne. It follows the system now and says so.
func TestZoneFollowsTheSystemAndTellsTheOwner(t *testing.T) {
	ctx := context.Background()
	sys := "Australia/Melbourne"
	h, o := zoned(t, &sys)
	var heard []string
	h.OnZone = func(l *time.Location) { heard = append(heard, l.String()) }
	_, _ = h.store.AddReminder(ctx, "telegram:1", at(0, 20, 0), "call mum") // 06:00 tomorrow in Melbourne
	h.look(ctx)
	if got := o.messages(); len(got) != 0 || h.Location().String() != "Australia/Melbourne" {
		t.Fatalf("the first look only notes the zone: %q %s", got, h.Location())
	}
	sys = "Europe/London"
	h.look(ctx)
	if h.Location().String() != "Europe/London" || len(heard) != 1 || heard[0] != "Europe/London" {
		t.Fatalf("want London time, got %s (told %v)", h.Location(), heard)
	}
	got := o.messages()
	want := "telegram:1 You're in London now; reminders and routines follow local time. The reminder you'd already set keeps its moment: \"call mum\" is at 21:00 here. Ask me if you'd like it moved."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("want\n%q\ngot\n%q", want, got)
	}
	h.look(ctx)
	if n := len(o.messages()); n != 1 {
		t.Fatal("told more than once")
	}
}

func TestZoneMoveWhileStoppedIsToldAtStart(t *testing.T) {
	ctx := context.Background()
	sys := "America/New_York"
	h, o := zoned(t, &sys)
	_ = h.store.Set(ctx, zoneKey, "Asia/Tokyo")
	h.look(ctx)
	if got := o.messages(); len(got) != 1 || !strings.HasPrefix(got[0], "telegram:1 You're in New York now;") {
		t.Fatalf("want the move told, got %q", got)
	}
}

func TestPinnedZoneStaysAndSaysSoOnce(t *testing.T) {
	ctx := context.Background()
	sys := "Australia/Melbourne"
	h, o := zoned(t, &sys)
	mel, _ := time.LoadLocation("Australia/Melbourne")
	h.zone = newZone(mel, h.zone.system)
	h.look(ctx)
	sys = "Europe/London"
	h.look(ctx)
	if h.Location().String() != "Australia/Melbourne" {
		t.Fatalf("a pinned zone stays: %s", h.Location())
	}
	got := o.messages()
	want := "telegram:1 " + capitalize(machine()) + " is on London time now, but I'm keeping to Melbourne time, as your settings say. To follow " + machine() + " instead, set timezone to Local in config.yaml, then choose Restart from my menu."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("want\n%q\ngot\n%q", want, got)
	}
	sys = "Australia/Melbourne"
	h.look(ctx)
	sys = "Europe/London"
	h.look(ctx)
	if n := len(o.messages()); n != 1 {
		t.Fatalf("told again about the same zone: %q", o.messages())
	}
}

func TestZoneSettingIsReadAgain(t *testing.T) {
	ctx := context.Background()
	sys := "Australia/Melbourne"
	h, o := zoned(t, &sys)
	setting := "Local"
	h.ZoneSetting(func() string { return setting })
	h.look(ctx)
	setting = "Asia/Tokyo"
	h.look(ctx)
	if h.Location().String() != "Asia/Tokyo" {
		t.Fatalf("want the pinned zone, got %s", h.Location())
	}
	setting = "Local"
	h.look(ctx)
	if h.Location().String() != "Australia/Melbourne" {
		t.Fatalf("want the system zone back, got %s", h.Location())
	}
	if got := o.messages(); len(got) != 0 {
		t.Fatalf("a setting the owner made needs no announcement: %q", got)
	}
}

func TestScheduleFollowsTheZone(t *testing.T) {
	ctx := context.Background()
	sys := "Australia/Melbourne"
	h, o := zoned(t, &sys)
	c := &clock{t: time.Date(2030, 10, 1, 5, 0, 0, 0, time.UTC)} // 15:00 in Melbourne, 06:00 in London
	h.now, h.started = c.now, c.now()
	_ = h.LoadProtocols([]protocols.Protocol{brief})
	h.look(ctx)
	sys = "Europe/London"
	c.set(time.Date(2030, 10, 1, 6, 0, 0, 0, time.UTC)) // 07:00 in London (BST)
	h.look(ctx)
	got := o.messages()
	if len(got) != 2 || !strings.Contains(got[1], "Three meetings") {
		t.Fatalf("want the brief at 07:00 London time, got %q", got)
	}
}

// The scheduler itself: it looks at the clock on its own and runs what's due.
func TestSchedulerLoopRunsWhatIsDue(t *testing.T) {
	ran := make(chan string, 8)
	h, _, _ := setup(t, func(_ context.Context, _, task string) (string, error) {
		select {
		case ran <- task:
		default:
		}
		return "NOTHING_TO_REPORT", nil
	}, "c:1")
	h.tick, h.first = 20*time.Millisecond, 10*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Start(ctx)
	_ = h.LoadProtocols([]protocols.Protocol{{Name: "often", Schedule: "@every 1s", Prompt: "tick"}})
	select {
	case task := <-ran:
		if !strings.Contains(task, `Protocol "often"`) {
			t.Fatalf("ran %q", task)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler never ran a protocol due every second")
	}
}

func TestReEnabledRoutineOwesNothingFromWhileItWasOff(t *testing.T) {
	ctx := context.Background()
	h, c, o, _ := timed(t, at(0, 6, 0))
	_ = h.LoadProtocols([]protocols.Protocol{brief})
	h.look(ctx)
	c.set(at(0, 6, 30))
	_ = h.LoadProtocols(nil) // turned off
	h.look(ctx)
	c.set(at(2, 9, 0))
	_ = h.LoadProtocols([]protocols.Protocol{brief}) // back on two days later
	h.look(ctx)
	if got := o.messages(); len(got) != 0 {
		t.Fatalf("a routine that was off owes nothing: %q", got)
	}
}

func TestLongStopListsWhatWasMissedOnce(t *testing.T) {
	ctx := context.Background()
	h, c, _, _ := timed(t, at(0, 22, 0))
	_ = h.LoadProtocols([]protocols.Protocol{brief})
	h.look(ctx)

	o2 := &outbox{down: map[string]bool{}}
	h2 := New(h.store, h.run, o2.send, h.owner, time.UTC, nil)
	h2.now, h2.spawn = c.now, func(f func()) { f() }
	c.set(at(3, 20, 0)) // three days off; the last brief was due at 07:00, 13 hours ago
	h2.started = c.now()
	_ = h2.LoadProtocols([]protocols.Protocol{brief})
	h2.look(ctx)
	want := "telegram:1 While I wasn't running:\n• Morning briefing didn't run (due 3 times, the last at 07:00)\nSay \"run morning briefing\" if you still want it."
	if got := o2.messages(); len(got) != 1 || got[0] != want {
		t.Fatalf("want\n%q\ngot\n%q", want, got)
	}
}

// The regression: counting stopped at 1000 missed runs, and the 1000th was
// taken for the last, so a five-minutely routine after four days asleep was
// listed as "didn't run (due 1000 times, the last Fri 4 Oct 21:20)" and not
// run, though its last run was due two minutes before the lid opened.
func TestFrequentRoutineAfterDaysAsleepRunsOnceWithTheRightTime(t *testing.T) {
	ctx := context.Background()
	h, c, o, tasks := timed(t, at(0, 10, 0))
	_ = h.LoadProtocols([]protocols.Protocol{{Name: "inbox", Schedule: "*/5 * * * *", Prompt: "check"}})
	h.look(ctx)
	h.nextWait()
	c.set(at(4, 10, 2)) // Saturday 10:02; the last run was due at 10:00
	h.look(ctx)
	got := o.messages()
	if len(got) != 1 || len(*tasks) != 1 || strings.Contains(got[0], "didn't run") || strings.Contains(got[0], "Late") {
		t.Fatalf("want one run, as good as on time, got %q (ran %d)", got, len(*tasks))
	}
}

func TestRoutineMissedManyTimesLongAgoSaysManyAndTheRealLastTime(t *testing.T) {
	ctx := context.Background()
	h, c, o, tasks := timed(t, at(0, 12, 0))
	// Every minute from midnight to ten: 600 runs a day.
	_ = h.LoadProtocols([]protocols.Protocol{{Name: "early check", Schedule: "* 0-9 * * *", Prompt: "check"}})
	h.look(ctx)
	h.nextWait()
	c.set(at(4, 22, 0)) // four days on; the last run was due at 09:59
	h.look(ctx)
	if len(*tasks) != 0 {
		t.Fatalf("a run 12 hours late is listed, not run: %q", *tasks)
	}
	want := "telegram:1 While " + machine() + " was asleep:\n• Early check didn't run (due many times, the last at 09:59)\nSay \"run early check\" if you still want it."
	if got := o.messages(); len(got) != 1 || got[0] != want {
		t.Fatalf("want\n%q\ngot\n%q", want, got)
	}
}

func TestLastDueFindsTheLastRunBeforeNow(t *testing.T) {
	for _, tc := range []struct {
		spec     string
		after    time.Time
		now      time.Time
		wantLast time.Time
	}{
		{"*/5 * * * *", at(0, 10, 5), at(4, 10, 2), at(4, 10, 0)},
		{"* 0-9 * * *", at(0, 0, 0), at(4, 22, 0), at(4, 9, 59)},
		{"0 7 * * *", at(0, 7, 0), at(9, 7, 0), at(9, 7, 0)},
		{"*/5 * * * *", at(0, 10, 5), at(4, 10, 2).Add(30 * time.Second), at(4, 10, 0)},
	} {
		sched, err := cron.ParseStandard(tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := lastDue(sched, tc.after, tc.now); !got.Equal(tc.wantLast) {
			t.Errorf("%s up to %s: got %s, want %s", tc.spec, tc.now, got, tc.wantLast)
		}
	}
}

// The regression: built-in jobs caught up at any lateness, so the 17:00 note
// about the owner's week could arrive at 01:00 when the lid opened.
func TestBuiltInJobThatMessagesTheOwnerSkipsAStaleRun(t *testing.T) {
	ctx := context.Background()
	h, c, _, _ := timed(t, at(0, 16, 0))
	ran := 0
	h.AddJobWithin("0 17 * * *", 3*time.Hour, func(context.Context) { ran++ })
	h.look(ctx)
	c.set(at(1, 1, 0)) // the lid opens at 01:00, eight hours after 17:00
	h.look(ctx)
	if ran != 0 {
		t.Fatal("a nudge eight hours late should be skipped, not sent at 01:00")
	}
	c.set(at(1, 16, 0))
	h.look(ctx)
	c.set(at(1, 18, 30)) // slept through 17:00, but only by an hour and a half
	h.look(ctx)
	if ran != 1 {
		t.Fatalf("a nudge a little late still goes; ran %d", ran)
	}
}

// The regression: when a grouped late-reminder message failed, the retries
// went one by one, a burst after the outage.
func TestLateRemindersRetriedAfterAnOutageStillArriveTogether(t *testing.T) {
	ctx := context.Background()
	h, c, o, _ := timed(t, at(0, 23, 0))
	h.look(ctx)
	_, _ = h.store.AddReminder(ctx, "telegram:1", at(1, 7, 0), "take your pills")
	_, _ = h.store.AddReminder(ctx, "telegram:1", at(1, 8, 15), "call the dentist")
	o.down["telegram:1"] = true
	c.set(at(1, 9, 0))
	h.look(ctx)
	if len(o.messages()) != 0 {
		t.Fatal("the channel was down")
	}
	delete(o.down, "telegram:1")
	c.set(at(1, 9, 2)) // after the first retry wait
	h.look(ctx)
	got := o.messages()
	want := "telegram:1 While " + machine() + " was asleep:\n• Reminder, due at 07:00: take your pills\n• Reminder, due at 08:15: call the dentist"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("want one message with both\n%q\ngot\n%q", want, got)
	}
}

// The regression: every delivery ran on the scheduler's goroutine with no
// time limit, so one channel that hung held up every routine.
func TestAHungChannelDoesntHoldUpTheSchedule(t *testing.T) {
	ctx := context.Background()
	h, c, o, tasks := timed(t, at(0, 6, 59))
	old := sendTimeout
	sendTimeout = 50 * time.Millisecond
	defer func() { sendTimeout = old }()
	release := make(chan struct{})
	defer close(release)
	h.send = func(ctx context.Context, key, text string) error {
		if key == "whatsapp:2" {
			<-release // a transport that ignores its context
			return nil
		}
		return o.send(ctx, key, text)
	}
	_ = h.LoadProtocols([]protocols.Protocol{brief})
	_, _ = h.store.AddReminder(ctx, "whatsapp:2", at(0, 7, 0), "water the plants")
	c.set(at(0, 7, 0))
	done := make(chan struct{})
	go func() { h.look(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler waited on a hung channel")
	}
	got := o.messages()
	if len(*tasks) != 1 || len(got) != 2 || got[1] != "telegram:1 Reminder: water the plants" {
		t.Fatalf("want the reminder on the owner's chat and the brief run, got %q (ran %d)", got, len(*tasks))
	}
}

// The regression: a paused twin announced a new time zone on its own, and a
// move whose message failed was never told.
func TestZoneMoveWaitsForResumeAndForAChannel(t *testing.T) {
	ctx := context.Background()
	sys := "Australia/Melbourne"
	h, o := zoned(t, &sys)
	c := &clock{t: at(0, 12, 0)}
	h.now = c.now
	h.look(ctx)
	h.SetPaused(true)
	sys = "Europe/London"
	h.look(ctx)
	if got := o.messages(); len(got) != 0 {
		t.Fatalf("a paused twin says nothing of its own accord: %q", got)
	}
	if h.Location().String() != "Europe/London" {
		t.Fatalf("the zone is followed even while paused, got %s", h.Location())
	}
	o.down["telegram:1"] = true
	h.SetPaused(false)
	h.look(ctx)
	if got := o.messages(); len(got) != 0 {
		t.Fatal("the channel was down")
	}
	delete(o.down, "telegram:1")
	c.set(at(0, 12, 1))
	h.look(ctx)
	if got := o.messages(); len(got) != 0 {
		t.Fatalf("a failed notice waits a little before trying again: %q", got)
	}
	c.set(at(0, 12, 10))
	h.look(ctx)
	h.look(ctx)
	if got := o.messages(); len(got) != 1 || !strings.HasPrefix(got[0], "telegram:1 You're in London now;") {
		t.Fatalf("want the move told once, got %q", got)
	}
}

// The daemon's upkeep runs on this wall-clock scheduler (approvals' lapse
// sweep every 15 minutes, memory retention twice a day): a recurring job
// runs as its schedule says while awake, once on waking however many runs
// the sleep swallowed, and not while paused.
func TestRecurringUpkeepRunsOnTheWallClock(t *testing.T) {
	ctx := context.Background()
	h, c, _, _ := timed(t, at(0, 1, 0))
	sweeps, tidies := 0, 0
	h.AddJob("@every 15m", func(context.Context) { sweeps++ })
	h.AddJob("17 3,15 * * *", func(context.Context) { tidies++ })
	for m := 5; m <= 60; m += 5 {
		c.set(at(0, 1, m))
		h.look(ctx)
	}
	if sweeps != 4 {
		t.Fatalf("an hour awake: %d sweeps, want 4", sweeps)
	}
	c.set(at(0, 3, 20))
	h.look(ctx)
	if tidies != 1 {
		t.Fatalf("03:17 passed: %d tidies", tidies)
	}
	sweeps = 0
	c.set(at(1, 9, 0)) // asleep until the next morning
	h.look(ctx)
	if sweeps != 1 || tidies != 2 {
		t.Fatalf("on waking: %d sweeps, %d tidies; want one catch-up each", sweeps, tidies)
	}
	h.SetPaused(true)
	c.set(at(1, 16, 0))
	h.look(ctx)
	if sweeps != 1 || tidies != 2 {
		t.Fatalf("paused, upkeep ran: %d sweeps, %d tidies", sweeps, tidies)
	}
	if got := h.JobSchedules(); len(got) != 2 || got[0] != "@every 15m" || got[1] != "17 3,15 * * *" {
		t.Fatalf("schedules %q", got)
	}
}
