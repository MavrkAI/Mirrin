// Package reach orchestrates the free remote listeners: Tailscale and files
// here, a self-hosted relay in relay.go. Cloud is a reserved mode.
package reach

import (
	"context"
	"fmt"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tailscale"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
)

type Config = config.Reach

func DefaultListen(goos, ip string, privileged ...bool) string {
	if goos == "linux" && !(len(privileged) > 0 && privileged[0]) {
		return net.JoinHostPort(ip, "7743")
	}
	return net.JoinHostPort(ip, "443")
}
func Serve(ctx context.Context, s *api.Server, c Config) error {
	return supervise(ctx, s, c, nil, dependencies{prepare: prepare, listen: net.Listen, wait: pause})
}
func prepare(ctx context.Context, c Config) (tlsmgr.Source, string, error) {
	var src tlsmgr.Source
	var err error
	addr := c.Listen
	switch c.Mode {
	case "files":
		src = tlsmgr.Files(c.CertFile, c.KeyFile)
		if _, err = src.GetCertificate(nil); err != nil {
			return nil, "", err
		}
		if addr == "" {
			addr = ":7743"
		}
	case "tailscale":
		cli := &tailscale.CLI{}
		status, e := cli.Status(ctx)
		if e != nil {
			return nil, "", e
		}
		if len(status.TailscaleIPs) == 0 {
			return nil, "", &tailscale.Problem{Message: "Tailscale has no address yet; open Tailscale and sign in"}
		}
		if addr == "" {
			addr = DefaultListen(runtime.GOOS, status.TailscaleIPs[0], canBindHTTPS())
		}
		src, err = tlsmgr.Tailscale(ctx, cli)
		if err != nil {
			return nil, "", err
		}
	default:
		return nil, "", fmt.Errorf("Reach mode %q is planned. Use tailscale, files or off", c.Mode)
	}
	if len(src.Hostnames()) == 0 {
		return nil, "", fmt.Errorf("the certificate needs a DNS name; choose a certificate for this computer's HTTPS hostname")
	}
	return src, addr, nil
}

// Linux advertises effective capabilities in /proc; checking the kernel's
// value avoids guessing from the username or trying a privileged port.
func bindCapability(status []byte) bool {
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "CapEff:") {
			n, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
			return err == nil && n&(1<<10) != 0
		}
	}
	return false
}
func canBindHTTPS() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	b, err := os.ReadFile("/proc/self/status")
	return err == nil && bindCapability(b)
}
