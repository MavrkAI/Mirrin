// Package tailscale talks only to the local Tailscale CLI.
package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/procenv"
)

const AdminURL = "https://login.tailscale.com/admin/dns"

type CLI struct {
	Path string
	Dir  string
}
type Status struct {
	BackendState string
	TailscaleIPs []string
	Self         struct{ DNSName string }
	CertDomains  []string
	// Peer is the tailnet's other devices, by node key.
	Peer map[string]PeerStatus
}

// PeerStatus is another device on the tailnet, as `tailscale status --json`
// describes it.
type PeerStatus struct {
	HostName string
	DNSName  string
	OS       string // iOS, android, macOS, linux, windows…
	Online   bool
}

func (c *CLI) Run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	path, err := cliPath(c.Path, runtime.GOOS, exec.LookPath, os.Stat)
	if err != nil {
		return nil, problem("could not find the Tailscale command; install its CLI or open the Tailscale app, then check "+AdminURL, err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = procenv.Base()
	cmd.Dir = c.Dir
	b, err := cmd.Output()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			return nil, problem("could not find the Tailscale command; install its CLI or open the Tailscale app, then check "+AdminURL, err)
		}
		return nil, problem("Tailscale is not ready; open Tailscale and sign in, then check HTTPS at "+AdminURL, err)
	}
	return b, nil
}
func (c *CLI) Status(ctx context.Context) (Status, error) {
	var s Status
	b, err := c.Run(ctx, "status", "--json")
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return s, problem("could not read Tailscale status; update Tailscale and try again", err)
	}
	if s.BackendState != "Running" {
		return s, problem("Tailscale is still starting or signed out; open Tailscale and check HTTPS at "+AdminURL, nil)
	}
	s.Self.DNSName = strings.TrimSuffix(s.Self.DNSName, ".")
	if len(s.CertDomains) == 0 || s.Self.DNSName == "" {
		return s, problem("enable HTTPS certificates in your tailnet at "+AdminURL, nil)
	}
	for _, d := range s.CertDomains {
		if d == s.Self.DNSName {
			return s, nil
		}
	}
	return s, problem("enable HTTPS for this machine at "+AdminURL, nil)
}

// Problem separates the health message from the diagnostic retained in logs.
type Problem struct {
	Message string
	Cause   error
}

func (p *Problem) Error() string { return p.Message }
func (p *Problem) Unwrap() error { return p.Cause }
func problem(message string, cause error) error {
	if cause != nil {
		slog.Warn("Tailscale command failed", "err", cause)
	}
	return &Problem{Message: message, Cause: cause}
}
func cliPath(explicit, goos string, look func(string) (string, error), stat func(string) (os.FileInfo, error)) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	path, err := look("tailscale")
	if err == nil {
		return path, nil
	}
	if goos == "darwin" {
		const app = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		if info, e := stat(app); e == nil && !info.IsDir() {
			return app, nil
		}
	}
	return "", fmt.Errorf("Tailscale CLI lookup: %w", err)
}
