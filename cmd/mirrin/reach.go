package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/reach"
	"github.com/MavrkAI/Mirrin/internal/tailscale"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func configureReach(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) >= 1 {
		switch args[0] { // reach_relay: verify, fingerprint, alarm
		case "verify":
			return reachVerify(cfg, args[1:], os.Stdout)
		case "fingerprint":
			return reachFingerprint(cfg, os.Stdout)
		case "alarm":
			return reachAlarm(cfg, args[1:], os.Stdout)
		}
	}
	if len(args) >= 2 && args[0] == "use" && args[1] == "relay" {
		return useRelay(cfg, args[2:], os.Stdout)
	}
	if len(args) >= 2 && args[0] == "use" && args[1] == "cloud" {
		return useCloud(context.Background(), cfg, args[2:], os.Stdout) // reach_cloud.go
	}
	if len(args) == 2 && args[0] == "stay-awake" {
		if args[1] != "on" && args[1] != "off" {
			return fmt.Errorf("use `mirrin reach stay-awake on|off`")
		}
		cfg.Reach.StayAwake = args[1] == "on"
		if err = cfg.Save(); err != nil {
			return err
		}
		fmt.Println("Power setting saved. Restart Mirrin to use it.")
		return nil
	}
	if len(args) == 1 && args[0] == "status" {
		mode := cfg.Reach.Mode
		if mode == "" {
			mode = "off"
		}
		fmt.Printf("Remote HTTPS: %s\nStay awake while on power: %t\n", mode, cfg.Reach.StayAwake)
		if mode == "tailscale" {
			s, e := (&tailscale.CLI{}).Status(context.Background())
			if e != nil {
				return e
			}
			fmt.Println("Hostname:", s.Self.DNSName)
		}
		if mode == "relay" {
			fmt.Printf("Hostname: %s\nRelay: %s\nCheck it from outside with `mirrin reach verify`.\n", cfg.Reach.Hostname, cfg.Reach.RelayURL)
		}
		if mode == "cloud" {
			cloudReachStatus(cfg, os.Stdout)
		}
		if cfg.API.Remote {
			fmt.Println("Plain HTTP remote access is enabled. Run `mirrin reach use tailscale` for HTTPS.")
		}
		return nil
	}
	if len(args) < 2 || args[0] != "use" {
		return fmt.Errorf("Use `mirrin reach status`, `mirrin reach verify`, `mirrin reach fingerprint` or `mirrin reach use tailscale|files|relay|cloud|off` (files needs a certificate and key path; relay needs its address and --hostname)")
	}
	switch args[1] {
	case "off", "tailscale":
		if len(args) != 2 {
			return fmt.Errorf("Use `mirrin reach use %s`", args[1])
		}
	case "files":
		if len(args) != 4 {
			return fmt.Errorf("Use `mirrin reach use files <certificate.pem> <key.pem>`")
		}
		source := tlsmgr.Files(args[2], args[3])
		if _, err = source.GetCertificate(nil); err != nil {
			return err
		}
		if len(source.Hostnames()) == 0 {
			return fmt.Errorf("the certificate needs a DNS name; choose a certificate for this computer’s HTTPS hostname")
		}
		cfg.Reach.CertFile, err = filepath.Abs(args[2])
		if err != nil {
			return err
		}
		cfg.Reach.KeyFile, err = filepath.Abs(args[3])
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("That mode is planned. Choose tailscale, files, relay, cloud or off")
	}
	warnOriginChange(cfg.Reach, args[1], "", os.Stdout)
	cfg.Reach.Mode = args[1]
	if cfg.Reach.StepUp == "" {
		cfg.Reach.StepUp = "dangerous"
	}
	if err = cfg.Save(); err != nil {
		return err
	}
	fmt.Println("Remote access saved. Restart Mirrin to use it.")
	if args[1] == "files" {
		fmt.Printf("For key rotation, issue the next certificate using %s.next, then install it with that key.\n", cfg.Reach.KeyFile)
	}
	return nil
}

// warnOriginChange says what a new address costs the phones already paired
// (docs/cloud-design.md §6.1, origin continuity).
func warnOriginChange(old config.Reach, mode, host string, w io.Writer) {
	if old.Mode == "" || old.Mode == "off" || mode == "off" || old.Mode == mode && (mode != "relay" || old.Hostname == host) {
		return
	}
	fmt.Fprintln(w, "Heads up: this changes the address your phones use. Each phone will need the app added again, pairing again, notifications and Face ID again.")
}

// acmeHTTPClient talks to the certificate authority; tests replace it.
var acmeHTTPClient *http.Client

// useRelay is `mirrin reach use relay <wss-url> --hostname <name>`. It
// makes this machine's relay key, prints the relay.yaml allow line and the
// DNS records for the name, and saves the mode.
func useRelay(cfg *config.Config, args []string, w io.Writer) error {
	const usage = "Use `mirrin reach use relay wss://relay.example.com/v1/tunnel --hostname twin.example.com` (optional: --acme <directory-url>, --relay-ca <file.pem>)"
	next := cfg.Reach
	next.Mode, next.RelayURL = "relay", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, bool) {
			if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") {
				return v, true
			}
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		flag, _, _ := strings.Cut(a, "=")
		var ok bool
		switch flag {
		case "--hostname":
			next.Hostname, ok = value()
		case "--acme":
			next.ACMEDirectory, ok = value()
		case "--relay-ca":
			var p string
			if p, ok = value(); ok {
				next.RelayCAFile, _ = filepath.Abs(p)
			}
		default:
			if strings.HasPrefix(a, "-") || next.RelayURL != "" {
				return fmt.Errorf("%s", usage)
			}
			next.RelayURL, ok = a, true
		}
		if !ok {
			return fmt.Errorf("%s", usage)
		}
	}
	host, err := reach.CheckRelayConfig(next)
	if err != nil {
		return fmt.Errorf("%v. %s", err, usage)
	}
	next.Hostname = host
	key, _, err := reach.RelayKeys(cfg.DataDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account, regErr, err := relayAccount(ctx, cfg.DataDir, next, host)
	if err != nil {
		return err
	}
	u, _ := url.Parse(next.RelayURL)
	ips, dnsErr := lookupRelay(ctx, u.Hostname())

	warnOriginChange(cfg.Reach, "relay", host, w)
	fmt.Fprintf(w, "1. Add this machine to your relay's relay.yaml, under allow:\n\n%s\n\n", reach.AllowLine(host, key))
	fmt.Fprintf(w, "2. Publish these DNS records for %s (never a CNAME):\n\n", host)
	for _, r := range reach.BYODRecords(host, account, ips) {
		fmt.Fprintf(w, "   %s\n", r)
	}
	fmt.Fprintln(w)
	if dnsErr != nil || len(ips) == 0 {
		fmt.Fprintf(w, "   (Couldn't look up %s, so add A/AAAA records for the relay's addresses yourself.)\n\n", u.Hostname())
	}
	if regErr != nil {
		fmt.Fprintf(w, "   (Couldn't reach the certificate authority yet, so the CAA record naming this machine's account is missing. Run `mirrin reach fingerprint` later to see it.)\n\n")
	}
	fmt.Fprintf(w, "The CAA records mean only this machine can get a certificate for %s. The relay only ever sees encrypted traffic.\n", host)
	cfg.Reach = next
	if cfg.Reach.StepUp == "" {
		cfg.Reach.StepUp = "dangerous"
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintln(w, "Remote access saved. Restart Mirrin to use it, then run `mirrin reach verify`.")
	return nil
}

// relayAccount is the ACME account the CAA record pins. When data/tls
// already names one at this CA it is read without opening the files for
// writing, since a running daemon may own them; otherwise the account is
// opened and registered; regErr says the CA couldn't be reached yet.
func relayAccount(ctx context.Context, dataDir string, rc config.Reach, host string) (account string, regErr, err error) {
	dir := filepath.Join(dataDir, "tls")
	want := rc.ACMEDirectory
	if want == "" {
		want = tlsmgr.LetsEncrypt
	}
	if st, err := tlsmgr.ReadACMEState(dir); err == nil && st.AccountURI != "" && st.Directory == want {
		return st.AccountURI, nil, nil
	}
	acme, err := tlsmgr.NewACME(tlsmgr.ACMEConfig{Directory: rc.ACMEDirectory, Hostnames: []string{host}, Dir: dir, Email: rc.ACMEEmail, HTTPClient: acmeHTTPClient})
	if err != nil {
		return "", nil, err
	}
	account, regErr = acme.Register(ctx)
	return account, regErr, nil
}

func lookupRelay(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func reachVerify(cfg *config.Config, args []string, w io.Writer) error {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return fmt.Errorf("Use `mirrin reach verify [--json]`")
		}
		asJSON = true
	}
	var e *reach.Endpoint
	var err error
	switch cfg.Reach.Mode {
	case "relay":
		e, err = reach.LocalEndpoint(cfg.Reach, cfg.DataDir)
	case "cloud":
		e, err = cloudVerifyEndpoint(cfg) // reach_cloud.go
	default:
		return fmt.Errorf("`mirrin reach verify` checks relay and cloud reach; this machine uses %q. Run `mirrin reach status`", cfg.Reach.Mode)
	}
	if err != nil {
		return err
	}
	return reach.VerifyCommand(context.Background(), e, w, asJSON)
}

func reachFingerprint(cfg *config.Config, w io.Writer) error {
	st, err := tlsmgr.ReadACMEState(filepath.Join(cfg.DataDir, "tls"))
	if err != nil {
		return fmt.Errorf("this machine has no HTTPS keys yet. Run `mirrin reach use relay …` first")
	}
	host := cfg.Reach.Hostname
	fmt.Fprintf(w, "Hostname:    %s\nCurrent key: %s\nNext key:    %s\n", host, st.Current, st.Next)
	if st.AccountURI != "" {
		fmt.Fprintf(w, "ACME account: %s\n", st.AccountURI)
	}
	if st.Leaf != nil {
		fmt.Fprintf(w, "Certificate: valid until %s, issued by %s\n", st.Leaf.NotAfter.Format("2 Jan 2006"), st.Leaf.Issuer.CommonName)
	}
	if host != "" && st.AccountURI != "" {
		fmt.Fprintln(w, "CAA records to publish:")
		for _, r := range reach.BYODRecords(host, st.AccountURI, nil) {
			fmt.Fprintf(w, "   %s\n", r)
		}
	}
	return nil
}

// reachAlarm is `mirrin reach alarm [clear]`. Clearing goes through the
// running twin when there is one, so it takes effect at once.
func reachAlarm(cfg *config.Config, args []string, w io.Writer) error {
	if len(args) > 1 || len(args) == 1 && args[0] != "clear" {
		return fmt.Errorf("Use `mirrin reach alarm` or `mirrin reach alarm clear`")
	}
	a, err := reach.OpenAlarm(cfg.DataDir)
	if err != nil {
		return err
	}
	st := a.State()
	if len(args) == 0 {
		if !st.Active {
			fmt.Fprintln(w, "No certificate alarm.")
			return nil
		}
		fmt.Fprintf(w, "Certificate alarm since %s: %s\nApprovals from other devices are paused. Clear it with `mirrin reach alarm clear` once you've checked.\n", st.Since.Local().Format("2 Jan 15:04"), st.Detail)
		return nil
	}
	if !st.Active {
		fmt.Fprintln(w, "There's no alarm to clear.")
		return nil
	}
	if c := api.Connect(cfg.API.Listen, cfg.DataDir); c != nil {
		req, _ := http.NewRequest(http.MethodPost, "http://"+c.Address()+"/reach/alarm/clear", nil)
		req.Header.Set("Authorization", "Bearer "+c.Token())
		res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return fmt.Errorf("couldn't reach the running Mirrin: %w", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("the running Mirrin didn't clear it (%s)", res.Status)
		}
		var after reach.AlarmState
		if json.NewDecoder(res.Body).Decode(&after) != nil || after.Active {
			return fmt.Errorf("the running Mirrin didn't clear it; try again")
		}
	} else if err := a.Clear("this computer"); err != nil {
		return err
	}
	fmt.Fprintln(w, "Alarm cleared. Approvals from other devices work again. Devices that were signed out need pairing again with `mirrin pair`.")
	return nil
}
