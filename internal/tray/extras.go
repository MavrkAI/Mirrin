package tray

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/procenv"
)

func desktopPresent() bool { return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" }

func terminalLine(exe string, args []string) string {
	return "MIRRIN_HOME=" + shellQuote(config.Home()) + " " + shellLine(exe, args)
}

func spendTitle(sp memory.Spend, err error) string {
	if err != nil {
		return "Model spending: can't add it up · type mirrin usage in Terminal"
	}
	title := fmt.Sprintf("Model: US$%.2f this month (estimate)", sp.ToDate)
	if sp.Budget > 0 {
		title += fmt.Sprintf(" / US$%.2f budget", sp.Budget)
	}
	return title
}

// reportProblem uses the CLI's existing redaction and report flow. Its
// output names the exact file just written, even if reports already exist.
func reportProblem(ctx context.Context, run func(context.Context, string, ...string) ([]byte, error), reveal func(string)) error {
	exe, args := selfCommand("report")
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := run(ctx, exe, args...)
	if err != nil {
		return fmt.Errorf("Couldn't make a report. Try again, or type mirrin report in Terminal.")
	}
	path, err := reportPath(string(out), config.Home())
	if err != nil {
		return err
	}
	reveal(path)
	return nil
}
func reportPath(out, home string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		path := strings.TrimSpace(line)
		if filepath.IsAbs(path) && filepath.Dir(path) == filepath.Join(home, "reports") &&
			strings.HasPrefix(filepath.Base(path), "mirrin-report-") && strings.HasSuffix(path, ".txt") {
			return path, nil
		}
	}
	return "", fmt.Errorf("Couldn't find the report. It is in the reports folder inside Mirrin's folder.")
}
func runReport(ctx context.Context, exe string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(procenv.Base(), "MIRRIN_HOME="+config.Home())
	return cmd.Output()
}

// Desktop helpers need the session environment and the selected twin, never
// the daemon's API keys. Reap each process after launching it.
func startDesktop(cmd *exec.Cmd) {
	cmd.Env = append(procenv.Base(), "MIRRIN_HOME="+config.Home())
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
}

// The menu's event wiring is separate from native systray handles so tests
// can click every entry without starting a desktop session.
type extraClicks struct {
	spend, logs, report, devices, backup <-chan struct{}
	// The device and safety pages (a nil channel is never clicked).
	addPhone, reach, review <-chan struct{}
}
type extraActions struct {
	terminal func(...string)
	logs     func()
	report   func() error
	failure  func(error)
	// page opens one of the twin's pages on this computer, reporting false
	// when it can't (the API is off): the terminal command stands in.
	page func(path string) bool
}

// pageOr opens path, or runs the terminal command when there is no page.
func (a extraActions) pageOr(path string, args ...string) {
	if a.page != nil && a.page(path) {
		return
	}
	a.terminal(args...)
}

func wireExtras(ctx context.Context, clicks extraClicks, actions extraActions) {
	go func() {
		reportDone := make(chan error, 1)
		reporting := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-clicks.spend:
				actions.terminal("usage")
			case <-clicks.logs:
				actions.logs()
			case <-clicks.devices:
				actions.pageOr("/devices/page", "devices", "list")
			case <-clicks.backup:
				actions.pageOr("/backup", "backup", "status")
			case <-clicks.addPhone:
				actions.pageOr("/devices/add", "pair", "--screen")
			case <-clicks.reach:
				actions.pageOr("/reach", "reach", "status")
			case <-clicks.review:
				actions.pageOr("/restore/review", "devices", "list")
			case <-clicks.report:
				if !reporting {
					reporting = true
					go func() { reportDone <- actions.report() }()
				}
			case err := <-reportDone:
				reporting = false
				if err != nil {
					actions.failure(err)
				}
			}
		}
	}()
}

// pagesBackend is what the device and safety entries read from the twin.
// A backend without it keeps the terminal commands.
type pagesBackend interface {
	PageURL(path string) string
	DeviceCount() int
	ReachLine() string
	RestoreReview(ctx context.Context) api.RestoreReview
}

// pageTitles are the device and safety entries' titles now.
type pageTitles struct {
	devices, reach string
	review         bool // a restore's device review is waiting
}

func titlesFor(ctx context.Context, b pagesBackend) pageTitles {
	if b == nil {
		return pageTitles{devices: "Devices…", reach: "Only on this Mac"}
	}
	return pageTitles{devices: fmt.Sprintf("Devices (%d)", b.DeviceCount()), reach: b.ReachLine(), review: b.RestoreReview(ctx).Pending}
}

func spendRefresh(ctx context.Context, spend func(context.Context) (memory.Spend, error), title func(string), now func() time.Time) func() {
	var next time.Time
	return func() {
		at := now()
		if at.Before(next) {
			return
		}
		next = at.Add(time.Minute)
		sp, err := spend(ctx)
		title(spendTitle(sp, err))
	}
}
