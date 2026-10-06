package daemon

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/protocols"
	"github.com/MavrkAI/Mirrin/internal/tasks"
)

const budgetPausedText = "Background work is paused because your monthly model budget is used up. To raise it, open the settings file from the menu bar, change usage.monthly_budget (your monthly limit) and restart me; or wait until next month. I'll still answer you."

// budgetTask skips scheduled work at the cap, so resuming cannot replay a
// backlog. Long tasks use the manager's existing resumable pause condition.
func (d *Daemon) budgetTask(ctx context.Context, key, text string) (string, error) {
	blocked, err := d.backgroundOverBudget(ctx)
	if err != nil {
		return "", err
	}
	if blocked {
		if memory.UsageKind(key) == "task" {
			return "", tasks.ErrOverBudget
		}
		return "NOTHING_TO_REPORT", nil
	}
	return d.stoppableTask(ctx, key, text)
}

// stoppableTask runs background work (a routine, a protocol, the first-week
// tour) so the owner's "stop", or Stop in the menu, cuts it short: it is
// tracked under its scratch conversation, which stopElsewhere reaches. A
// long task keeps its own "cancel task" and resumable pause instead. The
// scratch conversation is let go when the run ends (acquire).
func (d *Daemon) stoppableTask(ctx context.Context, key, text string) (string, error) {
	if memory.UsageKind(key) == "task" || !memory.IsScratch(key) {
		return d.runTask(ctx, key, text)
	}
	c, release := d.acquire(key)
	defer release()
	ctx, run := track(ctx, backgroundLabel(text), false, true, key, c)
	out, err := d.runTask(ctx, key, text)
	if untrack(run) && err != nil { // a stop that came once it had finished changes nothing
		// "OK, I've stopped …" already told the owner: nothing more to
		// report, and no "didn't run this time" or failed audit after it.
		d.log.Info("background run stopped by the owner", "chat", key)
		return "NOTHING_TO_REPORT", nil
	}
	return out, err
}

// backgroundLabel says what a background run is doing, for "OK, I've
// stopped …": the protocol it runs, by name.
func backgroundLabel(text string) string {
	if rest, ok := strings.CutPrefix(text, "Protocol "); ok {
		if name, err := strconv.QuotedPrefix(rest); err == nil {
			if n, err := strconv.Unquote(name); err == nil && n != "" {
				return "running " + quoted(n)
			}
		}
	}
	return "some background work"
}

func (d *Daemon) backgroundOverBudget(ctx context.Context) (bool, error) {
	sp, err := d.Spend(ctx)
	if err != nil {
		return true, fmt.Errorf("Couldn't check model spending. Background work is paused; run mirrin usage and try again.")
	}
	blocked := sp.Budget > 0 && sp.Used >= 1
	if blocked {
		d.hardBudgetNotice(ctx, sp)
	}
	return blocked, nil
}

// backgroundPaused also stops watcher snapshots while the model cap is hit.
// budgetWatchTask runs a watcher turn under the stranger policy
// (runWatchTask), and not at all while background work is over budget.
func (d *Daemon) budgetWatchTask(ctx context.Context, key, text string) (string, error) {
	if blocked, err := d.backgroundOverBudget(ctx); err != nil || blocked {
		return "NOTHING_TO_REPORT", err
	}
	return d.runWatchTask(ctx, key, text)
}

func (d *Daemon) backgroundPaused() bool {
	if d.paused.Load() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	blocked, err := d.backgroundOverBudget(ctx)
	return blocked || err != nil
}

func (d *Daemon) hardBudgetNotice(ctx context.Context, sp memory.Spend) {
	budgetMu.Lock()
	key := "usage.paused." + sp.Month
	was, err := d.store.Get(ctx, key)
	if err != nil || was != "" {
		budgetMu.Unlock()
		return
	}
	err = d.store.Set(ctx, key, time.Now().Format(time.RFC3339))
	budgetMu.Unlock()
	if err != nil {
		return
	}
	_ = desktopNotify(d.Config().Name, budgetPausedText)
	if owner := d.ownerChatKey(); owner != "" {
		// Never wait for an owner's conversation while holding a background turn.
		go func() { _ = d.Notify(context.WithoutCancel(ctx), owner, budgetPausedText) }()
	}
}

// budgetWarnEvery is how often a chat is told it's over budget.
const budgetWarnEvery = 6 * time.Hour

// budgetReply warns in the actual reply, including streaming, rather than
// relying on the model to repeat an instruction.
func (d *Daemon) budgetReply(ctx context.Context, in channels.Inbound, ev *agent.Events, atEnd bool) string {
	if !in.IsOwner || spoken(in) && !atEnd { // out loud it comes after the answer
		return ""
	}
	sp, err := d.Spend(ctx)
	if err != nil || sp.Budget <= 0 || sp.Used < 1 {
		return ""
	}
	// Once in a while, not on every reply: a person mentions it once.
	key := homeKey(in.Key())
	if at, ok := d.budgetWarned.Load(key); ok && clock().Sub(at.(time.Time)) < budgetWarnEvery {
		return ""
	}
	d.budgetWarned.Store(key, clock())
	text := fmt.Sprintf("You're over your monthly model budget (%s used). Background work is paused until next month, or until you raise the budget: open the settings file from the menu bar, change usage.monthly_budget (your monthly limit) and restart me. I'll still answer you.\n\n", USD(sp.ToDate))
	if spoken(in) {
		text = "By the way, you're over this month's model budget, so background work is paused until you raise it."
	}
	if ev.OnDelta != nil {
		ev.OnDelta(text)
	}
	return text
}

// A direct request can be the one that crosses the cap. Warn at its end
// when there was nothing to warn about before it started.
func (d *Daemon) finishBudgetReply(ctx context.Context, in channels.Inbound, ev *agent.Events, warning, out string) string {
	if warning != "" {
		return warning + out
	}
	tail := *ev
	if delta := ev.OnDelta; delta != nil {
		tail.OnDelta = func(s string) { delta("\n\n" + s) }
	}
	if warning = d.budgetReply(ctx, in, &tail, true); warning != "" {
		return out + "\n\n" + warning
	}
	return out
}

func (d *Daemon) runRequestedProtocol(ctx context.Context, caller string, p protocols.Protocol) (string, error) {
	if memory.IsScratch(caller) {
		blocked, err := d.backgroundOverBudget(ctx)
		if err != nil {
			return "", err
		}
		if blocked {
			return "", fmt.Errorf("%s", budgetPausedText)
		}
	}
	return d.runTask(ctx, scratchKey(caller, "protocol"), fmt.Sprintf("Protocol %q: %s", p.Name, p.Prompt))
}

func (d *Daemon) budgetPortrait(ctx context.Context, key string) (string, error) {
	blocked, err := d.backgroundOverBudget(ctx)
	if err != nil || blocked {
		return "", err
	}
	return d.writePortrait(ctx, key) // portrait.go
}

func (d *Daemon) budgetStranger(ctx context.Context, in channels.Inbound) (string, bool) {
	if in.IsOwner {
		return "", false
	}
	// Do not disclose the owner's spending or settings to other people.
	sp, err := d.Spend(ctx)
	if err != nil || (sp.Budget > 0 && sp.Used >= 1) {
		return "I can't help right now. Please contact my owner directly.", true
	}
	return "", false
}
