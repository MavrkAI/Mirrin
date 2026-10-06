package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// Regression: Resume in the menu on a machine standing by after a backup
// handover lifted the pause but left the standby, so its chat apps stayed
// refused ("standing by") until `mirrin backup resume` in a terminal.
func TestResumingAStandbyGoesThroughBackupResume(t *testing.T) {
	var shown []string
	prev := desktopNotify
	desktopNotify = func(_, body string) error { shown = append(shown, body); return nil }
	t.Cleanup(func() { desktopNotify = prev })
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	h := backup.Handover{Name: "handover-20260927T090000Z-0a0b0c0d.age", HostLabel: "New Mac", At: time.Now()}
	if err := backup.StandBy(td.cfg.DataDir, h); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	td.startBackup(ctx)
	cancel()
	if !td.paused.Load() {
		t.Fatal("standing by isn't paused")
	}
	if err := td.StartChannel("telegram"); err == nil || !strings.Contains(err.Error(), "standing by") {
		t.Fatalf("start while standing by: %v", err)
	}
	shown = nil // the standby's own notice
	td.SetPaused(false)
	st, err := backup.LoadState(td.cfg.DataDir)
	if err != nil || st.Standby != nil {
		t.Fatalf("still standing by: %+v %v", st.Standby, err)
	}
	if td.paused.Load() || td.standingBy() {
		t.Fatal("resumed, but not the twin again")
	}
	// The owner is warned, as `mirrin backup resume` warns in a terminal:
	// Resume may have come from the menu or a paired device's /pause.
	if len(shown) != 1 || !strings.Contains(shown[0], "Quit Mirrin on New Mac first") {
		t.Fatalf("warned %q", shown)
	}
	if err := td.StartChannel("telegram"); err != nil && strings.Contains(err.Error(), "standing by") {
		t.Fatalf("channels still refused: %v", err)
	}
}

// A pause for a while ends by itself.
func TestAPauseForAWhileResumesItself(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	td.PauseUntil(time.Now().Add(50 * time.Millisecond))
	if !td.paused.Load() {
		t.Fatal("not paused")
	}
	if td.PausedUntil().IsZero() {
		t.Fatal("the end isn't kept")
	}
	v, _ := td.store.Get(context.Background(), pauseKey)
	if p, ok := memory.ParsePause(v); !ok || p.Until.IsZero() || p.Since.IsZero() {
		t.Fatalf("kept as %q", v)
	}
	eventually(t, "the pause to end", func() bool { return !td.paused.Load() })
	if v, _ := td.store.Get(context.Background(), pauseKey); v != "" {
		t.Fatalf("the pause is still kept: %q", v)
	}
}

// Pause in the menu over a pause for a while lasts until the owner resumes.
func TestAPlainPauseOverATimedOneStays(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	td.PauseUntil(time.Now().Add(50 * time.Millisecond))
	td.SetPaused(true)
	time.Sleep(1200 * time.Millisecond)
	if !td.paused.Load() || !td.PausedUntil().IsZero() {
		t.Fatal("a plain pause ended by itself")
	}
}

func TestRestoringAPauseHonoursItsEnd(t *testing.T) {
	restart := func(t *testing.T, td *testDaemon) *Daemon {
		t.Helper()
		cfg := td.Config()
		d2, err := New(&cfg, Options{Headless: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d2.disarmPause(); d2.store.Close() })
		return d2
	}
	now := time.Now().UTC()
	for _, c := range []struct {
		name   string
		value  string
		paused bool
	}{
		{"past its end", fmt.Sprintf(`{"since":%q,"until":%q}`, now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339)), false},
		{"before its end", fmt.Sprintf(`{"since":%q,"until":%q}`, now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339)), true},
		{"an old plain record", now.Add(-time.Hour).Format(time.RFC3339), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
			if err := td.store.Set(context.Background(), pauseKey, c.value); err != nil {
				t.Fatal(err)
			}
			d := restart(t, td)
			if d.paused.Load() != c.paused {
				t.Fatalf("paused %v, want %v", d.paused.Load(), c.paused)
			}
			if v, _ := d.store.Get(context.Background(), pauseKey); !c.paused && v != "" {
				t.Fatalf("an expired pause is still kept: %q", v)
			}
		})
	}
}

// Regression: "stop" (and Stop in the menu) didn't reach a scheduled
// protocol or routine run: it ran in a scratch conversation nothing tracked.
func TestStopReachesAProtocolRun(t *testing.T) {
	td, started, ended := newBooker(t)
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := td.budgetTask(context.Background(), ownerKey+"#protocol-20260929-090000-1", `Protocol "Long": book the table`)
		done <- result{out, err}
	}()
	waitStarted(t, started)
	msg, ok := td.Interrupt(context.Background())
	if !ok || !strings.Contains(msg, "Long") {
		t.Fatalf("interrupt: %q %v", msg, ok)
	}
	waitEnded(t, ended)
	// Regression: the heartbeat then took the stop for a failure and told
	// the owner "Long didn't run this time", right after "OK, I've
	// stopped". A stopped run has nothing more to report.
	select {
	case r := <-done:
		if r.err != nil || !strings.Contains(r.out, "NOTHING_TO_REPORT") {
			t.Fatalf("a stopped run returned %q, %v", r.out, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run never ended")
	}
}

// Regression: every scheduled run left its scratch conversation in d.locks
// for as long as the twin ran.
func TestScratchConversationsAreLetGo(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	count := func() int {
		n := 0
		td.locks.Range(func(any, any) bool { n++; return true })
		return n
	}
	base := count()
	for i := range 100 {
		if _, err := td.budgetTask(context.Background(), fmt.Sprintf("%s#protocol-20260929-090000-%d", ownerKey, i), `Protocol "Brief": say hi`); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != base {
		t.Fatalf("%d conversations, want %d", n, base)
	}
	// One still in use stays.
	c, release := td.acquire(ownerKey + "#watch-1")
	c2, release2 := td.acquire(ownerKey + "#watch-1")
	if c != c2 {
		t.Fatal("two conversations for one key")
	}
	release()
	if count() != base+1 {
		t.Fatal("let go while still in use")
	}
	release2()
	release2() // twice is once
	if count() != base {
		t.Fatal("kept once idle")
	}
	// A live chat is never let go.
	_, release = td.acquire(ownerKey)
	release()
	if _, ok := td.locks.Load(ownerKey); !ok {
		t.Fatal("a live chat was let go")
	}
}

// Regression: an approval a background run raised was decided in the run's
// scratch conversation, which then stayed in d.locks for as long as the
// twin ran: one more for every protocol run that asked the owner anything.
func TestDecidingABackgroundRunsApprovalLetsItsConversationGo(t *testing.T) {
	td := newTestDaemon(t, butler)
	count := func() int {
		n := 0
		td.locks.Range(func(any, any) bool { n++; return true })
		return n
	}
	td.owner(t, "hello") // the owner's own chat is there already, and stays
	base := count()
	for i := range 3 {
		key := fmt.Sprintf("%s#protocol-20260929-090000-%d", ownerKey, i)
		id := raise(t, td, key, "send", `{"to":"landlord"}`)
		if got := td.owner(t, fmt.Sprintf("yes %d", id)); got != "Sent." {
			t.Fatalf("reply %q", got)
		}
		if _, ok := td.locks.Load(key); ok {
			t.Fatalf("%s kept after its approval was decided", key)
		}
	}
	if n := count(); n != base {
		t.Fatalf("%d conversations, want %d", n, base)
	}
}

// A task's update recorded in a scratch conversation leaves nothing behind.
func TestRecordingInAScratchConversationLetsItGo(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	key := ownerKey + "#task-7"
	td.record(context.Background(), key, llm.Text(llm.RoleAssistant, "done"))
	if _, ok := td.locks.Load(key); ok {
		t.Fatal("kept after recording")
	}
}

// The screen hears that the twin is paused and until when, so it can look
// paused, and the hours its clock dozes through: the owner's quiet hours,
// else their push quiet hours, else 23:00 to 06:00. A standby's pause says
// where it moved to: the menu didn't pause it.
func TestTheScreenKnowsTheTwinsHours(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	ctx := context.Background()
	hours := func() ScreenData {
		t.Helper()
		var sd ScreenData
		td.screenHours(ctx, &sd)
		return sd
	}
	td.cfg.User.QuietHours = ""
	if sd := hours(); sd.Paused || sd.PausedUntil != "" || sd.StandingBy != "" || sd.QuietHours != "23:00-06:00" {
		t.Fatalf("not paused, no quiet hours set: %+v", sd)
	}
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	td.PauseUntil(until)
	sd := hours()
	if got, err := time.Parse(time.RFC3339, sd.PausedUntil); !sd.Paused || err != nil || !got.Equal(until) || sd.StandingBy != "" {
		t.Fatalf("paused for an hour: %+v", sd)
	}
	td.SetPaused(true) // until the owner resumes
	if sd := hours(); !sd.Paused || sd.PausedUntil != "" {
		t.Fatalf("paused until resumed: %+v", sd)
	}
	td.cfg.Push.QuietHours = "22:30-06:30"
	if sd := hours(); sd.QuietHours != "22:30-06:30" {
		t.Fatalf("push quiet hours: %q", sd.QuietHours)
	}
	td.cfg.User.QuietHours = "21:00 - 07:00"
	if sd := hours(); sd.QuietHours != "21:00-07:00" {
		t.Fatalf("the owner's quiet hours: %q", sd.QuietHours)
	}
	td.cfg.User.QuietHours = "off"
	if sd := hours(); sd.QuietHours != "off" {
		t.Fatalf("quiet hours off: %q", sd.QuietHours)
	}
	if err := td.store.Set(ctx, standbyPauseKey, "New Mac"); err != nil {
		t.Fatal(err)
	}
	if sd := hours(); !sd.Paused || sd.StandingBy != "New Mac" {
		t.Fatalf("standing by: %+v", sd)
	}
	td.SetPaused(false)
	if sd := hours(); sd.Paused || sd.PausedUntil != "" || sd.StandingBy != "" {
		t.Fatalf("resumed: %+v", sd)
	}
}
