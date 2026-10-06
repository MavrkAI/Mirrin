package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/service"
	"github.com/MavrkAI/Mirrin/internal/tray"
)

// exitCode ends the program with a status and no further message.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// doctor runs the self-checks, on the running twin when there is one, and says
// first whether a twin is running at all. Any failed check exits 1, so scripts
// can tell.
func doctor(ctx context.Context, cfg *config.Config) error {
	var rep health.Report
	twin := health.Result{Name: "twin", Label: "Twin"}
	if c := api.Connect(cfg.API.Listen, cfg.DataDir); c != nil {
		if r, err := c.RunHealth(ctx); err == nil {
			rep, twin.State, twin.Detail = r, health.OK, "running"
		}
	}
	if twin.State == "" {
		d, err := daemon.New(cfg, daemon.Options{Headless: true, Version: version, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			return err
		}
		defer d.Close()
		rep = d.RunHealth(ctx)
		twin = twinState(cfg)
	}
	rep.Results = append([]health.Result{twin}, rep.Results...)
	printHealth(rep)
	for _, r := range rep.Results {
		if r.State == health.Fail {
			return exitCode(1)
		}
	}
	return nil
}

// twinState describes the background twin when it isn't answering locally.
func twinState(cfg *config.Config) health.Result {
	r := health.Result{Name: "twin", Label: "Twin"}
	installed, running := service.State()
	switch {
	case running && cfg.API.Listen == "":
		r.State, r.Detail = health.OK, "running in the background (its local connection is off, so pages and terminals can't join it)"
	case running:
		r.State, r.Detail, r.Fix = health.Fail, "running in the background, but not answering", "mirrin service restart"
	case installed:
		r.State, r.Detail, r.Fix = health.Fail, "installed, but not running", "mirrin service restart"
		if e := service.LastError(); e != "" {
			r.Detail += " (last error: " + e + ")"
		}
	default:
		r.State, r.Detail, r.Fix = health.Warn, "not running in the background", "mirrin service install keeps it running"
	}
	return r
}

// commands lists what mirrin answers to, for suggestions.
var commands = []string{"init", "run", "tray", "chat", "voice", "whatsapp", "calendar", "service", "identity",
	"persona", "protocols", "models", "pair", "connect", "disconnect", "doctor", "audit", "version", "help",
	"usage", "report", "memory"}

// unknownCommand says what wasn't recognised and suggests the closest command.
func unknownCommand(w io.Writer, cmd string) {
	fmt.Fprintf(w, "mirrin: %q isn't a command.", cmd)
	if s := closest(cmd, commands); s != "" {
		fmt.Fprintf(w, " Did you mean `mirrin %s`?", s)
	}
	fmt.Fprintln(w, " Run `mirrin help` to see them all.")
}

// closest returns the candidate within two edits of s (or that s starts), if any.
func closest(s string, candidates []string) string {
	s = strings.ToLower(s)
	best, bestD := "", 3
	for _, c := range candidates {
		if len(s) >= 3 && strings.HasPrefix(c, s) {
			return c
		}
		if d := editDistance(s, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// serviceMode is what the installed service runs: the menu bar app on macOS
// when this binary has one, else the headless daemon.
func serviceMode(goos string, wantTray, trayAvailable bool) string {
	if goos == "darwin" && wantTray && trayAvailable {
		return "tray"
	}
	return "run"
}

// disconnect forgets a paired remote twin; with none, it just says so. It
// first asks that twin to cut off this computer's key, and forgets the twin
// even when it can't be reached.
func disconnect(w io.Writer) error {
	if r, ok := loadRemote(); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := api.RevokeSelf(ctx, api.Target{Address: r.Address, Token: r.Token, Pins: r.Pins}); err != nil {
			fmt.Fprintln(w, "I couldn't reach the other twin to remove this computer's key; remove it there with `mirrin devices` if you want.")
		}
		cancel()
	}
	err := os.Remove(config.RemotePath())
	switch {
	case err == nil:
		fmt.Fprintln(w, "Disconnected; chat and voice use this machine's twin again.")
	case os.IsNotExist(err):
		fmt.Fprintln(w, "Not connected to another machine; chat and voice already use this one.")
		return nil
	}
	return err
}

// clientErrorReply is what a terminal shows when the running twin couldn't answer.
func clientErrorReply(err error) string {
	if msg, ok := llm.Describe(err); ok {
		return msg
	}
	return "Something went wrong: " + strings.TrimPrefix(err.Error(), "daemon: ")
}

// trayNeedsModel is the menu bar app's notice when it starts without a model.
// It points at the menu, because `mirrin` is often not on the PATH of people
// who installed the app.
const trayNeedsModel = "I need a model to think with before I can help. Choose \"Chat in terminal\" from my menu and I'll ask for a key there."

// alreadyInMenuBar handles a second menu bar app: it says where the running
// one is and quits quietly, rather than showing a second icon.
func alreadyInMenuBar(cfg *config.Config, err error) error {
	switch {
	case errors.Is(err, context.Canceled): // stopped while waiting for another copy to quit
		return nil
	case !errors.Is(err, daemon.ErrAlreadyRunning):
		return err
	}
	tray.Notify(cfg.Name, "I'm already running, in the menu bar or in the background.")
	fmt.Fprintln(os.Stderr, err)
	return nil
}

// cantJoin explains a twin that is running but that this terminal couldn't
// reach, so it can't start its own either.
func cantJoin(err error) error {
	if !errors.Is(err, daemon.ErrAlreadyRunning) {
		return err
	}
	return errors.New("Mirrin is already running, but this terminal can't reach it: its local connection is off or not answering (`mirrin doctor` says which). Set api.listen to 127.0.0.1:7742 in the settings file and restart it, or quit it to talk here")
}

// wakePhrase is how to say the wake word: "Hey Mirrin", or the word as
// configured when it already starts with "hey" (voice.WakePhrase).
func wakePhrase(word string) string { return voice.WakePhrase(word) }
