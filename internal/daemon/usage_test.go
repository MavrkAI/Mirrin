package daemon

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// A conversation's model use reaches Spend, priced from the owner's config,
// and Health warns as the monthly budget runs out.
func TestSpendAndTheBudgetWarning(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response {
		r := say("Done.")
		r.InputTokens, r.OutputTokens = 1_000_000, 100_000
		return r
	})
	ctx := context.Background()
	if err := td.UpdateConfig(func(c *config.Config) {
		c.Usage.MonthlyBudget = 10
		c.Usage.Prices = map[string]config.ModelPrice{"fake": {Input: 5, Output: 25}}
	}); err != nil {
		t.Fatal(err)
	}
	td.owner(t, "hello")
	sp, err := td.Spend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(sp.Today-7.5) > 1e-9 || math.Abs(sp.ToDate-7.5) > 1e-9 || sp.Budget != 10 || sp.TodayCalls != 1 || !sp.Estimate {
		t.Fatalf("spend %+v", sp)
	}
	if r := firstRunResult(t, td.RunHealth(ctx), "spend"); r.State != health.OK || !strings.Contains(r.Detail, "US$7.50 of your US$10.00 monthly budget") {
		t.Fatalf("under 80%%: %+v", r)
	}
	td.owner(t, "again")
	r := firstRunResult(t, td.RunHealth(ctx), "spend")
	if r.State != health.Warn || !strings.Contains(r.Detail, "over your US$10.00 monthly budget: US$15.00") || !strings.Contains(r.Fix, "mirrin usage") {
		t.Fatalf("over budget: %+v", r)
	}
}

// The owner hears once a month at 80% of the budget and once when it is
// passed, on the desktop, whether the line was crossed while the twin ran
// or before it started.
func TestBudgetNotices(t *testing.T) {
	var shown []string
	prev := desktopNotify
	desktopNotify = func(_, body string) error { shown = append(shown, body); return nil }
	t.Cleanup(func() { desktopNotify = prev })
	td := newTestDaemon(t, func(string, llm.Request) llm.Response {
		r := say("Done.")
		r.InputTokens, r.OutputTokens = 1_000_000, 100_000 // US$7.50 at the prices below
		return r
	})
	ctx := context.Background()
	if err := td.UpdateConfig(func(c *config.Config) {
		c.Usage.MonthlyBudget = 9
		c.Usage.Prices = map[string]config.ModelPrice{"fake": {Input: 5, Output: 25}}
	}); err != nil {
		t.Fatal(err)
	}
	td.RunHealth(ctx)
	if len(shown) != 0 {
		t.Fatalf("nothing used yet: %q", shown)
	}
	td.owner(t, "hello") // 83%
	td.RunHealth(ctx)
	td.RunHealth(ctx)
	if len(shown) != 1 || !strings.Contains(shown[0], "about US$7.50 of your US$9.00 monthly budget (estimate)") || !strings.Contains(shown[0], "mirrin usage") {
		t.Fatalf("at 80%% (once, and not the general self-check notice as well): %q", shown)
	}
	td.owner(t, "again") // 167%
	td.RunHealth(ctx)
	td.RunHealth(ctx)
	if len(shown) != 2 || !strings.Contains(shown[1], "passed your US$9.00 monthly budget: about US$15.00 so far this month") {
		t.Fatalf("over the budget: %q", shown)
	}

	// A twin that starts when the month is already over budget says so once,
	// and not the 80% notice as well.
	shown = nil
	month := time.Now().In(td.loc).Format("2006-01")
	for _, lvl := range []string{"80", "100"} {
		if err := td.store.Set(ctx, "usage.notified."+month+"."+lvl, ""); err != nil {
			t.Fatal(err)
		}
	}
	td.health = nil // a new start
	td.RunHealth(ctx)
	td.RunHealth(ctx)
	if len(shown) != 1 || !strings.Contains(shown[0], "passed your US$9.00") {
		t.Fatalf("already over at start: %q", shown)
	}

	// A one-off check (doctor, a problem report) never pops up a notice.
	shown = nil
	_ = td.store.Set(ctx, "usage.notified."+month+".100", "")
	run := td.runCtx
	td.runCtx, td.health = nil, nil
	td.RunHealth(ctx)
	td.runCtx = run
	if len(shown) != 0 {
		t.Fatalf("a one-off check gave a notice: %q", shown)
	}
}

func TestSpendWording(t *testing.T) {
	cases := []struct {
		sp     memory.Spend
		state  health.State
		detail string
	}{
		{memory.Spend{}, health.OK, "nothing used this month"},
		{memory.Spend{MonthCalls: 3, Today: 0.004, ToDate: 1.2}, health.OK, "under US$0.01 today, US$1.20 this month (estimate)"},
		{memory.Spend{MonthCalls: 3, ToDate: 21, Today: 0.5, Budget: 25, Used: 0.84}, health.Warn, "84% of your US$25.00 monthly budget used: US$21.00 so far, US$0.50 today (estimate)"},
		{memory.Spend{MonthCalls: 3, ToDate: 26, Budget: 25, Used: 1.04, Unpriced: []string{"custom/x"}}, health.Warn, "over your US$25.00 monthly budget: US$26.00 so far this month (estimate); no price known for custom/x"},
		{memory.Spend{MonthCalls: 3, ToDate: 2, Today: 1, Budget: 25, Used: 0.08}, health.OK, "US$2.00 of your US$25.00 monthly budget, US$1.00 today (estimate)"},
		{memory.Spend{MonthCalls: 3, ToDate: 1, Unpriced: []string{"custom/x"}}, health.OK, "no price known for custom/x"},
	}
	for _, c := range cases {
		state, detail, _ := spendState(c.sp)
		if state != c.state || !strings.Contains(detail, c.detail) {
			t.Errorf("%+v: got %s %q", c.sp, state, detail)
		}
	}
}

func TestTidyMemoryFollowsTheConfig(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	ctx := context.Background()
	td.store.Audit(ctx, "tool.ok", ownerKey, "recent")
	old := time.Now().AddDate(0, 0, -100)
	if err := td.UpdateConfig(func(c *config.Config) { c.Retention.AuditDays = 30 }); err != nil {
		t.Fatal(err)
	}
	insertAuditAt(t, td.Daemon, old, "old entry")
	td.tidyMemory(ctx)
	es, _ := td.store.RecentAudit(ctx, 50)
	for _, e := range es {
		if e.Detail == "old entry" {
			t.Fatal("an entry past the retention period was kept")
		}
	}
	if len(es) == 0 {
		t.Fatal("recent entries must stay")
	}
}

// insertAuditAt writes an audit entry with a past timestamp, as an old
// install would have.
func insertAuditAt(t *testing.T, d *Daemon, at time.Time, detail string) {
	t.Helper()
	db, err := sql.Open("sqlite", memory.FileURI(filepath.Join(d.cfg.DataDir, "memory.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO audit(ts, kind, chat_key, detail) VALUES(?,?,?,?)`, at.UTC().Format(time.RFC3339), "tool.ok", "c", detail); err != nil {
		t.Fatal(err)
	}
}

// Regression (google merged with observability): Google signing the twin
// out is told to the owner by googleSignedOut, from the self-check itself,
// and then the same round's general "Self-check" notice said it again. It is
// one notice, as with spending; other Google trouble still gets the general
// one.
func TestAGoogleSignOutIsOneNotice(t *testing.T) {
	td := newTestDaemon(t, butler)
	var shown []string
	prev := desktopNotify
	desktopNotify = func(_, body string) error { shown = append(shown, body); return nil }
	t.Cleanup(func() { desktopNotify = prev })
	m := td.newHealth()
	ok := health.Report{Results: []health.Result{{Name: "google", State: health.OK}, {Name: "spend", State: health.OK}}}
	signedOut := health.Report{Results: []health.Result{{Name: "google", State: health.Fail, Detail: googleSignedOutDetail + " on Sun 27 Sep"}, {Name: "spend", State: health.OK}}}
	m.OnChange(ok, signedOut)
	if len(shown) != 0 {
		t.Fatalf("a sign-out, already told, got the general notice too: %q", shown)
	}
	apisOff := health.Report{Results: []health.Result{{Name: "google", State: health.Fail, Detail: "Calendar isn't switched on"}, {Name: "spend", State: health.OK}}}
	m.OnChange(ok, apisOff)
	if len(shown) != 1 || !strings.Contains(shown[0], "Self-check") {
		t.Fatalf("other Google trouble: %q", shown)
	}
}

// The twin's upkeep is on the heartbeat's wall-clock scheduler, which keeps
// time while a Mac sleeps (heartbeat's TestRecurringUpkeepRunsOnTheWallClock
// shows these schedules run): approvals lapse every 15 minutes, and memory
// retention runs twice a day, no longer on a Go timer that stops in sleep.
func TestUpkeepIsOnTheWallClock(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.scheduleJobs()
	got := td.beat.JobSchedules()
	for _, want := range []string{"@every 15m", tidySchedule} {
		if !slices.Contains(got, want) {
			t.Errorf("upkeep %q isn't scheduled: %q", want, got)
		}
	}
}
