//go:build (darwin && cgo) || linux || windows

package tray

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/procenv"
)

type welcomeBackend interface {
	NeedsWelcome(context.Context) bool
	WelcomeURL() string
}

// openWelcome waits for the local listener, once per tray launch. Completion
// is persisted by Hello, so a half-finished welcome is offered on the next run.
func openWelcome(ctx context.Context, b Backend) {
	w, ok := b.(welcomeBackend)
	if !ok || !w.NeedsWelcome(ctx) {
		return
	}
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for i := 0; i < 40; i++ {
		u := w.WelcomeURL()
		if u == "" {
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}
		probe := strings.SplitN(u, "/welcome", 2)[0] + "/healthz"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe, nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusOK {
				openWelcomeWindow(u)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func welcomeChromeArgs(url, profile string) []string {
	return []string{"--use-mock-keychain", "--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check", "--app=" + url}
}
func openWelcomeWindow(url string) {
	candidates := []string{"google-chrome", "chromium", "chrome"}
	if runtime.GOOS == "darwin" {
		candidates = append([]string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}, candidates...)
	}
	for _, candidate := range candidates {
		exe, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		profile, err := os.MkdirTemp("", "mirrin-welcome-")
		if err != nil {
			break
		}
		cmd := exec.Command(exe, welcomeChromeArgs(url, profile)...)
		cmd.Env = procenv.Base()
		if err := cmd.Start(); err != nil {
			os.RemoveAll(profile)
			continue
		}
		go func() { _ = cmd.Wait(); _ = os.RemoveAll(profile) }()
		return
	}
	openURL(url)
}
