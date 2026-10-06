// Command mirrin-cloud is the Mirrin Cloud control plane: it turns a
// payment into a handle, its DNS records and a signed entitlement, and keeps
// nothing more than it must (cloud/README.md). Admin is this CLI, over SSH;
// there is no admin web page.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/billing"
	"github.com/MavrkAI/Mirrin/cloud/internal/dns"
	"github.com/MavrkAI/Mirrin/cloud/internal/keys"
	"github.com/MavrkAI/Mirrin/cloud/internal/server"
	"github.com/MavrkAI/Mirrin/cloud/internal/storage"
	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/sigv4"
)

var version = "dev"

const usage = `mirrin-cloud: the Mirrin Cloud control plane.

Usage:
  mirrin-cloud serve --config cloud.yaml
      Run the control plane (Paddle, Route 53, keys from keys.dir).
  mirrin-cloud serve --dev [--config cloud.yaml] [--listen 127.0.0.1:8787] [--data DIR]
      Run it locally on a fake merchant of record, fake DNS and the public
      development keys, on a loopback address only, with its data in a new
      temporary directory unless --data names one. Link a daemon built with
      -tags mirrin_devkeys.
  mirrin-cloud keys generate --dir DIR     Make the first ent-* and dl-* signing keys
  mirrin-cloud keys rotate --dir DIR --purpose ent|dl
                                           Make the next key of one purpose
  mirrin-cloud keys public --dir DIR       Print the public keys for internal/entitle/keys.go
  mirrin-cloud admin deny handle HANDLE [--why TEXT] --config cloud.yaml
  mirrin-cloud admin deny key DEVICE_KEY [--why TEXT] --config cloud.yaml
  mirrin-cloud admin show HANDLE --config cloud.yaml
  mirrin-cloud version
`

const defaultConfig = "/etc/mirrin-cloud/cloud.yaml"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, nil)
	stop()
	os.Exit(code)
}

// run is the CLI. ready, if not nil, hears the origin once serve listens.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(origin string)) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = serve(ctx, args[1:], stderr, ready)
	case "keys":
		err = keysCmd(args[1:], stdout)
	case "admin":
		err = admin(ctx, args[1:], stdout)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "mirrin-cloud %s (api v1)\n", version)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "mirrin-cloud:", err)
		return 1
	}
	return 0
}

// parse parses fs, allowing flags after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func serve(ctx context.Context, args []string, stderr io.Writer, ready func(string)) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "cloud.yaml")
	dev := fs.Bool("dev", false, "fake merchant of record, fake DNS, development keys")
	listen := fs.String("listen", "", "address to serve on (dev)")
	data := fs.String("data", "", "data directory (dev)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected %q", pos[0])
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	if !*dev {
		if *cfgPath == "" {
			*cfgPath = defaultConfig
		}
		if *listen != "" || *data != "" {
			return errors.New("--listen and --data are for --dev; set listen and data_dir in cloud.yaml")
		}
		return serveProd(ctx, *cfgPath, log)
	}

	cfg := server.DevConfig()
	if *cfgPath != "" {
		if cfg, err = server.LoadConfig(*cfgPath, cfg); err != nil {
			return err
		}
	}
	derived := cfg.PublicURL == "http://"+cfg.Listen
	if *listen != "" {
		cfg.Listen = *listen
	}
	if err := server.CheckDevListen(cfg.Listen); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	if derived {
		cfg.PublicURL = "http://" + ln.Addr().String()
	}
	if *data != "" {
		cfg.DataDir = *data
	}
	if cfg.DataDir == "" {
		// A new private directory each run, never a shared, guessable path
		// someone else could have made first.
		if cfg.DataDir, err = os.MkdirTemp("", "mirrin-cloud-dev-"); err != nil {
			return err
		}
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "cloud.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	fake := billing.NewFake(cfg.PublicURL)
	// Backups go to a folder beside the database, at URLs this server
	// signs and serves itself under /storage/.
	objects, err := storage.NewFake(filepath.Join(cfg.DataDir, "storage"), cfg.PublicURL+"/storage")
	if err != nil {
		return err
	}
	s, err := server.New(cfg, server.Options{
		Store: st, Billing: fake, DevBilling: fake, DNS: dns.NewFake(), Keys: keys.Dev(),
		Storage: objects, DevStorage: objects,
		Dev: true, Log: log, Version: version,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "mirrin-cloud --dev on %s (data in %s; --data DIR keeps it between runs)\n"+
		"  fake checkout, fake DNS, public development keys: never expose this.\n"+
		"  link a daemon built with -tags mirrin_devkeys: mirrin cloud link --api %s\n",
		s.Origin(), cfg.DataDir, s.Origin())
	if ready != nil {
		ready(s.Origin())
	}
	return s.Serve(ctx, ln)
}

func serveProd(ctx context.Context, path string, log *slog.Logger) error {
	cfg, err := server.LoadConfig(path, server.DefaultConfig())
	if err != nil {
		return err
	}
	if cfg.DataDir == "" {
		return errors.New("data_dir is required")
	}
	ks, err := keys.Load(os.ExpandEnv(cfg.Keys.Dir), cfg.Keys.Entitlement, cfg.Keys.DenyList)
	if err != nil {
		return err
	}
	if cfg.Billing.Provider != "paddle" {
		return fmt.Errorf("billing.provider %q: only paddle is supported (--dev uses a fake)", cfg.Billing.Provider)
	}
	pc := cfg.Billing.Paddle
	pay := &billing.Paddle{APIBase: pc.APIBase, APIKey: os.Getenv(pc.APIKeyEnv), WebhookSecret: os.Getenv(pc.WebhookSecretEnv), Prices: pc.Prices}
	if pay.APIBase == "" {
		pay.APIBase = "https://api.paddle.com"
	}
	if pay.APIKey == "" || pay.WebhookSecret == "" || pay.Prices[cfg.Plan] == "" {
		return errors.New("billing.paddle needs api_key_env and webhook_secret_env set in the environment, and a price for the plan")
	}
	if cfg.DNS.Provider != "route53" {
		return fmt.Errorf("dns.provider %q: only route53 is supported (--dev uses a fake)", cfg.DNS.Provider)
	}
	rc := cfg.DNS.Route53
	r53 := &dns.Route53{Zone: cfg.TenantZone, HostedZoneID: rc.HostedZoneID, Endpoint: rc.Endpoint, TTL: rc.TTL,
		Creds: sigv4.Creds{AccessKeyID: os.Getenv(rc.AccessKeyEnv), SecretAccessKey: os.Getenv(rc.SecretKeyEnv), SessionToken: envOr(rc.SessionTokenEnv)}}
	if r53.Creds.AccessKeyID == "" || r53.Creds.SecretAccessKey == "" || r53.HostedZoneID == "" {
		return errors.New("dns.route53 needs hosted_zone_id, and access_key_env and secret_key_env set in the environment")
	}
	objects, err := prodStorage(cfg.Storage)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "cloud.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := server.New(cfg, server.Options{Store: st, Billing: pay, DNS: r53, Keys: ks, Storage: objects, Log: log, Version: version})
	if err != nil {
		return err
	}
	return s.Run(ctx)
}

// prodStorage builds the backup store cloud.yaml names; nil for none.
func prodStorage(c server.Storage) (storage.Provider, error) {
	switch c.Provider {
	case "":
		return nil, nil
	case "r2":
		r := &storage.R2{Endpoint: c.R2.Endpoint, Bucket: c.R2.Bucket, Region: c.R2.Region,
			Creds: sigv4.Creds{AccessKeyID: envOr(c.R2.AccessKeyEnv), SecretAccessKey: envOr(c.R2.SecretKeyEnv)}}
		if r.Endpoint == "" || r.Bucket == "" || r.Creds.AccessKeyID == "" || r.Creds.SecretAccessKey == "" {
			return nil, errors.New("storage.r2 needs endpoint and bucket, and access_key_env and secret_key_env set in the environment")
		}
		return r, nil
	}
	return nil, fmt.Errorf("storage.provider %q: only r2 is supported (--dev uses a fake)", c.Provider)
}

func envOr(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}

func keysCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("keys generate|rotate|public --dir DIR")
	}
	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
	dir := fs.String("dir", "", "the key directory")
	purpose := fs.String("purpose", "", "ent or dl (rotate)")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if *dir == "" || len(pos) > 0 {
		return errors.New("usage: mirrin-cloud keys " + args[0] + " --dir DIR")
	}
	year := time.Now().UTC().Year()
	var purposes []string
	switch args[0] {
	case "generate":
		if ks, _ := keys.Public(*dir); len(ks) > 0 {
			return fmt.Errorf("%s already holds keys; use keys rotate", *dir)
		}
		purposes = []string{keys.Entitlement, keys.DenyList}
	case "rotate":
		if *purpose != keys.Entitlement && *purpose != keys.DenyList {
			return errors.New("--purpose ent or --purpose dl")
		}
		purposes = []string{*purpose}
	case "public":
		ks, err := keys.Public(*dir)
		if err != nil {
			return err
		}
		printKeys(stdout, ks)
		return nil
	default:
		return fmt.Errorf("unknown keys command %q", args[0])
	}
	made := map[string]ed25519.PublicKey{}
	for _, p := range purposes {
		kid, err := keys.NextKid(*dir, p, year)
		if err != nil {
			return err
		}
		pub, err := keys.Generate(*dir, kid)
		if err != nil {
			return err
		}
		made[kid] = pub
		fmt.Fprintf(stdout, "wrote %s\n", filepath.Join(*dir, kid+".pem"))
	}
	printKeys(stdout, made)
	fmt.Fprintln(stdout, "Ship new public keys in internal/entitle/keys.go a release before they sign; then name them under keys: in cloud.yaml.")
	return nil
}

// printKeys prints keys as the lines internal/entitle/keys.go takes.
func printKeys(w io.Writer, ks map[string]ed25519.PublicKey) {
	for _, kid := range keys.Kids(ks) {
		m := "EntitlementKeys"
		if p, _ := keys.Purpose(kid); p == keys.DenyList {
			m = "DenyListKeys"
		}
		fmt.Fprintf(w, "%s[%q] = mustKey(%q)\n", m, kid, entitle.EncodeKey(ks[kid]))
	}
}

func admin(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfig, "cloud.yaml")
	why := fs.String("why", "admin", "the reason recorded on the deny list")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	cfg, err := server.LoadConfig(*cfgPath, server.DefaultConfig())
	if err != nil {
		return err
	}
	if cfg.DataDir == "" {
		return errors.New("data_dir is required")
	}
	if len(*why) > 64 || strings.ContainsFunc(*why, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errors.New("--why is one line of at most 64 bytes")
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "cloud.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now().UTC()
	switch {
	case len(pos) == 3 && pos[0] == "deny" && pos[1] == "handle":
		if err := server.DenyHandle(ctx, st, pos[2], *why, now); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "denied handle %s; relays refuse it within a minute\n", pos[2])
	case len(pos) == 3 && pos[0] == "deny" && pos[1] == "key":
		if err := server.DenyKey(ctx, st, pos[2], *why, now); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "denied key %s; relays refuse it within a minute\n", pos[2])
	case len(pos) == 2 && pos[0] == "show":
		doc, err := server.Show(ctx, st, pos[1], now)
		if err != nil {
			return err
		}
		b, err := json.Marshal(doc, json.Deterministic(true))
		if err != nil {
			return err
		}
		stdout.Write(append(b, '\n'))
	default:
		return errors.New("usage: mirrin-cloud admin deny handle|key VALUE [--why TEXT] | admin show HANDLE")
	}
	return nil
}
