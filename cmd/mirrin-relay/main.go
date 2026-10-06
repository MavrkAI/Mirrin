// Command mirrin-relay is the relay that lets a phone on any network reach
// a Mirrin twin while TLS still ends on the twin's own machine. It routes
// each connection by the SNI in its ClientHello into the tunnel of the
// daemon holding that name, and terminates TLS only for its own control
// name. MavrkAI runs it for Mirrin Cloud; anyone can run it for
// themselves (docs/relay-selfhost.md).
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/MavrkAI/Mirrin/internal/brand"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/relay/server"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

var version = "dev"

const usage = `mirrin-relay: SNI passthrough for Mirrin twins.

Usage:
  mirrin-relay serve --config relay.yaml         Run the relay
  mirrin-relay check-config --config relay.yaml  Check a config and say what it would do
  mirrin-relay keygen --kid K --out key.pem      Make an issuer key (ent-* or dl-*) for a private control plane
  mirrin-relay version

Docs: https://github.com/MavrkAI/Mirrin/blob/main/docs/relay-selfhost.md
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = serve(args[1:], stderr)
	case "check-config":
		err = checkConfig(args[1:], stdout)
	case "keygen":
		err = keygen(args[1:], stdout)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "mirrin-relay %s (%s)\n", version, wire.Subprotocol)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "mirrin-relay:", err)
		return 1
	}
	return 0
}

// The relay's settings, and where a relay set up before the rename keeps them.
const (
	defaultConfig = "/etc/mirrin-relay/relay.yaml"
	legacyConfig  = "/etc/antbot-relay/relay.yaml" // rename:keep
)

func configFlag(name string, args []string) (string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", "", "relay.yaml (default "+defaultConfig+")")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected %q", fs.Arg(0))
	}
	if *path == "" {
		return defaultConfigPath(fileExists), nil
	}
	return *path, nil
}

// defaultConfigPath is the relay's settings when --config doesn't name
// them: Mirrin's place, or AntBot's when only that one is there.
func defaultConfigPath(exists func(string) bool) string {
	if !exists(defaultConfig) && exists(legacyConfig) {
		return legacyConfig
	}
	return defaultConfig
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func serve(args []string, stderr io.Writer) error {
	path, err := configFlag("serve", args)
	if err != nil {
		return err
	}
	cfg, err := server.LoadConfig(path)
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if brand.Env("RELAY_DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
	opts := server.Options{Log: log, Version: version}
	if cfg.Abuse.NotifyCommand != "" {
		opts.OnSuspend = suspendNotifier(cfg.Abuse.NotifyCommand, log)
	}
	s, err := server.New(*cfg, opts)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.Run(ctx)
}

func checkConfig(args []string, stdout io.Writer) error {
	path, err := configFlag("check-config", args)
	if err != nil {
		return err
	}
	cfg, err := server.LoadConfig(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: ok\n", path)
	fmt.Fprintf(stdout, "  relay %s, control name %s, TLS on %s\n", cfg.ID, cfg.ControlHostname, cfg.Listen)
	if cfg.CertFile != "" {
		fmt.Fprintf(stdout, "  control certificate from %s\n", cfg.CertFile)
	} else {
		fmt.Fprintf(stdout, "  control certificate from ACME (%s), cached in %s\n", cfg.ACMEDirectory, cfg.StateDir)
	}
	if cfg.Abuse.NotifyCommand != "" {
		fmt.Fprintf(stdout, "  suspensions run %s\n", cfg.Abuse.NotifyCommand)
	}
	if cfg.SelfHosted() {
		names := map[string]bool{}
		for _, a := range cfg.Allow {
			names[a.Hostname] = true
		}
		fmt.Fprintf(stdout, "  self-host: %d hostnames for %d allow entries; contacts no MavrkAI host\n", len(names), len(cfg.Allow))
	} else {
		fmt.Fprintf(stdout, "  hosted: zones %s; deny list from %s", strings.Join(cfg.Zones, ", "), cfg.DenylistURL)
		if cfg.MirrorURL != "" {
			fmt.Fprintf(stdout, " (mirror %s)", cfg.MirrorURL)
		}
		fmt.Fprintln(stdout)
	}
	l := cfg.Limits
	fmt.Fprintf(stdout, "  limits: %d streams and %d bit/s per tunnel, %d hellos per address per minute, idle %s\n",
		l.StreamsPerTunnel, l.BPS, l.HelloPerIPPerMin, l.Idle)
	return nil
}

// keygen writes a new Ed25519 issuer key, for a private control plane or
// staging, as PKCS#8 PEM (0600), and prints its issuer_keys entry. It
// makes no device keys: a twin's daemon makes its own, and `mirrin reach
// use relay` prints its allow entry.
func keygen(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	out := fs.String("out", "", "where to write the private key (created, never overwritten)")
	kid := fs.String("kid", "", "the key id: ent-* signs entitlements, dl-* deny lists")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || *kid == "" || fs.NArg() > 0 {
		return errors.New("usage: mirrin-relay keygen --kid ent-…|dl-… --out key.pem\n" +
			"  (issuer keys for a private control plane; a twin's allow entry comes from `mirrin reach use relay` on its machine)")
	}
	if !validKid(*kid) {
		return fmt.Errorf("kid %q: ent- or dl-, then 1-32 of a-z 0-9 -", *kid)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := errors.Join(pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}), f.Close()); err != nil {
		return err
	}
	k := entitle.EncodeKey(pub)
	fmt.Fprintf(stdout, "wrote %s\npublic key: %s\nissuer_keys:\n  - {kid: %s, key: %s}\n", *out, k, *kid, k)
	return nil
}

// validKid is relay.yaml's rule for issuer key ids.
func validKid(kid string) bool {
	rest, ok := strings.CutPrefix(kid, "ent-")
	if !ok {
		rest, ok = strings.CutPrefix(kid, "dl-")
	}
	if !ok || rest == "" || len(rest) > 32 {
		return false
	}
	return strings.Trim(rest, "abcdefghijklmnopqrstuvwxyz0123456789-") == ""
}
