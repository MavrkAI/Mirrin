package main

import (
	"context"
	"log/slog"
	"os/exec"
	"strconv"
	"time"
)

// Bounds on abuse.notify_command.
const (
	notifyTimeout = 30 * time.Second
	maxNotify     = 4 // runs at once; more are dropped, with a log line
	maxNotifyLog  = 512
)

// suspendNotifier returns the relay's OnSuspend hook for
// abuse.notify_command: it runs `<command> <handle> <networks>`, with no
// shell, in the background, so a suspension reaches the operator (the
// abuse inbox, for Mirrin Cloud) and not only the log. It never blocks the
// relay.
func suspendNotifier(command string, log *slog.Logger) func(handle string, networks int) {
	sem := make(chan struct{}, maxNotify)
	return func(handle string, networks int) {
		select {
		case sem <- struct{}{}:
		default:
			log.Error("relay: abuse notify skipped; too many still running", "handle", handle)
			return
		}
		go func() {
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
			defer cancel()
			out, err := exec.CommandContext(ctx, command, handle, strconv.Itoa(networks)).CombinedOutput()
			if err != nil {
				if len(out) > maxNotifyLog {
					out = out[:maxNotifyLog]
				}
				log.Error("relay: abuse notify", "handle", handle, "command", command, "err", err, "output", string(out))
				return
			}
			log.Info("relay: abuse notify sent", "handle", handle)
		}()
	}
}
