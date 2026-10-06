//go:build (darwin && cgo) || linux || windows

package tray

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"fyne.io/systray"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/procenv"
	"github.com/MavrkAI/Mirrin/internal/service"
)

func buildExtras(ctx context.Context, name string, backend Backend) func() {
	pages, _ := any(backend).(pagesBackend)
	reachLine := systray.AddMenuItem("Only on this Mac", "Where other devices reach "+name)
	reachLine.Disable()
	addPhone := systray.AddMenuItem("Add your phone…", "One code: pair, install, notifications and Face ID")
	devices := systray.AddMenuItem("Devices…", "Your paired devices: rename or remove them")
	review := systray.AddMenuItem("Review devices after restore…", "Remove any device you don't recognise")
	review.Hide()
	backup := systray.AddMenuItem("Backup…", "Encrypted backups and your Recovery Kit")
	reach := systray.AddMenuItem("Reach from anywhere…", "How your phone reaches "+name+" away from home")
	spend := systray.AddMenuItem("Model: checking spending…", "")
	logs := systray.AddMenuItem("Open logs", "")
	report := systray.AddMenuItem("Report a problem…", "Make a report to review before sharing")

	wireExtras(ctx, extraClicks{spend: spend.ClickedCh, logs: logs.ClickedCh, report: report.ClickedCh, devices: devices.ClickedCh, backup: backup.ClickedCh,
		addPhone: addPhone.ClickedCh, reach: reach.ClickedCh, review: review.ClickedCh}, extraActions{
		terminal: openTerminal,
		logs:     func() { revealPath(filepath.Join(config.Home(), "logs"), false) },
		report:   func() error { return reportProblem(ctx, runReport, func(p string) { revealPath(p, true) }) },
		failure:  func(err error) { notify(name, err.Error()) },
		page: func(path string) bool {
			if pages == nil {
				return false
			}
			u := pages.PageURL(path)
			if u == "" {
				return false
			}
			openURL(u)
			return true
		},
	})
	spendNow := spendRefresh(ctx, backend.Spend, spend.SetTitle, time.Now)
	var next time.Time
	var busy atomic.Bool
	return func() {
		spendNow()
		if pages == nil || time.Now().Before(next) || !busy.CompareAndSwap(false, true) {
			return
		}
		next = time.Now().Add(30 * time.Second)
		// The reach line may ask Tailscale (up to 5 s): work the titles out
		// off the refresh path and apply them when they arrive.
		go func() {
			defer busy.Store(false)
			t := titlesFor(ctx, pages)
			if ctx.Err() != nil {
				return
			}
			reachLine.SetTitle(t.reach)
			devices.SetTitle(t.devices)
			if t.review {
				review.Show()
			} else {
				review.Hide()
			}
		}()
	}
}

func revealPath(path string, file bool) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		args := []string{path}
		if file {
			args = []string{"-R", path}
		}
		cmd = exec.Command("open", args...)
	case "windows":
		if file {
			cmd = exec.Command("explorer", "/select,", path)
		} else {
			cmd = exec.Command("explorer", path)
		}
	default:
		if file {
			path = filepath.Dir(path)
		}
		cmd = exec.Command("xdg-open", path)
	}
	cmd.Env = procenv.Base()
	_ = cmd.Run()
}

func maintainServiceLogs(ctx context.Context) func() { return service.MaintainLogs(ctx) }
