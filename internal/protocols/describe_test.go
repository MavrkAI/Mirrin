package protocols

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDescribeSaysTheScheduleInWords(t *testing.T) {
	for cron, want := range map[string]string{
		"0 7 * * *":             "every day at 7:00",
		"30 8 * * 1-5":          "weekdays at 8:30",
		"30 8 * * MON-FRI":      "weekdays at 8:30",
		"45 9 * * 0,6":          "weekends at 9:45",
		"45 9 * * sat,sun":      "weekends at 9:45",
		"0 18 * * 0":            "Sundays at 18:00",
		"0 18 * * SUN":          "Sundays at 18:00",
		"0 17 * * 1,4":          "Mondays and Thursdays at 17:00",
		"0 17 * * 1,3,5":        "Mondays, Wednesdays and Fridays at 17:00",
		"0 17 * * 0-6":          "every day at 17:00",
		"0 9 1 * *":             "on the 1st at 9:00",
		"0 9 2,22 * *":          "on the 2nd and 22nd at 9:00",
		"0 9 11 * *":            "on the 11th at 9:00",
		"5 0 * * *":             "every day at 0:05",
		"0 7,19 * * *":          "every day at 7:00 and 19:00",
		"0 * * * *":             "every hour",
		"*/15 * * * *":          "every 15 minutes",
		"@daily":                "every day at 0:00",
		"":                      "when you ask",
		"  0 21 * * *  ":        "every day at 21:00",
		"0 9 1 1 *":             "0 9 1 1 *", // a month: as written
		"0 9 1 * 1":             "0 9 1 * 1", // a day of the month and of the week
		"*/5 7-9 * * 1":         "*/5 7-9 * * 1",
		"0 25 * * *":            "0 25 * * *",
		"0 7 * * 1-9":           "0 7 * * 1-9",
		"CRON_TZ=UTC 0 7 * * *": "CRON_TZ=UTC 0 7 * * *",
	} {
		if got := Describe(cron); got != want {
			t.Errorf("Describe(%q) = %q, want %q", cron, got, want)
		}
	}
}

func TestWhenText(t *testing.T) {
	loc := time.FixedZone("here", 10*3600)
	now := time.Date(2030, 10, 1, 22, 0, 0, 0, loc) // a Tuesday
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{time.Date(2030, 10, 1, 23, 30, 0, 0, loc), "today at 23:30"},
		{time.Date(2030, 10, 2, 7, 0, 0, 0, loc), "tomorrow at 7:00"},
		{time.Date(2030, 10, 6, 18, 0, 0, 0, loc), "Sunday 6 Oct at 18:00"},
	} {
		if got := WhenText(c.t, now); got != c.want {
			t.Errorf("WhenText(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

// A change to the owner's own protocol is made in its file, keeping what
// they wrote; a change to a pack's makes the owner's copy, which shadows the
// pack's and leaves the pack as it came.
func TestEditKeepsTheFileAndCopiesAPacksProtocol(t *testing.T) {
	dir := t.TempDir()
	own := "# my morning\nname: morning briefing\nschedule: \"0 7 * * *\" # at seven\nprompt: |\n  Brief me.\n  Keep it short.\n"
	if err := os.WriteFile(filepath.Join(dir, "morning-briefing.yaml"), []byte(own), 0o600); err != nil {
		t.Fatal(err)
	}
	pdir := filepath.Join(PacksDir(dir), "news", "protocols")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	packFile := filepath.Join(pdir, "headlines.yaml")
	pack := "schedule: \"0 21 * * *\"\nprompt: Read the {{source}} headlines.\n"
	if err := os.WriteFile(packFile, []byte(pack), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, _ := LoadAll(dir)
	brief, _ := Find(ps, "morning briefing")
	weekdays := "30 8 * * 1-5"
	path, err := Edit(dir, brief, &weekdays, nil)
	if err != nil || path != brief.Source {
		t.Fatalf("edit: %v %q", err, path)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{"# my morning", `schedule: "30 8 * * 1-5"`, "  Brief me.\n  Keep it short.\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the file lost %q:\n%s", want, b)
		}
	}

	head, _ := Find(ps, "headlines")
	off := false
	path, err = Edit(dir, head, nil, &off)
	if err != nil || filepath.Dir(path) != dir {
		t.Fatalf("a pack's protocol is copied into the owner's folder: %v %q", err, path)
	}
	if b, _ := os.ReadFile(packFile); string(b) != pack {
		t.Fatalf("the pack's own file changed:\n%s", b)
	}
	ps, problems := LoadAll(dir)
	if len(problems) != 0 {
		t.Fatalf("the copy is reported as a problem: %+v", problems)
	}
	head, _ = Find(ps, "headlines")
	if head.Pack != "" || head.IsEnabled() || head.Schedule != "0 21 * * *" || head.RawPrompt != "Read the {{source}} headlines." {
		t.Fatalf("the owner's copy should shadow the pack's: %+v", head)
	}
}
