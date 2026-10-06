package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/reach"
)

// useCloud is `mirrin reach use cloud [--handle NAME]`: reach through Mirrin
// Cloud, linking this machine first when it isn't (the checkout opens in
// the browser). The free mode in use now stays as the fallback, for when
// the paid address can't serve.
func useCloud(ctx context.Context, cfg *config.Config, args []string, w io.Writer) error {
	const usage = "Use `mirrin reach use cloud` (optional: --handle NAME, the address to ask for when linking)"
	handle, api := "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, v, hasV := strings.Cut(a, "=")
		if !hasV {
			if i+1 >= len(args) {
				return fmt.Errorf("%s", usage)
			}
			i++
			v = args[i]
		}
		switch flag {
		case "--handle":
			handle = strings.ToLower(strings.TrimSpace(v))
		case "--api":
			api = v
		default:
			return fmt.Errorf("%s", usage)
		}
	}
	if handle != "" {
		if err := cloud.CheckHandle(handle); err != nil {
			return fmt.Errorf("the address name must be 3 to 32 letters, digits and single hyphens. %s", usage)
		}
	}
	if bs, err := backup.LoadState(cfg.DataDir); err == nil && bs.Standby != nil {
		// Standing by after a handover: nothing is sent, not even a checkout.
		return errors.New("this copy is standing by because the twin moved to another computer; run `mirrin backup resume` to bring it back here first")
	}
	st, err := cloud.OpenStateWithKeys(cfg.DataDir, cloudKeys())
	if err != nil {
		return err
	}
	if !st.Linked() {
		acct := reach.LinkedAccount(cfg.DataDir, cfg.Reach)
		if err := cloudLink(ctx, cfg, api, handle, acct, w); err != nil {
			return err
		}
	} else if handle != "" {
		if info, _, _ := st.Info(); info.Handle != handle {
			fmt.Fprintf(w, "This machine is linked as %s already; --handle only applies when linking.\n", info.Handle)
		}
	}
	switch _, kind := st.Current(time.Now()); kind {
	case cloud.Superseded:
		return errors.New("another machine took over this machine's address, so this one is standing by (mirrin cloud status)")
	case cloud.Expired:
		fmt.Fprintln(w, "The address has expired; Mirrin uses your fallback until it is renewed (mirrin cloud status).")
	}
	warnOriginChange(cfg.Reach, "cloud", "", w)
	if cfg.Reach.Mode == "tailscale" || cfg.Reach.Mode == "files" {
		cfg.Reach.Fallback = cfg.Reach.Mode
	}
	cfg.Reach.Mode = "cloud"
	if cfg.Reach.StepUp == "" {
		cfg.Reach.StepUp = "dangerous"
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintln(w, "Remote access saved. Restart Mirrin to use it, then run `mirrin reach verify`.")
	if fb := cfg.Reach.Fallback; fb != "" {
		fmt.Fprintf(w, "If the paid address stops (it expires or you unlink), Mirrin goes back to %s.\n", fb)
	}
	return nil
}

// cloudReachStatus is `mirrin reach status` in cloud mode.
func cloudReachStatus(cfg *config.Config, w io.Writer) {
	st, err := cloud.OpenStateWithKeys(cfg.DataDir, cloudKeys())
	if err != nil {
		fmt.Fprintln(w, "Couldn't read this machine's link:", err)
		return
	}
	cl, kind := st.Current(time.Now())
	if kind == cloud.None {
		fmt.Fprintln(w, "This machine isn't linked. Run `mirrin reach use cloud` to link it.")
		return
	}
	for _, h := range cl.Hosts {
		fmt.Fprintf(w, "Address: https://%s (%s)\n", h, kind)
	}
	for _, r := range cl.Relays {
		fmt.Fprintf(w, "Relay: %s %s\n", r.ID, r.URL)
	}
	if fb := cfg.Reach.Fallback; fb != "" {
		fmt.Fprintf(w, "Fallback: %s\n", fb)
	}
	fmt.Fprintln(w, "Check it from outside with `mirrin reach verify`.")
}

// cloudVerifyEndpoint is what `mirrin reach verify` checks in cloud mode.
func cloudVerifyEndpoint(cfg *config.Config) (*reach.Endpoint, error) {
	st, err := cloud.OpenStateWithKeys(cfg.DataDir, cloudKeys())
	if err != nil {
		return nil, err
	}
	return reach.LocalCloudEndpoint(st, cfg.DataDir)
}
