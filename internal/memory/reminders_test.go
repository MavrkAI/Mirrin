package memory

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

func TestLiveKey(t *testing.T) {
	cases := map[string]string{
		"telegram:123": "telegram:123",
		"telegram:123#protocol-20260927-070000-3":    "telegram:123",
		"telegram:123#task-09271":                    "telegram:123",
		"telegram:123#task-09271#protocol-20260927":  "telegram:123",
		"whatsapp:614#watch-20260927-070000":         "whatsapp:614",
		"cli:terminal#firstlook-20260927-070000.123": "cli:terminal",
		"telegram:123#followup-42":                   "telegram:123",
		"phone#phone-CA42f":                          "phone",
		"voice:phone#call-CA42f":                     "voice:phone",
		// IRC rooms carry a '#' of their own.
		"irc:#golang|tony":                    "irc:#golang|tony",
		"irc:#go-nuts|tony":                   "irc:#go-nuts|tony",
		"irc:#task-force|tony":                "irc:#task-force|tony",
		"irc:#task-force":                     "irc:#task-force",
		"irc:#golang|tony#protocol-20260927":  "irc:#golang|tony",
		"irc:#task-force|tony#task-09271":     "irc:#task-force|tony",
		"zulip:stream:5:Fix #task-list bug":   "zulip:stream:5:Fix #task-list bug",
		"mail:tony+#tag@example.com":          "mail:tony+#tag@example.com",
		"zulip:stream:5:release #protocol-v2": "zulip:stream:5:release #protocol-v2",
	}
	for key, want := range cases {
		if got := LiveKey(key); got != want {
			t.Errorf("LiveKey(%q) = %q, want %q", key, got, want)
		}
		if IsScratch(key) != (want != key) {
			t.Errorf("IsScratch(%q) wrong", key)
		}
	}
}

func TestRemindersFromBackgroundRunsBelongToTheLiveChat(t *testing.T) {
	cases := []struct{ setIn, live, alsoListedIn string }{
		{"telegram:123#protocol-20260927-070000", "telegram:123", "telegram:123#task-0927"},
		{"irc:#golang|tony#protocol-20260927-070000", "irc:#golang|tony", "irc:#golang|tony"},
		{"irc:#golang|tony", "irc:#golang|tony", "irc:#golang|tony#task-0927"},
	}
	for _, c := range cases {
		t.Run(c.setIn, func(t *testing.T) {
			ctx := context.Background()
			s := openTest(t)
			_, _ = s.AddReminder(ctx, "irc:#rust|ana", time.Now().Add(time.Hour), "another room's reminder")
			if _, err := s.AddReminder(ctx, c.setIn, time.Now().Add(-time.Minute), "call the dentist"); err != nil {
				t.Fatal(err)
			}
			due, _ := s.DueReminders(ctx, time.Now())
			if len(due) != 1 || due[0].ChatKey != c.live {
				t.Fatalf("want the reminder on %s, got %+v", c.live, due)
			}
			for _, key := range []string{c.live, c.alsoListedIn} {
				if ps, _ := s.PendingReminders(ctx, key); len(ps) != 1 || ps[0].Text != "call the dentist" {
					t.Errorf("list_reminders from %s should see it alone, got %+v", key, ps)
				}
			}
		})
	}
}

func TestMigrationRescuesStuckReminders(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// A database from before reminders learned to retry, with one stuck on a scratch key.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE reminders (id INTEGER PRIMARY KEY AUTOINCREMENT, chat_key TEXT NOT NULL, due_at TEXT NOT NULL, text TEXT NOT NULL, fired INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL)`,
		`INSERT INTO reminders(chat_key, due_at, text, created_at) VALUES('whatsapp:61400#task-09271', '2026-09-01T00:00:00Z', 'stuck', '2026-09-01T00:00:00Z')`,
		`INSERT INTO reminders(chat_key, due_at, text, fired, created_at) VALUES('whatsapp:61400#protocol-x', '2026-09-01T00:00:00Z', 'old', 1, '2026-09-01T00:00:00Z')`,
		`INSERT INTO reminders(chat_key, due_at, text, created_at) VALUES('irc:#golang|tony', '2026-09-01T00:00:00Z', 'room', '2026-09-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	due, err := s.DueReminders(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0].ChatKey != "whatsapp:61400" || due[0].Attempts != 0 {
		t.Fatalf("stuck reminder not rescued: %+v", due)
	}
	if due[1].ChatKey != "irc:#golang|tony" {
		t.Fatalf("an IRC room's reminder is live already and must keep its key: %+v", due[1])
	}
	// Opening again changes nothing.
	s.Close()
	if s, err = Open(dir); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.DueReminders(ctx, time.Now()); len(again) != 2 || again[0].ChatKey != due[0].ChatKey || again[1].ChatKey != due[1].ChatKey {
		t.Fatalf("migration isn't stable: %+v", again)
	}
}

func TestReminderRetryAndGiveUp(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id, _ := s.AddReminder(ctx, "c", time.Now().Add(-time.Minute), "stretch")
	if err := s.RetryReminder(ctx, id, 1, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueReminders(ctx, time.Now()); len(due) != 0 {
		t.Fatalf("should wait for the retry time: %+v", due)
	}
	due, _ := s.DueReminders(ctx, time.Now().Add(6*time.Minute))
	if len(due) != 1 || due[0].Attempts != 1 {
		t.Fatalf("should be due again with one attempt recorded: %+v", due)
	}
	_ = s.GiveUpReminder(ctx, id)
	if due, _ := s.DueReminders(ctx, time.Now().Add(time.Hour)); len(due) != 0 {
		t.Fatal("given-up reminder still due")
	}
	if ps, _ := s.AllPendingReminders(ctx, 10); len(ps) != 0 {
		t.Fatal("given-up reminder still listed as pending")
	}
}

// A reminder that went out can be ticked off or put back for later, and
// the chat it went to can find it for half an hour: not once it is ticked
// off, put back, or older than that.
func TestReminderTickedOffOrPutBack(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id, _ := s.AddReminder(ctx, "telegram:1", time.Now().Add(-time.Minute), "call the plumber")
	if _, ok := s.LastFired(ctx, "telegram:1", 30*time.Minute); ok {
		t.Fatal("a reminder not sent yet counts as sent")
	}
	if err := s.MarkFired(ctx, id); err != nil {
		t.Fatal(err)
	}
	r, ok := s.LastFired(ctx, "telegram:1", 30*time.Minute)
	if !ok || r.ID != id || r.Kind != "remind" || r.Snoozes != 0 || time.Since(r.FiredAt) > time.Minute {
		t.Fatalf("last fired: %v %+v", ok, r)
	}
	if _, ok := s.LastFired(ctx, "telegram:2", 30*time.Minute); ok {
		t.Fatal("another chat's reminder counts as sent there")
	}
	if r, ok := s.LastFired(ctx, "", 30*time.Minute); !ok || r.ID != id {
		t.Fatalf("any chat's last fired: %v %+v", ok, r)
	}

	// Put back: pending again, due then, with the snooze counted.
	back := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := s.Snooze(ctx, id, back); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LastFired(ctx, "telegram:1", 30*time.Minute); ok {
		t.Fatal("a reminder put back still counts as just sent")
	}
	if due, _ := s.DueReminders(ctx, time.Now()); len(due) != 0 {
		t.Fatalf("a reminder put back is due now: %+v", due)
	}
	ps, _ := s.PendingReminders(ctx, "telegram:1")
	if len(ps) != 1 || !ps[0].DueAt.Equal(back) || ps[0].Snoozes != 1 {
		t.Fatalf("put back: %+v", ps)
	}

	// Sent again, and over half an hour ago: not the one a "done" is about.
	_ = s.MarkFired(ctx, id)
	if _, err := s.db.Exec(`UPDATE reminders SET fired_at=? WHERE id=?`, time.Now().Add(-31*time.Minute).UTC().Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LastFired(ctx, "telegram:1", 30*time.Minute); ok {
		t.Fatal("a reminder sent 31 minutes ago counts as just sent")
	}
	if r, ok := s.LastFired(ctx, "telegram:1", time.Hour); !ok || r.Snoozes != 1 {
		t.Fatalf("within the hour: %v %+v", ok, r)
	}

	// Ticked off: done, and neither found nor put back again.
	if err := s.MarkDone(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LastFired(ctx, "telegram:1", time.Hour); ok {
		t.Fatal("a reminder ticked off still counts as just sent")
	}
	if err := s.Snooze(ctx, id, back); !errors.Is(err, ErrNoReminder) {
		t.Fatalf("snoozing one ticked off: %v", err)
	}
	if err := s.MarkDone(ctx, id); err != nil {
		t.Fatalf("ticking it off twice: %v", err)
	}
	if err := s.MarkDone(ctx, 999); !errors.Is(err, ErrNoReminder) {
		t.Fatalf("ticking off one that isn't there: %v", err)
	}

	// One ticked off before it went out never goes out.
	early, _ := s.AddReminder(ctx, "telegram:1", time.Now().Add(-time.Minute), "water the plants")
	if err := s.MarkDone(ctx, early); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueReminders(ctx, time.Now()); len(due) != 0 {
		t.Fatalf("a reminder ticked off is still due: %+v", due)
	}
	if ps, _ := s.AllPendingReminders(ctx, 10); len(ps) != 0 {
		t.Fatalf("a reminder ticked off is still pending: %+v", ps)
	}
}

func TestPruneScratchKeepsOpenTasks(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for _, key := range []string{"w:1#task-open", "w:1#task-done", "w:1#protocol-1", "w:1", "irc:#golang|tony"} {
		_ = s.AppendMessage(ctx, key, llm.Text(llm.RoleUser, "goal for "+key))
	}
	if _, err := s.db.Exec(`UPDATE messages SET created_at='2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneScratch(ctx, 48*time.Hour, "w:1#task-open")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 pruned, got %d", n)
	}
	for key, want := range map[string]int{"w:1#task-open": 1, "w:1#task-done": 0, "w:1#protocol-1": 0, "w:1": 1, "irc:#golang|tony": 1} {
		if h, _ := s.History(ctx, key, 10); len(h) != want {
			t.Errorf("%s: want %d messages, got %d", key, want, len(h))
		}
	}
}

func TestOpenInAFolderWithAHashInItsName(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	dir := filepath.Join(parent, "Tony's #1 twin?100%")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "user", "Tony likes tea.", "t"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 || entries[0].Name() != filepath.Base(dir) {
		t.Fatalf("memory.db landed outside its folder: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory.db")); err != nil {
		t.Fatal(err)
	}
	s, _ = Open(dir)
	defer s.Close()
	if fs, _ := s.AllFacts(ctx, 10); len(fs) != 1 {
		t.Fatalf("reopening lost the facts: %+v", fs)
	}
}

// Reminders that went off lately and aren't ticked off are listed, newest
// first, so the twin never says one "will go off" after it has.
func TestRecentlyFiredReminders(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a, _ := s.AddReminder(ctx, "telegram:1", time.Now().Add(-time.Minute), "stretch")
	b, _ := s.AddReminder(ctx, "telegram:1", time.Now().Add(-time.Minute), "call the plumber")
	_, _ = s.AddReminder(ctx, "telegram:1", time.Now().Add(time.Hour), "not yet")
	if rs := s.RecentlyFired(ctx, time.Hour); len(rs) != 0 {
		t.Fatalf("listed before any went off: %+v", rs)
	}
	_ = s.MarkFired(ctx, a)
	_ = s.MarkFired(ctx, b)
	if rs := s.RecentlyFired(ctx, time.Hour); len(rs) != 2 {
		t.Fatalf("fired: %+v", rs)
	}
	_ = s.MarkDone(ctx, a)
	if rs := s.RecentlyFired(ctx, time.Hour); len(rs) != 1 || rs[0].Text != "call the plumber" {
		t.Fatalf("after ticking one off: %+v", rs)
	}
}
