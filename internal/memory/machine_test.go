package memory

import (
	"testing"
	"time"
)

func TestPauseRecordReadsOldAndNew(t *testing.T) {
	since := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	// A plain pause is kept as older versions wrote it.
	if got := (Pause{Since: since}).String(); got != "2026-09-29T09:00:00Z" {
		t.Fatalf("plain: %q", got)
	}
	for _, c := range []struct {
		v    string
		want Pause
	}{
		{"2026-09-29T09:00:00Z", Pause{Since: since}}, // an old record
		{Pause{Since: since, Until: until}.String(), Pause{Since: since, Until: until}},
		{"garbage", Pause{}}, // still a pause, since an unknown time
	} {
		p, ok := ParsePause(c.v)
		if !ok || !p.Since.Equal(c.want.Since) || !p.Until.Equal(c.want.Until) {
			t.Fatalf("%q: %+v %v", c.v, p, ok)
		}
	}
	if _, ok := ParsePause(""); ok {
		t.Fatal("no record is no pause")
	}
	p := Pause{Since: since, Until: until}
	if p.Expired(until.Add(-time.Second)) || !p.Expired(until) || (Pause{Since: since}).Expired(until.Add(time.Hour*1000)) {
		t.Fatal("expiry")
	}
}
