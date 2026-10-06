package agent

import (
	"context"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// Every model call is metered, per model and kind of work, in the owner's
// day; before, token counts were only written to a debug log.
func TestModelUsageIsRecorded(t *testing.T) {
	withTokens := func(r llm.Response, in, out, cr, cw int64) llm.Response {
		r.InputTokens, r.OutputTokens, r.CacheRead, r.CacheWrite = in, out, cr, cw
		return r
	}
	fp := &fakeProvider{script: []llm.Response{
		withTokens(toolUse("t1", "echo", `{"s":"hi"}`), 1000, 50, 200, 300),
		withTokens(text("It said hi."), 1100, 20, 500, 0),
		withTokens(text("Morning brief."), 700, 90, 0, 0),
	}}
	a, store, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	a.cfg.User.Timezone = "Australia/Sydney"
	ctx := context.Background()
	if _, err := a.Handle(ctx, "test:1", "say hi"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunTask(ctx, "test:1#protocol-brief-1", "brief me"); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Australia/Sydney")
	day := time.Now().In(loc).Format(memory.DayFormat)
	rows, err := store.Usage(ctx, day, day)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]memory.UsageRow{}
	for _, r := range rows {
		got[r.Kind] = r
	}
	chat, bg := got["chat"], got["protocol"]
	if chat.Model != "fake" || chat.Calls != 2 || chat.Input != 2100 || chat.Output != 70 || chat.CacheRead != 700 || chat.CacheWrite != 300 {
		t.Fatalf("chat usage %+v", chat)
	}
	if bg.Calls != 1 || bg.Input != 700 || bg.Output != 90 {
		t.Fatalf("protocol usage %+v (all: %+v)", bg, rows)
	}
}

// A call that reports no tokens (a fake or a local server that doesn't say)
// adds no row.
func TestNoTokensNoRow(t *testing.T) {
	a, store, _ := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	if _, err := a.Handle(context.Background(), "test:1", "hello"); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.Usage(context.Background(), "0000-00-00", "9999-99-99")
	if len(rows) != 0 {
		t.Fatalf("rows %+v", rows)
	}
}

// Regression (observability merged with time's zone that travels): usage
// was kept under the day in the zone the process started in, while the twin
// reads "today" and "this month" in the zone it is in now (Location), so
// after a flight today's spend read short. It goes by the live zone.
func TestUsageIsKeptUnderTheDayWhereTheOwnerIsNow(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{func() llm.Response { r := text("Hi."); r.InputTokens, r.OutputTokens = 100, 10; return r }()}}
	a, store, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	a.cfg.User.Timezone = "auto" // follows the system
	// A zone whose date differs from the process's own for most of the day.
	now := time.Now()
	here := time.FixedZone("UTC+14", 14*3600)
	if now.In(here).Format(memory.DayFormat) == now.In(time.Local).Format(memory.DayFormat) {
		here = time.FixedZone("UTC-12", -12*3600)
	}
	a.Location = func() *time.Location { return here }
	if _, err := a.Handle(context.Background(), "test:1", "hello"); err != nil {
		t.Fatal(err)
	}
	day := time.Now().In(here).Format(memory.DayFormat)
	rows, err := store.Usage(context.Background(), day, day)
	if err != nil || len(rows) != 1 || rows[0].Input != 100 {
		all, _ := store.Usage(context.Background(), "0000-00-00", "9999-99-99")
		t.Fatalf("want today's row under %s, got %+v (all %+v, %v)", day, rows, all, err)
	}
}
