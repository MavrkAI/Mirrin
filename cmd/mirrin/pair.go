package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/devices"
)

// pairCmd is `mirrin pair`: it asks the running twin for a single-use offer
// and prints a code for another computer's terminal (the default) or a link
// and QR code for a phone, tablet or wall screen. Neither carries this
// computer's master key.
func pairCmd(ctx context.Context, cfg *config.Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	screen := fs.Bool("screen", false, "a phone, tablet or browser")
	kiosk := fs.Bool("kiosk", false, "a wall screen (it can only look)")
	kind := fs.String("kind", "", "pwa, kiosk or cli")
	scopes := fs.String("scopes", "", "view,chat,approve,admin")
	host := fs.String("host", "", "the address other devices use, e.g. http://myserver:7742")
	noQR := fs.Bool("no-qr", false, "don't draw a QR code")
	if err := fs.Parse(args); err != nil {
		return errors.New("usage: mirrin pair [--screen | --kiosk] [--scopes view,chat,approve] [--host http://address:port] [--no-qr]")
	}
	k := devices.KindCLI
	switch {
	case *kind != "":
		k = *kind
	case *kiosk:
		k = devices.KindKiosk
	case *screen:
		k = devices.KindPWA
	}
	if !devices.ValidKind(k) {
		return fmt.Errorf("%q isn't a kind of device; use --screen (a phone or tablet), --kiosk (a wall screen), or nothing for another computer", k)
	}
	var sc []devices.Scope
	if *scopes != "" {
		var err error
		if sc, err = devices.ParseScopes(*scopes); err != nil {
			return err
		}
	}
	if cfg.API.Listen == "" {
		return errors.New("pairing needs " + cfg.Name + "'s local connection, which is switched off (api.listen is empty in the settings file); set api.listen to 127.0.0.1:7742 and restart " + cfg.Name)
	}
	c := api.Connect(cfg.API.Listen, cfg.DataDir)
	if c == nil {
		return fmt.Errorf("%s isn't running, so there's nothing to pair with yet; start it (the menu bar app, or `mirrin run`), then run `mirrin pair` again", cfg.Name)
	}
	resp, err := c.NewOffer(ctx, k, sc)
	if err != nil {
		return plainErr(err)
	}
	if resp.Name == "" {
		resp.Name = cfg.Name
	}
	if *host != "" {
		resp.Compose([]api.Base{{URL: api.BaseURL(strings.TrimSpace(*host))}})
	}
	return printOffer(out, resp, !*noQR)
}

// printOffer shows an offer the way the owner can use it.
func printOffer(out io.Writer, resp api.OfferResponse, qr bool) error {
	name, o := resp.Name, resp.Offer
	if len(resp.Bases) == 0 {
		fmt.Fprintf(out, "Other devices can't reach %s yet.\n", name)
		fmt.Fprintln(out, "Set api.remote: true, and api.listen to this computer's Tailscale address (for example 100.x.y.z:7742), in the config, restart, then run `mirrin pair` again.")
		fmt.Fprintln(out, "(If other devices reach it at an address of your own, pass it: mirrin pair --host http://address:7742)")
		return nil
	}
	plain := false
	for _, b := range resp.Bases {
		plain = plain || strings.HasPrefix(b.URL, "http://")
	}
	if o.Kind == devices.KindCLI {
		fmt.Fprintf(out, "On the other computer, run:\n\n  mirrin connect %s\n\n", resp.Code)
		fmt.Fprintf(out, "The code works once, until %s. It holds no key of this computer's: the other computer gets a key of its own, to %s, which you can cut off any time with `mirrin devices revoke`.\n", api.Expiry(o), api.DescribeScopes(o.Scopes))
	} else {
		what := "phone or tablet"
		if o.Kind == devices.KindKiosk {
			what = "wall screen"
		}
		fmt.Fprintf(out, "Open this link on the %s, or scan the code with its camera:\n\n  %s\n\n", what, resp.Links[0])
		if qr {
			qrterminal.GenerateHalfBlock(resp.Links[0], qrterminal.L, out)
			fmt.Fprintln(out)
		}
		if len(resp.Links) > 1 {
			fmt.Fprintln(out, "If that address doesn't open there, try:")
			for _, l := range resp.Links[1:] {
				fmt.Fprintf(out, "  %s\n", l)
			}
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "The link works once, until %s. The %s will be able to %s.\n", api.Expiry(o), what, api.DescribeScopes(o.Scopes))
	}
	if plain {
		fmt.Fprintln(out, "It travels over plain HTTP, so use it over Tailscale or a network you trust.")
	}
	if o.Kind == devices.KindCLI {
		fmt.Fprintln(out, "For a phone, tablet or wall screen, run `mirrin pair --screen` instead.")
	}
	return nil
}

// connectCmd is `mirrin connect <code>`: pair this computer's terminal with a
// twin elsewhere, so `mirrin chat` and `mirrin voice` talk to it.
func connectCmd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "what to call this computer there")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return errors.New("usage: mirrin connect <pairing code>   (run `mirrin pair` on the computer your twin runs on to get one)")
	}
	if *name == "" {
		*name = thisComputer()
	}
	code := strings.TrimSpace(fs.Arg(0))
	if strings.HasPrefix(code, api.CodePrefix) {
		return connectV2(ctx, code, *name, out)
	}
	if addr, tok, twin, ok := api.DecodeLegacyCode(code); ok {
		return connectLegacy(ctx, addr, tok, twin, *name, out)
	}
	return errors.New("that doesn't look like a pairing code; on the computer your twin runs on, run `mirrin pair` and copy the whole `mirrin connect …` line")
}

func connectV2(ctx context.Context, code, name string, out io.Writer) error {
	c, err := api.DecodeCode(code)
	if err != nil {
		return fmt.Errorf("that pairing code is cut short or mistyped; copy the whole `mirrin connect …` line again")
	}
	twin := c.Name
	if twin == "" {
		twin = "your twin"
	}
	for _, u := range c.URLs {
		resp, err := api.Claim(ctx, u, c.Pins, c.Offer, c.Secret, name, devices.KindCLI)
		if err == nil {
			r := remoteFile{Address: u, Token: resp.Token, Name: c.Name, Pins: c.Pins, Device: resp.Device.ID}
			if err := saveRemote(r); err != nil {
				return err
			}
			fmt.Fprintf(out, "Paired with %s at %s, as %q. `mirrin chat` and `mirrin voice` now talk to it.\n", twin, u, resp.Device.Name)
			return nil
		}
		var ae *api.Error
		if errors.As(err, &ae) {
			return plainErr(err) // it answered: another address would say the same
		}
	}
	return fmt.Errorf("couldn't reach %s at %s; check this computer is on the same Tailscale or network, and that the code is less than 10 minutes old", twin, strings.Join(c.URLs, " or "))
}

// connectLegacy takes an old-style code (address|master key|name). It still
// works over plain-HTTP api.remote; the key is swapped for one of this
// computer's own at once, if the twin is new enough.
func connectLegacy(ctx context.Context, addr, tok, twin, name string, out io.Writer) error {
	if twin == "" {
		twin = "your twin"
	}
	fmt.Fprintln(out, "Warning: this is an old-style pairing code. It carries your twin's master key and works only over plain HTTP.")
	c := api.ConnectWithToken(addr, tok)
	if c == nil {
		return fmt.Errorf("couldn't reach %s at %s; is api.remote on there, and are both computers on the same Tailscale or network?", twin, addr)
	}
	r := remoteFile{Address: addr, Token: tok, Name: twin}
	resp, err := c.UpgradeLegacy(ctx, name)
	var ae *api.Error
	switch {
	case err == nil:
		r.Token, r.Device = resp.Token, resp.Device.ID
		fmt.Fprintln(out, "This computer now has a key of its own, so the old code isn't needed any more.")
	case errors.As(err, &ae) && ae.Code == "not_needed":
	default:
		fmt.Fprintln(out, "That twin is an older version, so this computer keeps its master key for now. Update it, then run `mirrin pair` there for a new code.")
	}
	if err := saveRemote(r); err != nil {
		return err
	}
	fmt.Fprintf(out, "Paired with %s at %s. `mirrin chat` and `mirrin voice` now talk to it.\n", twin, addr)
	return nil
}

// pairedClient connects to the twin this computer is paired with, if any. A
// terminal still on an old-style code's master key moves onto a key of its
// own on the way.
func pairedClient() *api.Client {
	r, ok := loadRemote()
	if !ok {
		return nil
	}
	twin := r.Name
	if twin == "" {
		twin = "your twin"
	}
	c, err := api.DialErr(api.Target{Address: r.Address, Token: r.Token, Pins: r.Pins})
	if c == nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Status == 401 {
			fmt.Fprintf(os.Stderr, "warning: %s at %s doesn't know this computer any more (it was disconnected there). Pair again with a code from `mirrin pair`, or run `mirrin disconnect`. Using this computer's twin for now.\n", twin, r.Address)
		} else {
			fmt.Fprintf(os.Stderr, "warning: paired %s at %s is not reachable; using local\n", twin, r.Address)
		}
		return nil
	}
	if !devices.LooksLikeToken(r.Token) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if resp, err := c.UpgradeLegacy(ctx, thisComputer()); err == nil {
			r.Token, r.Device = resp.Token, resp.Device.ID
			if err := saveRemote(r); err != nil {
				fmt.Fprintln(os.Stderr, "warning: couldn't save this computer's new key:", err)
			}
		}
		cancel()
	}
	fmt.Printf("(connected to %s at %s)\n", twin, strings.TrimPrefix(r.Address, "http://"))
	return c
}

// devicesCmd is `mirrin devices [list | revoke <id> | rename <id> <name> | add | page | review]`.
func devicesCmd(ctx context.Context, cfg *config.Config, args []string, out io.Writer) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	if _, ok := devicePages[sub]; ok {
		return devicesPage(cfg, sub, out) // devices.go
	}
	var c *api.Client
	if cfg.API.Listen != "" {
		c = api.Connect(cfg.API.Listen, cfg.DataDir)
	}
	var store *devices.Store
	if c == nil {
		if daemon.HomeInUse(cfg.DataDir) {
			return fmt.Errorf("%s is running but isn't answering on its local connection, so I can't change its devices from here; send /revoke in your chat with it instead", cfg.Name)
		}
		var err error
		if store, err = devices.Open(devices.Path(cfg.DataDir)); err != nil {
			return err
		}
	}
	list := func() ([]devices.Device, error) {
		if c != nil {
			return c.Devices(ctx)
		}
		return store.List(), nil
	}
	find := func(prefix string) (devices.Device, error) {
		all, err := list()
		if err != nil {
			return devices.Device{}, plainErr(err)
		}
		prefix = strings.ToLower(prefix)
		var hit *devices.Device
		for i, d := range all {
			if d.Revoked() || d.Local() || len(prefix) < 4 || !strings.HasPrefix(d.ID, prefix) {
				continue
			}
			if hit != nil {
				return devices.Device{}, fmt.Errorf("more than one device starts with %q; give more of its id", prefix)
			}
			hit = &all[i]
		}
		if hit == nil {
			return devices.Device{}, fmt.Errorf("no paired device starts with %q; `mirrin devices` lists them", prefix)
		}
		return *hit, nil
	}
	switch sub {
	case "list", "ls":
		all, err := list()
		if err != nil {
			return plainErr(err)
		}
		printDevices(out, all)
		return nil
	case "revoke":
		if len(args) < 2 {
			return errors.New("usage: mirrin devices revoke <id>   (`mirrin devices` lists them)")
		}
		d, err := find(args[1])
		if err != nil {
			return err
		}
		var res api.RevokeResult
		if c != nil {
			res, err = c.RevokeDevice(ctx, d.ID)
		} else {
			res, err = revokeStopped(store, cfg.DataDir, d.ID)
		}
		if err != nil {
			return plainErr(err)
		}
		fmt.Fprintf(out, "%q can't reach %s any more.\n", d.Name, cfg.Name)
		switch {
		case res.KeyChanged:
			fmt.Fprintln(out, "It came in with the old shared key, so that key was changed too: anything else still paired the old way needs pairing again with `mirrin pair`. This computer's menu and terminal keep working.")
		case res.KeyError != "":
			fmt.Fprintf(out, "It came in with the old shared key, which couldn't be changed (%s), so whoever holds that key can still reach %s. To change it, delete %s and restart %s.\n", res.KeyError, cfg.Name, api.TokenPath(cfg.DataDir), cfg.Name)
		}
		return nil
	case "rename":
		if len(args) < 3 {
			return errors.New("usage: mirrin devices rename <id> <new name>")
		}
		d, err := find(args[1])
		if err != nil {
			return err
		}
		newName := strings.Join(args[2:], " ")
		if c != nil {
			d, err = c.RenameDevice(ctx, d.ID, newName)
		} else {
			d, err = store.Rename(d.ID, newName)
		}
		if err != nil {
			return plainErr(err)
		}
		fmt.Fprintf(out, "Renamed to %q.\n", d.Name)
		return nil
	}
	return errors.New("usage: mirrin devices [list | revoke <id> | rename <id> <name> | add | page | review]")
}

// revokeStopped revokes a device while the twin isn't running, replacing the
// master key file too when the device came in with that key (the twin reads
// the new one when it starts).
func revokeStopped(store *devices.Store, dataDir, id string) (api.RevokeResult, error) {
	before, _ := store.Get(id)
	d, err := store.Revoke(id)
	if err != nil {
		return api.RevokeResult{}, err
	}
	res := api.RevokeResult{Device: d}
	if d.SharedKey && !before.Revoked() {
		if _, err := api.ResetToken(dataDir); err != nil {
			res.KeyError = err.Error()
		} else {
			res.KeyChanged = true
		}
	}
	// As the running twin does on a revoke (BackupSoon): the next encrypted
	// snapshot, taken as soon as the twin runs, no longer has the device, so
	// a restore can't bring it back.
	if s, err := backup.LoadSettings(config.Path()); err == nil && s.Recipient != "" {
		_ = backup.WantSoon(dataDir, time.Now())
	}
	return res, nil
}

func printDevices(out io.Writer, all []devices.Device) {
	var active, revoked []devices.Device
	local := 0
	for _, d := range all {
		switch {
		case d.Local() && !d.Revoked():
			local++
		case d.Local():
		case d.Revoked():
			revoked = append(revoked, d)
		default:
			active = append(active, d)
		}
	}
	if len(active) == 0 {
		fmt.Fprintln(out, "No devices are paired. Pair one with `mirrin pair` (another computer) or `mirrin pair --screen` (a phone or tablet).")
	} else {
		fmt.Fprintln(out, "Paired devices:")
		for _, d := range active {
			seen := "never used"
			if !d.LastSeen.IsZero() {
				seen = "last used " + d.LastSeen.Local().Format("02 Jan 15:04")
				if d.LastIP != "" {
					seen += " from " + d.LastIP
				}
			}
			fmt.Fprintf(out, "  %s  %-24s %-16s can %s; %s\n", shortID(d.ID), d.Name, kindName(d.Kind), api.DescribeScopes(d.Scopes), seen)
		}
		fmt.Fprintln(out, "Cut one off with `mirrin devices revoke <id>`.")
	}
	if local > 0 {
		fmt.Fprintf(out, "Plus %d browser(s) on this computer, opened from the menu.\n", local)
	}
	if n := len(revoked); n > 0 {
		fmt.Fprintf(out, "%d revoked device(s) are kept as tombstones, so a restored backup can't bring them back.\n", n)
	}
}

// shortID is enough of a device id to type (a hand-edited file may hold a
// shorter one).
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// kindName says what kind of device it is, in words.
func kindName(kind string) string {
	switch kind {
	case devices.KindKiosk:
		return "wall screen"
	case devices.KindCLI:
		return "terminal"
	case devices.KindLocal:
		return "this computer"
	}
	return "phone or tablet"
}

// plainErr drops the "daemon: " prefix from the twin's own words.
func plainErr(err error) error {
	var ae *api.Error
	if errors.As(err, &ae) {
		return errors.New(strings.TrimSpace(ae.Message + " " + ae.Fix))
	}
	return err
}

// thisComputer is what this computer is called on the twin it pairs with.
func thisComputer() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "A computer's terminal"
	}
	return strings.TrimSuffix(h, ".local") + " (terminal)"
}

// remoteFile is ~/.mirrin/remote.yaml: config.Remote's address, key and
// name, plus what pairing v2 adds (certificate pins, the device's id).
type remoteFile struct {
	Address string   `yaml:"address"`
	Token   string   `yaml:"token"`
	Name    string   `yaml:"name"`
	Pins    []string `yaml:"pins,omitempty"`
	Device  string   `yaml:"device,omitempty"`
}

func loadRemote() (remoteFile, bool) {
	b, err := os.ReadFile(config.RemotePath())
	if err != nil {
		return remoteFile{}, false
	}
	var r remoteFile
	if yaml.Unmarshal(b, &r) != nil || r.Address == "" || r.Token == "" {
		return remoteFile{}, false
	}
	return r, true
}

// saveRemote writes remote.yaml (0600, never half-written).
func saveRemote(r remoteFile) error {
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(config.RemotePath()), ".remote-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), config.RemotePath())
}
