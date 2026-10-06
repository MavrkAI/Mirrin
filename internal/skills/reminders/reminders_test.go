package reminders

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mem "github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// ParseWhen reads the owner's own words for putting a reminder back, as
// well as the times the model writes.
func TestParseWhen(t *testing.T) {
	loc := time.FixedZone("AEST", 10*3600)
	now := time.Date(2026, 10, 3, 15, 0, 0, 0, loc) // a Saturday afternoon
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, loc) }
	cases := map[string]time.Time{
		"45m":                       now.Add(45 * time.Minute),
		"2h":                        now.Add(2 * time.Hour),
		"3d":                        now.Add(72 * time.Hour),
		"2026-10-05 07:30":          at(5, 7, 30),
		"2026-10-05T07:30:00+10:00": at(5, 7, 30),
		"in 20 minutes":             now.Add(20 * time.Minute),
		"in 1 minute":               now.Add(time.Minute),
		"In 5 mins":                 now.Add(5 * time.Minute),
		"in an hour":                now.Add(time.Hour),
		"in 2 hours":                now.Add(2 * time.Hour),
		"in a day":                  now.Add(24 * time.Hour),
		"tonight":                   at(3, 20, 0),
		"tomorrow":                  at(4, 9, 0),
		"Tomorrow 9":                at(4, 9, 0),
		"tomorrow at 9":             at(4, 9, 0),
		"tomorrow 9:30":             at(4, 9, 30),
		"tomorrow 9.30am":           at(4, 9, 30),
		"tomorrow 3":                at(4, 15, 0), // the afternoon, not three in the morning
		"tomorrow 3am":              at(4, 3, 0),
		"tomorrow 12pm":             at(4, 12, 0),
		"tomorrow 12am":             at(4, 0, 0),
		"tomorrow 14:00":            at(4, 14, 0),
	}
	for in, want := range cases {
		got, err := ParseWhen(in, now, loc)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseWhen(%q) = %v, %v; want %v", in, got.In(loc), err, want)
		}
	}
	// Tonight, once eight has passed, is an hour from now.
	late := time.Date(2026, 10, 3, 21, 15, 0, 0, loc)
	if got, _ := ParseWhen("tonight", late, loc); !got.Equal(late.Add(time.Hour)) {
		t.Errorf("tonight at 9:15 pm = %v", got.In(loc))
	}
	for _, in := range []string{"", "soon", "later", "done", "tomorrow 25", "tomorrow 13pm", "tomorrow 9:75", "in some minutes", "tomorrow morning", "call me tomorrow"} {
		if got, err := ParseWhen(in, now, loc); err == nil {
			t.Errorf("ParseWhen(%q) = %v, want an error", in, got.In(loc))
		}
	}
}

// follow_up keeps a promise to look again as a reminder of kind "check",
// with its notes, and reads like set_reminder. Ten can be open at once: the
// eleventh is refused in words, and nothing is stored for it.
func TestFollowUpStoresACheckAndTenAtMost(t *testing.T) {
	ctx := context.Background()
	store, err := mem.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var follow tools.Tool
	for _, tl := range Tools(store, time.UTC) {
		if tl.Spec().Name == "follow_up" {
			follow = tl
		}
	}
	if follow == nil {
		t.Fatal("no follow_up tool")
	}
	if follow.Risk() != tools.RiskRead {
		t.Fatalf("follow_up's risk is %v, want read like set_reminder", follow.Risk())
	}
	call := func(key, about, notes string) string {
		in, _ := json.Marshal(map[string]string{"when": "3d", "about": about, "notes": notes})
		out, err := follow.Run(ctx, tools.Call{ChatKey: key, Input: in})
		if err != nil {
			t.Fatalf("follow_up %q: %v", about, err)
		}
		return out
	}
	if out := call("telegram:1#protocol-20261003-1", "Acme's reply about the refund", "Emailed refunds@acme.test on Tuesday, order 4471, £42"); !strings.HasPrefix(out, "follow-up #") {
		t.Fatalf("follow_up said %q", out)
	}
	rs, _ := store.PendingReminders(ctx, "telegram:1")
	if len(rs) != 1 || rs[0].Kind != mem.KindCheck || rs[0].Text != "Acme's reply about the refund" || !strings.Contains(rs[0].Brief, "order 4471") {
		t.Fatalf("stored %+v", rs)
	}
	if d := time.Until(rs[0].DueAt); d < 71*time.Hour || d > 73*time.Hour {
		t.Fatalf("due in %v, want three days", d)
	}
	// Looking again is the owner's call: a follow-up's own run can't set one.
	if out := call("telegram:1#followup-1", "Acme again", ""); !strings.HasPrefix(out, "A follow-up can't set another.") {
		t.Fatalf("a follow-up set another: %q", out)
	}
	// A plain reminder doesn't count against the follow-ups.
	if _, err := store.AddReminder(ctx, "telegram:1", time.Now().Add(time.Hour), "call the plumber"); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= MaxChecks; i++ {
		call("telegram:1", "thing "+string(rune('a'+i)), "")
	}
	if out := call("telegram:1", "one too many", ""); out != "There are already 10 follow-ups open. Finish or cancel one first." {
		t.Fatalf("the eleventh: %q", out)
	}
	rs, _ = store.PendingReminders(ctx, "telegram:1")
	if len(rs) != MaxChecks+1 {
		t.Fatalf("%d pending, want ten follow-ups and the reminder", len(rs))
	}
	// One that has run makes room again.
	for _, r := range rs {
		if r.Kind == mem.KindCheck {
			if err := store.MarkFired(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if out := call("telegram:1", "room again", ""); !strings.HasPrefix(out, "follow-up #") {
		t.Fatalf("after one ran: %q", out)
	}
}
