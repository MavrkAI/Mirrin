package agent

import (
	"context"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// recordUsage adds a model call's tokens to the day's tally in memory, so the
// owner can see what the model costs (`mirrin usage`, the Health page) and be
// warned before a surprise bill. Only counts are kept, never what was said.
// A turn cancelled after the reply arrived was still billed, so it is still
// counted.
func (a *Agent) recordUsage(ctx context.Context, chatKey string, p llm.Provider, resp *llm.Response) {
	a.log.Debug("llm", "stop", resp.StopReason, "in", resp.InputTokens, "out", resp.OutputTokens, "cache_read", resp.CacheRead, "cache_write", resp.CacheWrite)
	t := llm.Tokens{Input: resp.InputTokens, Output: resp.OutputTokens, CacheRead: resp.CacheRead, CacheWrite: resp.CacheWrite}
	if t == (llm.Tokens{}) || p == nil {
		return
	}
	day := time.Now().In(a.location()).Format(memory.DayFormat)
	if err := a.store.RecordUsage(context.WithoutCancel(ctx), day, p.Name(), memory.UsageKind(chatKey), t); err != nil {
		a.log.Warn("model usage not recorded", "err", err)
	}
}

// location is the owner's time zone, which decides what "today" is: the
// live zone the daemon keeps (Location, which follows the system as a laptop
// travels, as the heartbeat and Daemon.Spend do), else the configured one.
// The prompt's "Current time" and the usage day both go by it, so the day
// usage is kept under is the day the twin reads it back as.
func (a *Agent) location() *time.Location {
	if live := a.root; live != nil && live.Location != nil {
		return live.Location() // a turn's copy: the zone is the live agent's
	}
	if a.Location != nil {
		return a.Location()
	}
	if tz := a.cfg.User.Timezone; !config.FollowsSystem(tz) {
		if l, err := time.LoadLocation(strings.TrimSpace(tz)); err == nil {
			return l
		}
	}
	return time.Local
}
