package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/procenv"
)

var welcomeHandoff sync.Mutex

// The helper gets words through stdin, never argv, environment or disk.
// Only the tray installs this hook: a service manager could restart the old
// twin while the helper is waiting for its home to become free.
func startWelcomeRestore(r api.WelcomeRestoreRequest) error {
	if config.UnderServiceManager() {
		return &api.HumanError{Sentence: "The background service needs to stop before restoring.", Fix: "Run mirrin service stop, reopen Mirrin, then choose I already have a twin."}
	}
	if !welcomeHandoff.TryLock() {
		return &api.HumanError{Sentence: "Your twin is already moving in.", Fix: "Wait for Mirrin to reopen."}
	}
	ok := false
	defer func() {
		if !ok {
			welcomeHandoff.Unlock()
		}
	}()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "welcome-restore-worker")
	// The home this process settled on, whichever variable named it.
	cmd.Env = append(procenv.Base(), "MIRRIN_HOME="+config.Home())
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		input.Close()
		return err
	}
	err = json.NewEncoder(input).Encode(r)
	input.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	ok = true
	// Let the HTTP response reach the window before leaving. Process exit
	// releases every worker, DB handle and the home's claim before restore.
	go func() { time.Sleep(time.Second); os.Exit(0) }()
	return nil
}

func welcomeRestoreWorker(in io.Reader) int {
	var r api.WelcomeRestoreRequest
	decoder := json.NewDecoder(io.LimitReader(in, 16<<10))
	if err := decoder.Decode(&r); err != nil || !r.Confirm {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		return 1
	}
	report, err := restoreWelcomeHome(ctx, cfg, r)
	// Clear our copy promptly. The phrase is never included in the report.
	r.Phrase = ""
	message := "Your twin is home. Review the devices below and remove any you don't recognise from Devices.\n"
	if err != nil {
		message = "Your backup couldn't be restored. Your previous twin was kept. Open Welcome and check your backup folder and 12 words, then try again.\n"
	} else {
		message += strings.Join(report.Devices, "\n") + "\n" + strings.Join(report.Checklist, "\n")
		if report.HandoverNote != "" {
			message += "\n" + report.HandoverNote
		}
		if report.Stale != "" {
			message += "\n" + report.Stale
		}
	}
	if writeErr := os.WriteFile(filepath.Join(config.Home(), "welcome-restore.txt"), []byte(message), 0o600); writeErr != nil {
		return 1
	}
	exe, exeErr := os.Executable()
	if exeErr != nil {
		return 1
	}
	cmd := exec.Command(exe, "tray")
	cmd.Env = append(procenv.Base(), "MIRRIN_HOME="+config.Home())
	if cmd.Start() != nil {
		return 1
	}
	return 0
}

func restoreWelcomeHome(ctx context.Context, cfg *config.Config, r api.WelcomeRestoreRequest) (backup.Report, error) {
	if !r.Confirm {
		return backup.Report{}, fmt.Errorf("restore wasn't confirmed")
	}
	target, phrase, err := daemon.WelcomeRestoreTarget(ctx, r)
	if err != nil {
		return backup.Report{}, err
	}
	release, err := daemon.LockHome(ctx, cfg.DataDir, 30*time.Second)
	if err != nil {
		return backup.Report{}, err
	}
	defer release()
	return backup.Restore(ctx, backup.RestoreOptions{Target: target, Phrase: phrase, Home: config.Home(), HostLabel: backup.HostLabel()})
}
