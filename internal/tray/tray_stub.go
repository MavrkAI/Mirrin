//go:build darwin && !cgo

package tray

import (
	"context"
	"errors"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/health"
)

// Backend is what the tray needs from the daemon.
type Backend interface {
	api.Backend
	Protocols() []string
	ConfigPath() string
	Config() config.Config
	UpdateConfig(mutate func(c *config.Config)) error
	PreviewVoice(ctx context.Context)
	Restart() error
	MemoryURL() string
	HealthURL() string
	UIURL() string
	ProtocolsURL() string
	Events() *events.Bus
	Health() health.Report
	RunHealth(ctx context.Context) health.Report
	Models(ctx context.Context, provider string) ([]string, error)
	SetProvider(provider string) error
	Interrupt(ctx context.Context) (string, bool)
}

// Available reports whether this build can show a menu bar icon.
func Available() bool { return false }

// Notify does nothing in a build without the menu bar.
func Notify(title, body string) {}

// Run reports that this build has no tray support.
func Run(_ context.Context, _ string, _ Backend, _ func(ctx context.Context) error) error {
	return errors.New("this binary was built without the menu bar (CGO_ENABLED=0 on macOS); use `mirrin run` or rebuild with `make build`")
}
