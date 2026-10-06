package daemon

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/protocols"
	"github.com/MavrkAI/Mirrin/internal/tasks"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

func budgetTestDaemon(t *testing.T) *testDaemon {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("Still here.") })
	if err := td.UpdateConfig(func(c *config.Config) {
		c.Usage.MonthlyBudget = 1
		c.Usage.Prices = map[string]config.ModelPrice{"fake": {Input: 1}}
	}); err != nil {
		t.Fatal(err)
	}
	return td
}

func TestHardBudgetPausesBackgroundButOwnerCanTalk(t *testing.T) {
	td := budgetTestDaemon(t)
	ctx := context.Background()
	if err := td.store.RecordUsage(ctx, time.Now().In(td.loc).Format(memory.DayFormat), "fake", "chat", llm.Tokens{Input: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	var notices atomic.Int32
	old := desktopNotify
	desktopNotify = func(_, text string) error {
		if !strings.Contains(text, "usage.monthly_budget") {
			t.Error(text)
		}
		notices.Add(1)
		return nil
	}
	t.Cleanup(func() { desktopNotify = old })
	for _, kind := range []string{"protocol", "watch", "task"} {
		out, err := td.budgetTask(ctx, "telegram:owner#"+kind+"-123", "do something")
		if kind == "task" {
			if !errors.Is(err, tasks.ErrOverBudget) {
				t.Fatalf("task wasn't paused: %v", err)
			}
		} else if err != nil || out != "NOTHING_TO_REPORT" {
			t.Fatalf("%s: %q %v", kind, out, err)
		}
	}
	if notices.Load() != 1 {
		t.Fatalf("notices: %d", notices.Load())
	}
	var stream strings.Builder
	out, err := td.dispatch(ctx, channels.Inbound{Channel: "telegram", ChatID: "owner", IsOwner: true, Text: "hello"}, agent.Events{OnDelta: func(s string) { stream.WriteString(s) }})
	if err != nil || !strings.Contains(out, "Still here.") || !strings.Contains(out, "over your monthly model budget") {
		t.Fatalf("%q %v", out, err)
	}
	if !strings.Contains(stream.String(), "over your monthly model budget") {
		t.Fatal(stream.String())
	}
	if td.paused.Load() {
		t.Fatal("budget must not set the owner's manual pause")
	}
	if err := td.UpdateConfig(func(c *config.Config) { c.Usage.MonthlyBudget = 2 }); err != nil {
		t.Fatal(err)
	}
	out, err = td.budgetTask(ctx, "telegram:owner#protocol-123", "carry on")
	if err != nil || out != "Still here." {
		t.Fatalf("%q %v", out, err)
	}
}

func TestHardBudgetMonthBoundaryAndUnreadableLedger(t *testing.T) {
	td := budgetTestDaemon(t)
	ctx := context.Background()
	oldMonth := time.Now().In(td.loc).AddDate(0, 0, -time.Now().In(td.loc).Day()).Format(memory.DayFormat)
	if err := td.store.RecordUsage(ctx, oldMonth, "fake", "chat", llm.Tokens{Input: 9_000_000}); err != nil {
		t.Fatal(err)
	}
	if blocked, err := td.backgroundOverBudget(ctx); err != nil || blocked {
		t.Fatalf("%v %v", blocked, err)
	}
	td.store.Close()
	if blocked, err := td.backgroundOverBudget(ctx); !blocked || err == nil {
		t.Fatalf("ledger error allowed spending: %v %v", blocked, err)
	}
	_, err := td.budgetTask(ctx, ownerKey+"#task-123", "carry on")
	if err == nil || !strings.Contains(err.Error(), "Couldn't check model spending") || strings.Contains(err.Error(), "used up") {
		t.Fatalf("ledger error misreported as exhausted budget: %v", err)
	}
}

func TestHardBudgetDoesNotWarnStrangers(t *testing.T) {
	td := budgetTestDaemon(t)
	exhaustBudget(t, td)
	out, err := td.dispatch(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "other", Sender: "visitor", Text: "hello"}, agent.Events{})
	if err != nil || out != "I can't help right now. Please contact my owner directly." {
		t.Fatalf("%q %v", out, err)
	}
	if got := td.llm.heard(); len(got) != 0 {
		t.Fatalf("stranger reached model: %v", got)
	}
	if strings.Contains(out, "budget") || strings.Contains(out, "US$") {
		t.Fatal("private spending leaked")
	}
}

func TestHardBudgetPausesWatchSnapshotsAndKeepsManualPause(t *testing.T) {
	td := budgetTestDaemon(t)
	old := desktopNotify
	desktopNotify = func(string, string) error { return nil }
	t.Cleanup(func() { desktopNotify = old })
	if td.backgroundPaused() {
		t.Fatal("unused budget")
	}
	td.paused.Store(true)
	if !td.backgroundPaused() {
		t.Fatal("manual pause lost")
	}
	td.paused.Store(false)
	if err := td.store.RecordUsage(context.Background(), time.Now().In(td.loc).Format(memory.DayFormat), "fake", "chat", llm.Tokens{Input: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if !td.backgroundPaused() {
		t.Fatal("watcher should pause")
	}
	if err := td.UpdateConfig(func(c *config.Config) { c.Usage.MonthlyBudget = 0 }); err != nil {
		t.Fatal(err)
	}
	if td.backgroundPaused() {
		t.Fatal("zero cap should disable enforcement")
	}
}

func TestHardBudgetTaskStaysOnBoard(t *testing.T) {
	td := budgetTestDaemon(t)
	old := desktopNotify
	desktopNotify = func(string, string) error { return nil }
	t.Cleanup(func() { desktopNotify = old })
	ctx := context.Background()
	if err := td.store.RecordUsage(ctx, time.Now().In(td.loc).Format(memory.DayFormat), "fake", "chat", llm.Tokens{Input: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	task, err := td.tasks.Start(ctx, ownerKey, "A saved task", "Look something up")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, snap := range td.tasks.List() {
			if snap.ID == task.ID && snap.Status == "paused" {
				if !strings.Contains(snap.PausedFor, "budget") {
					t.Fatalf("paused for %q, want the budget named", snap.PausedFor)
				}
				time.Sleep(50 * time.Millisecond) // a per-task message would have gone by now
				td.ch.mu.Lock()
				sent := append([]string(nil), td.ch.sent...)
				td.ch.mu.Unlock()
				for _, m := range sent {
					if strings.Contains(m, "ran out of steps") || strings.Contains(m, "retry task") {
						t.Fatalf("the owner was told a false reason: %q", m)
					}
				}
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("over-budget task wasn't saved as paused")
}

func TestHardBudgetWarnsOnTheRequestThatCrossesTheCap(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response {
		r := say("Done.")
		r.InputTokens = 1_000_000
		return r
	})
	if err := td.UpdateConfig(func(c *config.Config) {
		c.Usage.MonthlyBudget = 1
		c.Usage.Prices = map[string]config.ModelPrice{"fake": {Input: 1}}
	}); err != nil {
		t.Fatal(err)
	}
	var stream strings.Builder
	out, err := td.dispatch(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", IsOwner: true, Text: "hello"}, agent.Events{OnDelta: func(s string) { stream.WriteString(s) }})
	if err != nil || !strings.HasPrefix(out, "Done.\n\nYou're over") {
		t.Fatalf("%q %v", out, err)
	}
	if stream.String() != out {
		t.Fatalf("stream %q != reply %q", stream.String(), out)
	}
}

func exhaustBudget(t *testing.T, td *testDaemon) {
	t.Helper()
	if err := td.store.RecordUsage(context.Background(), time.Now().In(td.loc).Format(memory.DayFormat), "fake", "chat", llm.Tokens{Input: 1_000_000}); err != nil {
		t.Fatal(err)
	}
}

func TestFirstLookOverBudgetStillIntroducesItself(t *testing.T) {
	td := budgetTestDaemon(t)
	exhaustBudget(t, td)
	out, err := td.FirstLook(context.Background())
	if err != nil || out != "Still here." || len(td.llm.heard()) != 1 {
		t.Fatalf("%q %v", out, err)
	}
}

func TestRunProtocolToolOverBudgetDistinguishesLiveAndBackground(t *testing.T) {
	td := budgetTestDaemon(t)
	exhaustBudget(t, td)
	old := desktopNotify
	desktopNotify = func(string, string) error { return nil }
	t.Cleanup(func() { desktopNotify = old })
	p := protocols.Protocol{Name: "news", Prompt: "Give me the news."}
	if _, err := protocols.Write(td.Config().ProtocolsDir, p); err != nil {
		t.Fatal(err)
	}
	if err := td.ReloadProtocols(); err != nil {
		t.Fatal(err)
	}
	call := tools.Call{ChatKey: ownerKey, Input: []byte(`{"name":"news"}`)}
	out, err := td.agent.Tools().Run(context.Background(), "run_protocol", call)
	if err != nil || out != "Still here." || len(td.llm.heard()) != 1 {
		t.Fatalf("live: %q %v", out, err)
	}
	call.ChatKey = ownerKey + "#task-123"
	out, err = td.agent.Tools().Run(context.Background(), "run_protocol", call)
	if err == nil || !strings.Contains(err.Error(), "usage.monthly_budget") || strings.Contains(out, "NOTHING_TO_REPORT") || len(td.llm.heard()) != 1 {
		t.Fatalf("background: %q %v", out, err)
	}
}

func TestPortraitOverBudgetDoesNotCallModel(t *testing.T) {
	td := budgetTestDaemon(t)
	exhaustBudget(t, td)
	old := desktopNotify
	desktopNotify = func(string, string) error { return nil }
	t.Cleanup(func() { desktopNotify = old })
	out, err := td.budgetPortrait(context.Background(), ownerKey+"#portrait-123")
	if err != nil || out != "" || len(td.llm.heard()) != 0 {
		t.Fatalf("%q %v", out, err)
	}
}

func TestBudgetWarningDoesNotDecorateHelpOrApprovalReplies(t *testing.T) {
	td := budgetTestDaemon(t)
	exhaustBudget(t, td)
	for _, text := range []string{"/help", "yes 999999"} {
		out, err := td.dispatch(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", IsOwner: true, Text: text}, agent.Events{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "monthly model budget") {
			t.Fatalf("%s: %q", text, out)
		}
	}
}
