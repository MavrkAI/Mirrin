package reach

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/tailscale"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
)

type snapshot struct {
	state        health.State
	message, fix string
	source       tlsmgr.Source
}
type dependencies struct {
	prepare func(context.Context, Config) (tlsmgr.Source, string, error)
	listen  func(string, string) (net.Listener, error)
	wait    func(context.Context, time.Duration) bool
}

func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func supervise(ctx context.Context, s *api.Server, c Config, report func(snapshot), d dependencies) error {
	if c.Mode == "" || c.Mode == "off" {
		return nil
	}
	if c.Mode != "tailscale" && c.Mode != "files" {
		return fmt.Errorf("that reach mode is planned; choose tailscale, files or off")
	}
	if report == nil {
		report = func(snapshot) {}
	}
	delay := time.Second
	for ctx.Err() == nil {
		report(snapshot{state: health.Warn, message: "starting remote HTTPS", fix: "Mirrin will retry automatically"})
		started := time.Now()
		err := attempt(ctx, s, c, report, d)
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("remote HTTPS will retry", "err", err)
		message := "remote HTTPS is unavailable; check the certificate and listen address"
		var ts *tailscale.Problem
		if errors.As(err, &ts) {
			message = ts.Error()
		}
		report(snapshot{state: health.Fail, message: message, fix: "Mirrin will retry automatically; check `mirrin reach status`"})
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		if !d.wait(ctx, delay) {
			return nil
		}
		delay = min(delay*2, time.Minute)
	}
	return nil
}
func attempt(ctx context.Context, s *api.Server, c Config, report func(snapshot), d dependencies) error {
	src, addr, err := d.prepare(ctx, c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	certDone := make(chan error, 1)
	go func() { certDone <- src.Run(ctx) }()
	ln, err := d.listen("tcp", addr)
	if err != nil {
		cancel()
		<-certDone
		return err
	}
	defer ln.Close()
	host := src.Hostnames()[0]
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	url := "https://" + host
	if port != "443" {
		url = "https://" + net.JoinHostPort(host, port)
	}
	report(snapshot{state: health.OK, message: "remote HTTPS is running at " + url, source: src})
	served := make(chan error, 1)
	go func() {
		served <- s.ServeRemote(ctx, ln, src, api.RemoteOptions{Via: c.Mode, AllowAdmin: c.AdminRemote})
	}()
	select {
	case err = <-certDone:
		cancel()
		ln.Close()
		<-served
	case err = <-served:
		cancel()
		<-certDone
	case <-ctx.Done():
		cancel()
		ln.Close()
		<-served
		<-certDone
	}
	return err
}
