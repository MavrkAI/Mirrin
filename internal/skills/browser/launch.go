package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/health"
)

// launchTimeout bounds starting Chrome, so a start that hangs (a keychain
// prompt nobody can see, a stuck profile) ends in a plain error instead of a
// twin that stops answering.
var launchTimeout = 30 * time.Second

// errLaunchTimeout is Chrome not answering within launchTimeout.
var errLaunchTimeout = errors.New("chrome did not start in time")

// killWait bounds waiting for a hung Chrome to exit once it is killed.
var killWait = 10 * time.Second

// stopChrome ends a Chrome that failed to start. One that hung is killed and
// waited for (up to killWait), so a second try doesn't meet it still holding
// the profile; anything else is ended in the background.
func stopChrome(err error, cancel context.CancelFunc) {
	if !errors.Is(err, errLaunchTimeout) {
		go cancel()
		return
	}
	done := make(chan struct{})
	go func() { cancel(); close(done) }() // kills the process, then waits for it
	select {
	case <-done:
	case <-time.After(killWait):
	}
}

// startChromeRetrying is startChrome, tried once more after a start that
// hung: a stuck first start (a slow disk, a profile lock) often clears. One
// put down to a locked keychain with nobody at the screen would only hang
// again, so it isn't retried; with someone there the prompt shows, and they
// may answer it.
func (s *Session) startChromeRetrying(headed bool, proxy, ua string) (context.Context, context.CancelFunc, chromeVersion, error) {
	ctx, cancel, ver, err := s.startChrome(headed, proxy, ua)
	var le *launchError
	if err == nil || !errors.Is(err, errLaunchTimeout) || (errors.As(err, &le) && le.keychain && !someoneAtScreen()) {
		return ctx, cancel, ver, err
	}
	beforeRetry()
	s.log.Warn("browser start hung; trying once more")
	return s.startChrome(headed, proxy, ua)
}

// someoneAtScreen is atConsole; beforeRetry runs between a hung start and
// its retry. Variables so tests can stand in for them.
var (
	someoneAtScreen = atConsole
	beforeRetry     = func() {}
)

// launchError is Chrome failing to start, in words, with what to do.
type launchError struct {
	what, fix string
	err       error
	keychain  bool // put down to a locked keychain, which may since have been unlocked
}

func (e *launchError) Error() string {
	if e.fix == "" {
		return e.what
	}
	return e.what + " " + e.fix
}

func (e *launchError) Unwrap() error { return e.err }

// unlockFix is how to unlock a Mac's login keychain from a distance.
const unlockFix = "Log in or unlock the keychain on the Mac's screen (Screen Sharing works too), or run `security unlock-keychain ~/Library/Keychains/login.keychain-db` in Terminal."

// explainLaunch puts a failed start in words.
func explainLaunch(ctx context.Context, err error) *launchError {
	switch {
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "executable file not found"):
		return &launchError{what: "I couldn't find Google Chrome on this computer, so I can't use websites yet.", fix: "Install it from https://www.google.com/chrome/ and ask me again.", err: err}
	case errors.Is(err, errLaunchTimeout):
		if keychainLocked(ctx) {
			return &launchError{what: "Chrome didn't start: this Mac's login keychain is locked, so Chrome is waiting at a keychain password prompt, which nobody may be at the screen to answer.", fix: unlockFix + " Then ask me again.", err: err, keychain: true}
		}
		fix := "Restart the computer and ask me again."
		if runtime.GOOS == "darwin" {
			fix = "If a keychain password prompt is showing on the Mac's screen, answer it; otherwise restart the Mac and ask me again."
		}
		secs := int(launchTimeout.Round(time.Second).Seconds())
		unit := "seconds"
		if secs == 1 {
			unit = "second"
		}
		return &launchError{what: fmt.Sprintf("Chrome didn't start within %d %s, so I stopped waiting.", secs, unit), fix: fix, err: err}
	}
	if strings.Contains(err.Error(), "existing browser session") {
		return &launchError{what: "Chrome didn't start: another copy of my browser is still open with the same profile.", fix: "Quit that Chrome window (or restart the computer), then ask me again.", err: err}
	}
	// The details go to the log; the owner gets what to do.
	return &launchError{what: "Chrome didn't start.", fix: "Restart Mirrin and ask me again; if it keeps happening, reinstall Google Chrome.", err: err}
}

// launches is how the last start went, for the health check.
var launches struct {
	sync.Mutex
	failed   *launchError
	failedAt time.Time
}

// noteLaunch records a start: nil for one that worked.
func noteLaunch(le *launchError) {
	launches.Lock()
	defer launches.Unlock()
	launches.failed = le
	launches.failedAt = time.Now()
}

// lastLaunchFailure is the most recent start, if it failed.
func lastLaunchFailure() (*launchError, time.Time) {
	launches.Lock()
	defer launches.Unlock()
	return launches.failed, launches.failedAt
}

// findChrome is the Chrome to run, or "" when none is installed. It looks
// where chromedp does, and in ~/Applications too. A variable for tests.
var findChrome = func() string {
	var locations []string
	switch runtime.GOOS {
	case "darwin":
		locations = []string{
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}
		if home, err := os.UserHomeDir(); err == nil {
			locations = append(locations, filepath.Join(home, "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"))
		}
	case "windows":
		locations = []string{
			"chrome", "chrome.exe",
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			filepath.Join(os.Getenv("USERPROFILE"), `AppData\Local\Google\Chrome\Application\chrome.exe`),
			filepath.Join(os.Getenv("USERPROFILE"), `AppData\Local\Chromium\Application\chrome.exe`),
		}
	default:
		locations = []string{
			"headless_shell", "headless-shell", "chromium", "chromium-browser",
			"google-chrome", "google-chrome-stable", "google-chrome-beta", "google-chrome-unstable",
			"/usr/bin/google-chrome", "/usr/local/bin/chrome", "/snap/bin/chromium", "chrome",
		}
	}
	for _, p := range locations {
		if found, err := exec.LookPath(p); err == nil {
			return found
		}
	}
	return ""
}

// keychainLocked reports whether this Mac's login keychain is locked while
// nobody is logged in at its screen: the always-on Mac after a restart, or a
// twin started over SSH. Chrome then stops at a keychain prompt nobody can
// see. With someone at the screen only a status read that can't prompt is
// used (a cgo build on a Mac), as asking security(1) could itself show a
// prompt. A variable so tests can stand in for it.
var keychainLocked = func(ctx context.Context) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	if atConsole() {
		// Only a prompt-free status read (keychain_darwin_cgo.go): a Mac
		// that logs in on its own has someone "at" a screen nobody watches.
		locked, ok := quietKeychainLocked()
		return ok && locked
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	path := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	if _, err := os.Stat(path); err != nil {
		if path = filepath.Join(home, "Library", "Keychains", "login.keychain"); !exists(path) {
			return false
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "security", "show-keychain-info", path).CombinedOutput()
	return err != nil && lockedText(string(out))
}

// lockedText recognises security(1) failing on a locked keychain it may not
// prompt for ("User interaction is not allowed.").
func lockedText(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "interaction is not allowed") || strings.Contains(s, "keychain is locked") || strings.Contains(s, "-25308")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Health is the browser's self-check for the menu bar and `mirrin doctor`:
// Chrome is installed, the last start worked, on a Mac nobody is logged in
// to the login keychain is unlocked, and the browser can follow this
// computer's proxy.
func Health() health.Check {
	return health.Func("browser", "Browser", func(ctx context.Context) (health.State, string, string) {
		if le, at := lastLaunchFailure(); le != nil && time.Since(at) < 24*time.Hour {
			// A keychain unlocked since then is no longer in the way.
			if !le.keychain || keychainLocked(ctx) {
				return health.Fail, le.what, le.fix
			}
		}
		if findChrome() == "" {
			return health.Warn, "Google Chrome isn't installed, so I can't use websites.", "Install it from https://www.google.com/chrome/"
		}
		if keychainLocked(ctx) {
			return health.Warn, "This Mac's login keychain is locked, so Chrome would stop at a keychain password prompt, which nobody may be at the screen to answer.", unlockFix
		}
		if what, fix := proxyWarning(); what != "" {
			return health.Warn, what, fix
		}
		return health.OK, "Chrome ready", ""
	}, nil)
}

// envProxies are the settings the guard reads before the system's.
var envProxies = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"}

// proxyWarning describes a proxy setting the browser can't follow, with what
// to do; "" when there is none.
func proxyWarning() (what, fix string) {
	set := false
	for _, name := range envProxies {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			continue
		}
		set = true
		if !strings.Contains(v, "://") {
			v = "http://" + v
		}
		if u, err := url.Parse(v); err != nil || usableProxy(u) != nil {
			return fmt.Sprintf("%s is set to a proxy my browser can't use, so websites may not load.", name),
				"Set it to an http://, https:// or socks5:// proxy address, then restart Mirrin."
		}
	}
	if set {
		return "", "" // the owner's own choice comes first
	}
	if p := readSystemProxy(); p != nil && p.unsupported != "" {
		return fmt.Sprintf("This computer sends web traffic through a proxy set by %s, which my browser can't follow yet, so it connects directly and some sites may not load.", p.unsupported),
			"If websites don't load in my browser, set HTTPS_PROXY and HTTP_PROXY for Mirrin to your proxy's address (like http://proxy.example.com:8080), then restart Mirrin."
	}
	return "", ""
}
