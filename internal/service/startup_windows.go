//go:build windows

package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/homelock"
)

// newStartup is this user's Startup entry for this home and program.
// dataDir holds the home's claim, which tells its twin from another
// profile's.
func newStartup(dataDir string, secretEnvs []string, up func() bool, out io.Writer) (startup, error) {
	exe, err := os.Executable()
	if err != nil {
		return startup{}, err
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return startup{}, errors.New("can't find your Startup folder (APPDATA isn't set)")
	}
	lock := homelock.Path(dataDir)
	return startup{
		dir: startupFolder(appData), exe: exe, home: config.Home(), out: out, up: up,
		wait: 20 * time.Second, secretEnvs: secretEnvs,
		launch:   launchDetached,
		stopTwin: func() error { return stopTrays(lock) },
		running:  func() bool { return len(lockHolders(lock)) > 0 },
		elevated: func() bool { return windows.GetCurrentProcessToken().IsElevated() },
	}, nil
}

// dataDir is where this home's twin keeps its claim.
func dataDir(cfg *config.Config) string {
	if cfg != nil && cfg.DataDir != "" {
		return cfg.DataDir
	}
	if c, _ := config.Load(); c != nil && c.DataDir != "" {
		return c.DataDir
	}
	return filepath.Join(config.Home(), "data")
}

// controlStartup is Control on Windows.
func controlStartup(cfg *config.Config, action string, up func() bool, out io.Writer) error {
	s, err := newStartup(dataDir(cfg), cfg.SecretEnvs(), up, out)
	if err != nil {
		return err
	}
	return s.control(action)
}

// stateStartup is State on Windows.
func stateStartup() (installed, running bool) {
	s, err := newStartup(dataDir(nil), nil, nil, io.Discard)
	if err != nil {
		return false, false
	}
	return s.installed(), s.running()
}

const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

// launchDetached starts `exe tray` with env, with no console, outliving
// this command.
func launchDetached(exe string, env []string) error {
	cmd := exec.Command(exe, "tray")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: detachedProcess | createNewProcessGroup}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// processList is "pid<TAB>command line" for each mirrin.exe running.
func processList() string {
	script := `Get-CimInstance Win32_Process -Filter "Name='` + Name + `.exe'" | ForEach-Object { "$($_.ProcessId)` + "`t" + `$($_.CommandLine)" }`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(b)
}

// stopTrays quits this home's tray and waits for it to go. A twin of this
// home it can't quit (a `mirrin run` in a terminal) is an error, so a
// restart doesn't start a second one.
func stopTrays(lock string) error {
	holders := lockHolders(lock)
	if len(holders) == 0 {
		return nil
	}
	pids := ownTrays(processList(), os.Getpid(), holders)
	var failed []string
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			if err := p.Kill(); err != nil {
				failed = append(failed, strconv.Itoa(pid))
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("process %s wouldn't quit; use Quit in Mirrin's tray menu", strings.Join(failed, ", "))
	}
	for i := 0; i < 20 && len(lockHolders(lock)) > 0; i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if left := lockHolders(lock); len(left) > 0 {
		ids := make([]string, len(left))
		for i, pid := range left {
			ids[i] = strconv.Itoa(pid)
		}
		return fmt.Errorf("Mirrin is running from this home in another window (process %s); quit it there first", strings.Join(ids, ", "))
	}
	return nil
}

var (
	modRstrtmgr             = windows.NewLazySystemDLL("rstrtmgr.dll")
	procRmStartSession      = modRstrtmgr.NewProc("RmStartSession")
	procRmRegisterResources = modRstrtmgr.NewProc("RmRegisterResources")
	procRmGetList           = modRstrtmgr.NewProc("RmGetList")
	procRmEndSession        = modRstrtmgr.NewProc("RmEndSession")
)

// rmProcessInfo is RM_PROCESS_INFO.
type rmProcessInfo struct {
	PID              uint32
	StartTime        windows.Filetime
	AppName          [256]uint16
	ServiceShortName [64]uint16
	AppType          uint32
	AppStatus        uint32
	TSSessionID      uint32
	Restartable      int32
}

// lockHolders are the processes, other than this one, that have path (a
// home's claim) open: its running twin. Windows' Restart Manager finds
// them. Nothing is found when path was never claimed.
func lockHolders(path string) []int {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	var session uint32
	var key [33]uint16 // CCH_RM_SESSION_KEY + 1
	if r, _, _ := procRmStartSession.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); r != 0 {
		return nil
	}
	defer procRmEndSession.Call(uintptr(session))
	files := []*uint16{name}
	if r, _, _ := procRmRegisterResources.Call(uintptr(session), 1, uintptr(unsafe.Pointer(&files[0])), 0, 0, 0, 0); r != 0 {
		return nil
	}
	infos := make([]rmProcessInfo, 8)
	for tries := 0; tries < 4; tries++ {
		var needed, reasons uint32
		n := uint32(len(infos))
		r, _, _ := procRmGetList.Call(uintptr(session), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&n)),
			uintptr(unsafe.Pointer(&infos[0])), uintptr(unsafe.Pointer(&reasons)))
		if r == uintptr(windows.ERROR_MORE_DATA) {
			infos = make([]rmProcessInfo, needed+4)
			continue
		}
		if r != 0 {
			return nil
		}
		var out []int
		self := os.Getpid()
		for _, in := range infos[:n] {
			if pid := int(in.PID); pid != self {
				out = append(out, pid)
			}
		}
		return out
	}
	return nil
}
