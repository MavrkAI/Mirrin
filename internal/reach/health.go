package reach

import (
	"context"
	"log/slog"
	"net"
	"sync"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
)

// Start supervises remote access independently of the loopback listener.
func Start(ctx context.Context, s *api.Server, c Config, legacy bool, m *health.Monitor) {
	var mu sync.Mutex
	current := snapshot{state: health.Off, message: "remote HTTPS is off", fix: "Run `mirrin reach use tailscale` to connect other devices"}
	if c.Mode != "" && c.Mode != "off" {
		current = snapshot{state: health.Warn, message: "starting remote HTTPS", fix: "Mirrin will retry automatically"}
	}
	report := func(v snapshot) { mu.Lock(); current = v; mu.Unlock() }
	if m != nil {
		m.Add(health.Func("reach", "Remote access", func(context.Context) (health.State, string, string) {
			mu.Lock()
			v := current
			mu.Unlock()
			return healthSnapshot(v, legacy)
		}, nil))
	}
	if c.StayAwake {
		go func() {
			if err := StayAwake(ctx); err != nil {
				slog.Warn("keep awake failed", "err", err)
			}
		}()
	}
	go func() {
		if err := supervise(ctx, s, c, report, dependencies{prepare: prepare, listen: net.Listen, wait: pause}); err != nil {
			report(snapshot{state: health.Fail, message: err.Error(), fix: "Choose tailscale, files or off"})
		}
	}()
}

func healthSnapshot(v snapshot, legacy bool) (health.State, string, string) {
	if v.source != nil {
		if err := tlsmgr.Warning(v.source); err != nil {
			return health.Warn, err.Error(), "Check your certificate source; Mirrin will retry automatically"
		}
	}
	if legacy && v.state != health.Fail {
		return health.Warn, v.message + "; legacy remote access uses plain HTTP", "Run `mirrin reach use tailscale` for HTTPS"
	}
	return v.state, v.message, v.fix
}
