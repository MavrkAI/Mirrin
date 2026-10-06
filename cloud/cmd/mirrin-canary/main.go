// Command mirrin-canary watches Mirrin Cloud from the outside, as a paying
// customer would see it, and is the only thing that pages
// (docs/cloud-design.md §15, cloud/ops/ALERTS.md).
//
//   - twin: the canary twin, a linked machine that answers /healthz through
//     every relay under its own Let's Encrypt certificate.
//   - probe: one region's checker: HTTPS to the canary through each relay
//     on its own, every minute.
//   - watch: reads the probes and pages for exactly one condition (every
//     relay failing from at least two regions for three minutes); every
//     other alarm is an email.
//   - mirror: copies the signed deny list to the public bucket relays fall
//     back to.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

const usage = `mirrin-canary: the Mirrin Cloud canary, probes and pager.

Usage:
  mirrin-canary link  --config canary.yaml   Link the canary twin (pay with the operator's discount code)
  mirrin-canary twin  --config canary.yaml   Run the canary twin
  mirrin-canary probe --config canary.yaml   Run this region's probe
  mirrin-canary probe --config canary.yaml --once
                                             One round, printed; exit 1 if any relay failed
  mirrin-canary watch --config canary.yaml   Read the probes; page or email
  mirrin-canary watch --config canary.yaml --test-page
                                             Send a test page and email, then resolve it
  mirrin-canary mirror --config canary.yaml  Copy the deny list to the public bucket
  mirrin-canary check-config --config canary.yaml [--role twin,probe,watch,mirror]
  mirrin-canary version
`

const defaultConfig = "/etc/mirrin-canary/canary.yaml"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "mirrin-canary", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	case "link", "twin", "probe", "watch", "mirror", "check-config":
		err = command(ctx, args[0], args[1:], stdout, stderr)
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		var fail *probeFailed
		if !errors.As(err, &fail) {
			fmt.Fprintln(stderr, "mirrin-canary:", err)
		}
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	return 0
}

// probeFailed is probe --once finding a relay down; the round is already
// printed.
type probeFailed struct{}

func (*probeFailed) Error() string { return "a relay failed" }

func command(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "canary.yaml")
	roles := fs.String("role", "", "check-config: the roles to check, comma-separated (default: every section that is set)")
	once := fs.Bool("once", false, "probe: one round, printed")
	testPage := fs.Bool("test-page", false, "watch: send a test page and email, then resolve it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected %q", fs.Arg(0))
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))

	switch name {
	case "check-config":
		rs := presentRoles(cfg)
		if *roles != "" {
			rs = strings.Split(*roles, ",")
		}
		if len(rs) == 0 {
			return errors.New("the config sets no role's section")
		}
		if err := cfg.check(rs...); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: ok for %s\n", *cfgPath, strings.Join(rs, ", "))
		return nil
	case "link":
		if err := cfg.check(roleTwin); err != nil {
			return err
		}
		return link(ctx, cfg, twinDeps{Log: log}, stdout, nil, 5*time.Second, time.Hour)
	case "twin":
		if err := cfg.check(roleTwin); err != nil {
			return err
		}
		return runTwin(ctx, cfg, twinDeps{Log: log})
	case "probe":
		if err := cfg.check(roleProbe); err != nil {
			return err
		}
		p, err := newProber(cfg, log)
		if err != nil {
			return err
		}
		if *once {
			r := p.round(ctx)
			bad := false
			for _, x := range r.Relays {
				state := "ok"
				if !x.OK {
					state, bad = "FAILED", true
				}
				fmt.Fprintf(stdout, "%s via %s: %s\n", cfg.Probe.Host, x.ID, state)
				for _, a := range x.Addrs {
					fmt.Fprintf(stdout, "  %-45s %5d ms  %s\n", a.Addr, a.MS, a.Error)
				}
			}
			if bad {
				return &probeFailed{}
			}
			return nil
		}
		if cfg.Probe.Listen != "" {
			if err := serveHTTP(ctx, cfg.Probe.Listen, p.handler()); err != nil {
				return err
			}
		}
		p.run(ctx)
		return nil
	case "watch":
		if err := cfg.check(roleWatch); err != nil {
			return err
		}
		n := newNotifier(cfg.Watch)
		if *testPage {
			return sendTestPage(ctx, n, stdout)
		}
		w, err := newWatcher(cfg, n, log)
		if err != nil {
			return err
		}
		if cfg.Watch.Listen != "" {
			if err := serveHTTP(ctx, cfg.Watch.Listen, w.handler()); err != nil {
				return err
			}
		}
		w.run(ctx)
		return nil
	case "mirror":
		if err := cfg.check(roleMirror); err != nil {
			return err
		}
		m, err := newMirror(cfg, log)
		if err != nil {
			return err
		}
		m.run(ctx)
		return nil
	}
	return fmt.Errorf("unknown command %q", name)
}

// presentRoles are the roles whose sections the config sets.
func presentRoles(c *Config) []string {
	var rs []string
	if c.API != "" || c.DataDir != "" {
		rs = append(rs, roleTwin)
	}
	if c.Probe.Region != "" || len(c.Probe.Relays) > 0 {
		rs = append(rs, roleProbe)
	}
	if len(c.Watch.Regions) > 0 {
		rs = append(rs, roleWatch)
	}
	if c.Mirror.Source != "" {
		rs = append(rs, roleMirror)
	}
	return rs
}

// sendTestPage proves the pager and the mail path work, on the day they
// are set up and after every change to them.
func sendTestPage(ctx context.Context, n *notifier, out io.Writer) error {
	on := Event{Key: "mirrin-canary-test", Page: true, Raised: true, Summary: "Test page from mirrin-canary. Nothing is wrong."}
	if err := n.page(ctx, on); err != nil {
		return fmt.Errorf("the pager: %w", err)
	}
	fmt.Fprintln(out, "Paged. Acknowledge it on your phone.")
	if err := n.email(ctx, on); err != nil {
		return fmt.Errorf("email: %w", err)
	}
	fmt.Fprintln(out, "Emailed.")
	off := on
	off.Raised, off.Summary = false, "Test page resolved."
	if err := n.page(ctx, off); err != nil {
		return fmt.Errorf("resolving the test page: %w", err)
	}
	fmt.Fprintln(out, "Resolved.")
	return nil
}

// serveHTTP serves h on addr until ctx ends.
func serveHTTP(ctx context.Context, addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	go func() { <-ctx.Done(); srv.Close() }()
	return nil
}
