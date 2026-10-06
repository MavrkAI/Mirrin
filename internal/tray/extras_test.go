package tray

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

func TestSpendTitle(t *testing.T) {
	got := spendTitle(memory.Spend{ToDate: 3.1, Budget: 25}, nil)
	if got != "Model: US$3.10 this month (estimate) / US$25.00 budget" {
		t.Fatal(got)
	}
	if got := spendTitle(memory.Spend{}, errors.New("secret")); strings.Contains(got, "secret") || !strings.Contains(got, "mirrin usage") {
		t.Fatal(got)
	}
}

func TestReportProblemUsesExistingFlowAndRevealsExactReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	path := filepath.Join(home, "reports", "mirrin-report-20260928-120000.txt")
	var revealed string
	run := func(ctx context.Context, exe string, args ...string) ([]byte, error) {
		if !filepath.IsAbs(exe) || !reflect.DeepEqual(args, []string{"report"}) {
			t.Fatalf("%s %v", exe, args)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing timeout")
		}
		return []byte("Wrote your problem report to:\n  " + path + "\n\nRead it before sharing."), nil
	}
	if err := reportProblem(context.Background(), run, func(p string) { revealed = p }); err != nil {
		t.Fatal(err)
	}
	if revealed != path {
		t.Fatalf("revealed %q", revealed)
	}
	for _, bad := range []string{"/tmp/report.txt", filepath.Join(home, "reports", "..", "mirrin-report-1.txt"), "raw error"} {
		if _, err := reportPath(bad, home); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	revealed = ""
	err := reportProblem(context.Background(), func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("raw secret") }, func(p string) { revealed = p })
	if err == nil || strings.Contains(err.Error(), "raw secret") || revealed != "" {
		t.Fatalf("%v %s", err, revealed)
	}
}

func TestDesktopPresent(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	if desktopPresent() {
		t.Fatal("headless")
	}
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	if !desktopPresent() {
		t.Fatal("wayland")
	}
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":0")
	if !desktopPresent() {
		t.Fatal("X11")
	}
}

func TestTerminalLineKeepsHome(t *testing.T) {
	t.Setenv("MIRRIN_HOME", "/tmp/twin's home")
	line := terminalLine("/tmp/mirrin", []string{"backup", "status"})
	if !strings.Contains(line, "MIRRIN_HOME="+shellQuote("/tmp/twin's home")) || !strings.HasSuffix(line, "/tmp/mirrin backup status") {
		t.Fatal(line)
	}
}

func TestExtrasStayResponsiveWhileReportRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	usage, logs, report, devices, backup := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	events := make(chan string, 10)
	release := make(chan struct{})
	wireExtras(ctx, extraClicks{spend: usage, logs: logs, report: report, devices: devices, backup: backup}, extraActions{
		terminal: func(args ...string) { events <- strings.Join(args, " ") },
		logs:     func() { events <- "logs" },
		report:   func() error { events <- "report"; <-release; return errors.New("report failed") },
		failure:  func(err error) { events <- err.Error() },
	})
	click := func(ch chan struct{}) {
		t.Helper()
		select {
		case ch <- struct{}{}:
		case <-time.After(time.Second):
			t.Fatal("menu blocked")
		}
	}
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("%q != %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("waiting for", want)
		}
	}
	click(report)
	expect("report")
	click(report) // coalesced while the first report is running
	click(usage)
	expect("usage")
	click(logs)
	expect("logs")
	click(devices)
	expect("devices list")
	click(backup)
	expect("backup status")
	close(release)
	expect("report failed")
}

func TestExtrasRefreshSpendOnlyOnceAMinute(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	calls := 0
	title := ""
	refresh := spendRefresh(context.Background(), func(context.Context) (memory.Spend, error) {
		calls++
		return memory.Spend{ToDate: 3.1}, nil
	}, func(s string) { title = s }, func() time.Time { return now })
	refresh()
	for range 11 {
		now = now.Add(5 * time.Second)
		refresh()
	}
	if calls != 1 || !strings.Contains(title, "US$3.10") {
		t.Fatalf("%d %s", calls, title)
	}
	now = now.Add(5 * time.Second)
	refresh()
	if calls != 2 {
		t.Fatal(calls)
	}
}

// With the twin's pages, the device and safety entries open them; without
// (the API is off), each falls back to its terminal command.
func TestExtrasOpenThePages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	devices, backup, add, reach, review := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	events := make(chan string, 10)
	var pagesOn atomic.Bool
	wireExtras(ctx, extraClicks{devices: devices, backup: backup, addPhone: add, reach: reach, review: review}, extraActions{
		terminal: func(args ...string) { events <- "terminal " + strings.Join(args, " ") },
		page: func(path string) bool {
			if pagesOn.Load() {
				events <- "page " + path
			}
			return pagesOn.Load()
		},
	})
	for _, c := range []struct {
		ch         chan struct{}
		page, term string
	}{
		{add, "page /devices/add", "terminal pair --screen"},
		{devices, "page /devices/page", "terminal devices list"},
		{backup, "page /backup", "terminal backup status"},
		{reach, "page /reach", "terminal reach status"},
		{review, "page /restore/review", "terminal devices list"},
	} {
		for _, on := range []bool{true, false} {
			pagesOn.Store(on)
			c.ch <- struct{}{}
			want := c.page
			if !on {
				want = c.term
			}
			select {
			case got := <-events:
				if got != want {
					t.Fatalf("%q, want %q", got, want)
				}
			case <-time.After(time.Second):
				t.Fatal("waiting for", want)
			}
		}
	}
}

type fakePages struct {
	n       int
	line    string
	pending bool
}

func (f fakePages) PageURL(path string) string { return "http://127.0.0.1:7742" + path }
func (f fakePages) DeviceCount() int           { return f.n }
func (f fakePages) ReachLine() string          { return f.line }
func (f fakePages) RestoreReview(context.Context) api.RestoreReview {
	return api.RestoreReview{Pending: f.pending}
}

func TestPageTitles(t *testing.T) {
	got := titlesFor(context.Background(), fakePages{n: 2, line: "Reachable at mac.tail1234.ts.net", pending: true})
	if got.devices != "Devices (2)" || got.reach != "Reachable at mac.tail1234.ts.net" || !got.review {
		t.Fatalf("%+v", got)
	}
	if got := titlesFor(context.Background(), nil); got.reach != "Only on this Mac" || got.review {
		t.Fatalf("%+v", got)
	}
	// A machine whose address moved says so where the address would be.
	if got := titlesFor(context.Background(), fakePages{line: "Mirrin moved to another machine on 2 Oct"}); got.reach != "Mirrin moved to another machine on 2 Oct" {
		t.Fatalf("%+v", got)
	}
}
