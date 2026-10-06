package backup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/health"
)

// schedulerFor runs e's schedule by hand.
func schedulerFor(e *Engine) *Scheduler {
	return &Scheduler{Engine: func() (*Engine, error) { return e, nil }, DataDir: e.Layout.DataDir, Loc: time.UTC}
}

func TestHandoverMakesTheOldMachineStandBy(t *testing.T) {
	a := newTwin(t)
	p := fixedPhrase(t)
	dir := t.TempDir()
	ea := engineFor(t, a, p, dir)
	runAt(t, ea, time.Now().Add(-time.Hour))

	// Machine B restores the snapshot.
	homeB := filepath.Join(t.TempDir(), ".mirrin")
	r, err := Restore(context.Background(), RestoreOptions{Target: ea.Target, Phrase: p, Home: homeB, HostLabel: "Akshay's Mac mini"})
	if err != nil || r.Handover == "" {
		t.Fatalf("%v %q", err, r.HandoverNote)
	}

	// A's next tick finds the marker and stands by.
	var told *Handover
	sa := schedulerFor(ea)
	sa.OnStandby = func(h Handover) { told = &h }
	h := sa.CheckHandover(context.Background())
	if h == nil || told == nil || told.HostLabel != "Akshay's Mac mini" {
		t.Fatalf("A didn't stand by: %+v", h)
	}
	st, _ := LoadState(a.data)
	state, detail, fix := Health(st, Settings{Recipient: p.Recipient()}, time.Now())
	if state != health.Warn || detail != "Standing by: moved to Akshay's Mac mini" || !strings.Contains(fix, "resume") {
		t.Fatalf("health: %s %q %q", state, detail, fix)
	}
	if _, err := ea.Run(context.Background()); !errors.Is(err, ErrStandingBy) {
		t.Fatalf("a standing-by machine backed up: %v", err)
	}
	if sa.Due() {
		t.Fatal("a standing-by machine thinks a backup is due")
	}

	// Resume: A is the twin again, and that marker is ignored from now on.
	was, err := Resume(a.data)
	if err != nil || was == nil {
		t.Fatal(err)
	}
	told = nil
	if h := sa.CheckHandover(context.Background()); h != nil || told != nil {
		t.Fatal("a dismissed marker made A stand by again")
	}
	if _, err := ea.Run(context.Background()); err != nil {
		t.Fatalf("after resume: %v", err)
	}
}

func TestAMarkerForAnotherMachineOrKeyIsIgnored(t *testing.T) {
	a := newTwin(t)
	p := fixedPhrase(t)
	dir := t.TempDir()
	ea := engineFor(t, a, p, dir)
	runAt(t, ea, time.Now().Add(-time.Hour))
	standbyA := existingStandby(a.data)

	// A marker for some other machine.
	other := newTwin(t)
	eo := engineFor(t, other, p, t.TempDir())
	runAt(t, eo, time.Now())
	otherStandby := existingStandby(other.data)
	if _, err := writeHandover(context.Background(), ea.Target, p, otherStandby.Recipient().String(), "Elsewhere", 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A marker for A, but signed with other words: someone who can write to
	// the folder but doesn't have the words.
	stranger, _ := NewPhrase()
	if _, err := writeHandover(context.Background(), ea.Target, stranger, standbyA.Recipient().String(), "Intruder", 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if h, err := CheckHandover(context.Background(), ea.Target, standbyA, p.RecoveryPub(), nil, time.Time{}); err != nil || h != nil {
		t.Fatalf("A accepted a marker it shouldn't: %+v %v", h, err)
	}
}

func TestTriggerCoalescesIntoOneSnapshot(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	runs := make(chan string, 4)
	s := schedulerFor(e)
	s.Delay = 30 * time.Millisecond
	s.OnRun = func(m Manifest, err error) {
		if err != nil {
			runs <- err.Error()
			return
		}
		runs <- "ok"
	}
	s.Trigger("pairing")
	s.Trigger("revoke")
	if !s.Pending() {
		t.Fatal("nothing pending after a trigger")
	}
	select {
	case r := <-runs:
		if r != "ok" {
			t.Fatal(r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the triggered snapshot never ran")
	}
	select {
	case r := <-runs:
		t.Fatalf("two triggers made two snapshots (%s)", r)
	case <-time.After(150 * time.Millisecond):
	}
	objs, _ := e.Target.List(context.Background())
	if len(objs) != 1 {
		t.Fatalf("%d snapshots", len(objs))
	}
}

func TestNightlyIsDueAfter0330AndCatchesUp(t *testing.T) {
	tw := newTwin(t)
	s := &Scheduler{DataDir: tw.data, Loc: time.UTC}
	at := func(h, m int) time.Time { return time.Date(2026, 9, 27, h, m, 0, 0, time.UTC) }
	set := func(st State) {
		if err := SaveState(tw.data, st); err != nil {
			t.Fatal(err)
		}
	}
	s.Now = func() time.Time { return at(3, 31) }
	set(State{LastAttempt: at(3, 29).AddDate(0, 0, -1), LastGood: at(3, 29).AddDate(0, 0, -1)})
	if !s.Due() {
		t.Fatal("not due just after 03:30")
	}
	set(State{LastAttempt: at(3, 30), LastGood: at(3, 30)})
	if s.Due() {
		t.Fatal("due twice in a night")
	}
	// Asleep at 03:30: due when it wakes at 09:00.
	s.Now = func() time.Time { return at(9, 0) }
	set(State{LastAttempt: at(10, 0).AddDate(0, 0, -1), LastGood: at(10, 0).AddDate(0, 0, -1)})
	if !s.Due() {
		t.Fatal("a missed night doesn't catch up")
	}
	// A failed try is retried after an hour, not every five minutes.
	set(State{LastAttempt: at(8, 30), LastGood: at(3, 30).AddDate(0, 0, -1)})
	if s.Due() {
		t.Fatal("retried too soon")
	}
	s.Now = func() time.Time { return at(9, 31) }
	if !s.Due() {
		t.Fatal("never retried")
	}
}

// The machine moved zones since the twin started: 03:30 is where it is now.
func TestNightlyFollowsTheZoneItIsIn(t *testing.T) {
	tw := newTwin(t)
	tokyo := time.FixedZone("Tokyo", 9*3600)
	s := &Scheduler{DataDir: tw.data, Loc: time.UTC, Zone: func() *time.Location { return tokyo }}
	if err := SaveState(tw.data, State{LastAttempt: time.Date(2026, 9, 26, 3, 30, 0, 0, time.UTC), LastGood: time.Date(2026, 9, 26, 3, 30, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Date(2026, 9, 27, 3, 31, 0, 0, tokyo) } // 18:31 the day before in UTC
	if !s.Due() {
		t.Fatal("not due just after 03:30 in the zone the machine is in")
	}
	s.Zone = nil
	if s.Due() {
		t.Fatal("without Zone, 03:30 is read in Loc")
	}
}

func TestHealthAges(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	on := Settings{Recipient: "age1pq1x", Where: " (iCloud Drive › Mirrin Backups)"}
	cases := []struct {
		st   State
		s    Settings
		want health.State
		has  string
	}{
		{State{}, Settings{}, health.Off, "not set up"},
		{State{LastGood: now.Add(-3 * time.Hour)}, on, health.OK, "last backup 3 hours ago (iCloud Drive"},
		{State{LastGood: now.Add(-50 * time.Hour), LastError: "disk full"}, on, health.Warn, "no good backup for 2 days (iCloud Drive › Mirrin Backups) (last try: disk full)"},
		{State{LastGood: now.Add(-8 * 24 * time.Hour)}, on, health.Fail, "8 days"},
		{State{Since: now.Add(-time.Hour)}, on, health.OK, "first one runs tonight"},
		{State{Since: now.Add(-9 * 24 * time.Hour)}, on, health.Fail, "no backup yet"},
		{State{LastGood: now.Add(-3 * time.Hour), LeftOut: []string{"protocols/Meeting 10:30.yaml"}}, on, health.Warn, "but it left out protocols/Meeting 10:30.yaml"},
	}
	for _, c := range cases {
		st, detail, _ := Health(c.st, c.s, now)
		if st != c.want || !strings.Contains(detail, c.has) {
			t.Errorf("%+v: %s %q", c.st, st, detail)
		}
	}
}

// "Back up now" starts one run however quickly it is pressed, and none
// before the schedule starts or after it stops.
func TestRunNowStartsOneRun(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	s := &Scheduler{DataDir: t.TempDir(), Loc: time.UTC, Engine: func() (*Engine, error) {
		calls.Add(1)
		<-release
		return nil, nil
	}}
	if err := s.RunNow("test"); !errors.Is(err, ErrStopped) {
		t.Fatalf("before Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	var wg sync.WaitGroup
	var started atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.RunNow("test"); err == nil {
				started.Add(1)
			} else if !errors.Is(err, ErrBusy) {
				t.Errorf("RunNow: %v", err)
			}
		}()
	}
	wg.Wait()
	if started.Load() != 1 || !s.Running() {
		t.Fatalf("%d runs started", started.Load())
	}
	close(release)
	for deadline := time.Now().Add(3 * time.Second); s.Running() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Running() {
		t.Fatal("the run never finished")
	}
	cancel()
	if err := s.RunNow("test"); !errors.Is(err, ErrStopped) {
		t.Fatalf("after shutdown: %v", err)
	}
}
