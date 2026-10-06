package protocols

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/config"
	proto "github.com/MavrkAI/Mirrin/internal/protocols"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

func updateTool(t *testing.T, applied *[]Change) tools.Tool {
	t.Helper()
	load := func() []proto.Protocol {
		return []proto.Protocol{
			{Name: "morning briefing", Schedule: "0 7 * * *", Prompt: "p"},
			{Name: "nightly headlines", Schedule: "0 21 * * *", Prompt: "p", Pack: "news"},
			{Name: "rebook", Prompt: "p"},
		}
	}
	return UpdateTool(load, Updater{
		Apply: func(_ context.Context, c Change) (string, error) {
			*applied = append(*applied, c)
			return "done", nil
		},
		Next: func(p proto.Protocol) time.Time {
			if !p.IsEnabled() {
				return time.Time{}
			}
			return proto.NextRun(p.Schedule, time.Now())
		},
		CheckSchedule: func(s string) error { return proto.Check(proto.Protocol{Name: "x", Prompt: "p", Schedule: s}) },
	})
}

func call(v map[string]any) tools.Call {
	b, _ := json.Marshal(v)
	return tools.Call{Input: b}
}

// "Move my briefing to 8:30 on weekdays": one yes, to a change said in
// words, never in cron.
func TestUpdateProtocolNeedsAYesToAChangeInWords(t *testing.T) {
	var applied []Change
	tl := updateTool(t, &applied)
	if got := approvals.New(config.Default().Autonomy).Decide(tl); got != approvals.Ask {
		t.Fatalf("update_protocol: decision %v, want Ask", got)
	}
	sum := tl.(tools.Summarizer)
	for _, c := range []struct {
		in   map[string]any
		want []string
	}{
		{map[string]any{"name": "Morning Briefing", "schedule": "30 8 * * 1-5"}, []string{"Move “morning briefing” from every day at 7:00 to weekdays at 8:30."}},
		{map[string]any{"name": "nightly headlines", "enabled": false}, []string{"Turn off “nightly headlines”.", "This makes your own copy, so it stops getting updates from the “news” pack."}},
		{map[string]any{"name": "morning briefing", "skip_next": true}, []string{"Skip the next “morning briefing” (", " at 7:00)."}},
		{map[string]any{"name": "rebook", "schedule": "0 9 1 * *"}, []string{"Run “rebook” on the 1st at 9:00."}},
	} {
		got := sum.ApprovalSummary(call(c.in))
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("%v: approval %q lacks %q", c.in, got, want)
			}
		}
		if strings.Contains(got, "* *") {
			t.Errorf("%v: approval shows cron: %q", c.in, got)
		}
	}
	if strings.Contains(sum.ApprovalSummary(call(map[string]any{"name": "morning briefing", "skip_next": true})), "own copy") {
		t.Error("skipping a run changes no file, so makes no copy")
	}
	if _, err := tl.Run(context.Background(), call(map[string]any{"name": "morning briefing", "schedule": "30 8 * * 1-5"})); err != nil || len(applied) != 1 ||
		applied[0].Schedule == nil || *applied[0].Schedule != "30 8 * * 1-5" || applied[0].Enabled != nil || applied[0].SkipNext {
		t.Fatalf("run: %v %+v", err, applied)
	}
}

// What can't be done is refused before the owner is asked.
func TestUpdateProtocolIsCheckedBeforeTheOwnerIsAsked(t *testing.T) {
	var applied []Change
	c := updateTool(t, &applied).(tools.Checker)
	for _, tc := range []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"name": "evening wrap", "enabled": false}, "no protocol named"},
		{map[string]any{"name": "morning briefing", "schedule": "0 0 7 * * *"}, "drop the seconds"},
		{map[string]any{"name": "morning briefing", "schedule": "0 7 * * *"}, "already runs every day at 7:00"},
		{map[string]any{"name": "morning briefing", "enabled": true}, "already on"},
		{map[string]any{"name": "rebook", "skip_next": true}, "only when asked"},
		{map[string]any{"name": "morning briefing", "enabled": false, "skip_next": true}, "it's off"},
		{map[string]any{"name": "morning briefing"}, "say what to change"},
	} {
		err := c.Check(context.Background(), call(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.in, err, tc.want)
		}
	}
	if err := c.Check(context.Background(), call(map[string]any{"name": "morning briefing", "enabled": false})); err != nil {
		t.Fatalf("turning it off: %v", err)
	}
}
