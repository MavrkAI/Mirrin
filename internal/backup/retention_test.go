package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dailyHistory(days int, end time.Time) []Object {
	var objs []Object
	for i := days - 1; i >= 0; i-- {
		objs = append(objs, Object{Name: newName("snap", end.AddDate(0, 0, -i))})
	}
	return objs
}

func TestRetentionOver400DaysKeeps7Daily4Weekly6Monthly(t *testing.T) {
	now := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
	objs := dailyHistory(400, now)
	why := DefaultRetention.plan(objs, "", now)
	counts := map[string]int{}
	for _, reason := range why {
		counts[reason]++
	}
	if counts["daily"] != 7 || counts["weekly"] != 4 || counts["monthly"] != 6 || len(why) != 17 {
		t.Fatalf("kept %v (%d)", counts, len(why))
	}
	keep, drop := DefaultRetention.Apply(objs, "", now)
	if len(keep) != 17 || len(drop) != 383 {
		t.Fatalf("keep %d drop %d", len(keep), len(drop))
	}
	// The newest is always among them.
	if why[objs[len(objs)-1].Name] != "daily" {
		t.Fatal("the newest snapshot isn't kept")
	}
}

func TestRetentionNeverPrunesTheNewestVerified(t *testing.T) {
	now := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
	objs := dailyHistory(400, now)
	// Only an old snapshot was ever read back intact (the newer ones came
	// from another machine or failed their read-back).
	verified := objs[10].Name
	keep, _ := DefaultRetention.Apply(objs, verified, now)
	found := false
	for _, o := range keep {
		found = found || o.Name == verified
	}
	if !found || len(keep) != 18 {
		t.Fatalf("the newest verified snapshot was pruned (kept %d)", len(keep))
	}
}

func TestRetentionIgnoresFutureAndForeignNames(t *testing.T) {
	now := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
	objs := dailyHistory(30, now)
	// Planted names from the future must not push real snapshots out.
	for i := 1; i <= 40; i++ {
		objs = append(objs, Object{Name: newName("snap", now.AddDate(0, i, 0))})
	}
	objs = append(objs, Object{Name: newName("handover", now)})
	keep, drop := DefaultRetention.Apply(objs, "", now)
	for _, o := range drop {
		if _, at, _ := parseName(o.Name); at.After(now) || !IsSnapshot(o.Name) {
			t.Fatalf("dropped %s", o.Name)
		}
	}
	real := 0
	for _, o := range keep {
		if _, at, _ := parseName(o.Name); !at.After(now) && IsSnapshot(o.Name) {
			real++
		}
	}
	if real < 11 {
		t.Fatalf("only %d real snapshots kept", real)
	}
}

func TestShortHistoryKeepsEverythingUseful(t *testing.T) {
	now := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
	keep, drop := DefaultRetention.Apply(dailyHistory(3, now), "", now)
	if len(keep) != 3 || len(drop) != 0 {
		t.Fatalf("keep %d drop %d", len(keep), len(drop))
	}
}

func TestRunPrunes(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	dir := t.TempDir()
	e := engineFor(t, tw, p, dir)
	e.Keep = Retention{Daily: 2}
	base := time.Now().Add(-72 * time.Hour)
	for i := 0; i < 4; i++ {
		runAt(t, e, base.Add(time.Duration(i)*24*time.Hour))
	}
	objs, _ := e.Target.List(context.Background())
	if len(objs) != 2 {
		t.Fatalf("%d snapshots left", len(objs))
	}
	st, _ := LoadState(tw.data)
	if objs[len(objs)-1].Name != st.LastName {
		t.Fatal("the newest snapshot was pruned")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, p.Namespace()))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".partial") {
			t.Fatalf("partial file left: %s", e.Name())
		}
	}
}
