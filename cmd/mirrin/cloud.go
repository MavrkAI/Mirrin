package main

// mirrin cloud: link this machine to Mirrin Cloud, see where it stands, and
// see everything it has sent. Nothing here runs unless the user types it,
// and only link, me, billing, unlink and delete-account send anything.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/reach"
)

const cloudUsage = `Mirrin Cloud is optional: an address that works from anywhere, and off-site
backup. Mirrin is complete without it, and nothing is sent anywhere until you
run link.

Usage:
  mirrin cloud link [--handle NAME]      Pay for a handle and link this machine (checkout opens in your browser)
  mirrin cloud status                    Where this machine stands (sends nothing)
  mirrin cloud me                        Everything Mirrin Cloud stores about you
  mirrin cloud billing                   Open the billing page: receipts, card, cancelling
  mirrin cloud egress                    Every request this machine has sent to Mirrin Cloud (sends nothing)
  mirrin cloud unlink [--local]          Unlink this machine (--local: without reaching the server)
  mirrin cloud delete-account [--undo]   Delete the account after seven days (--undo cancels)
`

// Seams for tests: where checkout pages open, how often a link is polled,
// and which keys verify entitlements.
var (
	openBrowser   = openURL
	linkPollEvery = 2 * time.Second
	cloudKeys     = func() map[string]ed25519.PublicKey { return entitle.EntitlementKeys }
)

// cloudCmd runs `mirrin cloud …`.
func cloudCmd(ctx context.Context, args []string) error {
	cfg, err := cloudConfig()
	if err != nil {
		return err
	}
	return runCloud(ctx, cfg, args, os.Stdin, os.Stdout)
}

// cloudConfig reads the config without writing one: a machine with no
// config yet gets the defaults, which name no Cloud API.
func cloudConfig() (*config.Config, error) {
	if _, err := os.Stat(config.Path()); errors.Is(err, os.ErrNotExist) {
		return config.Default(), nil
	}
	return config.Load()
}

func runCloud(ctx context.Context, cfg *config.Config, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, cloudUsage)
		return nil
	}
	sub, fs := args[0], flag.NewFlagSet("mirrin cloud "+args[0], flag.ContinueOnError)
	fs.SetOutput(out)
	handle := fs.String("handle", "", "the handle to ask for (default: a random one)")
	api := fs.String("api", "", "the control plane, https://host[:port] (default: cloud.api in config, else "+cloud.DefaultAPI+")")
	acme := fs.String("acme-account", "", "the ACME account URI the handle's CAA record names")
	local := fs.Bool("local", false, "unlink without reaching the server")
	undo := fs.Bool("undo", false, "cancel a deletion")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("mirrin cloud %s: unexpected %q", sub, fs.Arg(0))
	}
	ask := func(prompt string) string {
		fmt.Fprint(out, prompt)
		line, _ := bufio.NewReader(in).ReadString('\n')
		return strings.TrimSpace(line)
	}
	switch sub {
	case "link":
		if *acme == "" {
			// An account this machine has already; otherwise the daemon
			// sends it before its first certificate.
			*acme = reach.LinkedAccount(cfg.DataDir, cfg.Reach)
		}
		return cloudLink(ctx, cfg, *api, *handle, *acme, out)
	case "status":
		return cloudStatus(cfg, out)
	case "me":
		c, err := linkedClient(cfg)
		if err != nil {
			return err
		}
		me, err := c.Me(ctx)
		if err != nil {
			return err
		}
		var b bytes.Buffer
		if err := json.Indent(&b, me, "", "  "); err != nil {
			return err
		}
		fmt.Fprintln(out, graphicOnly(bytes.TrimSpace(b.Bytes())))
		return nil
	case "billing":
		c, err := linkedClient(cfg)
		if err != nil {
			return err
		}
		u, err := c.BillingPortal(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Opening the billing page in your browser. If it does not open, visit:\n  %s\n", u)
		openBrowser(u)
		return nil
	case "egress":
		return cloudEgress(cfg, out)
	case "unlink":
		return cloudUnlink(ctx, cfg, *local, *yes, ask, out)
	case "delete-account":
		return cloudDelete(ctx, cfg, *undo, *yes, ask, out)
	}
	fmt.Fprint(out, cloudUsage)
	return fmt.Errorf("unknown command: mirrin cloud %s", sub)
}

// linkedClient is the client for the control plane this machine linked
// with.
func linkedClient(cfg *config.Config) (*cloud.Client, error) {
	c, err := cloud.OpenLinked(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("this machine is not linked to Mirrin Cloud (mirrin cloud link)")
	}
	return c, nil
}

func cloudLink(ctx context.Context, cfg *config.Config, api, handle, acme string, out io.Writer) error {
	keys := cloudKeys()
	if len(keys) == 0 {
		return errors.New("this build trusts no Mirrin Cloud signing key, so it could not check what it paid for. " +
			"Mirrin Cloud has not launched yet; development builds add the dev keys with -tags mirrin_devkeys")
	}
	if api == "" {
		api = cfg.Cloud.API
	}
	if api == "" {
		api = cloud.DefaultAPI
	}
	c, err := cloud.New(cfg.DataDir, api, keys)
	if err != nil {
		return err
	}
	// A checkout started earlier is resumed, never started again: it may
	// have been paid in a tab this terminal did not see.
	// sent is the ACME account this call sent with the checkout. A resumed
	// checkout was started without it (or with another), so the daemon
	// sends the account before its first certificate instead.
	sent := ""
	ls, started, resumed := c.PendingLink()
	if resumed {
		fmt.Fprintf(out, "Resuming the checkout started at %s.\n", started.Local().Format("2006-01-02 15:04"))
	} else if ls, err = startLink(ctx, c, handle, acme, out); err != nil {
		return err
	} else {
		sent = acme
	}
	for {
		st, _, err := c.PollLink(ctx, ls.ID)
		if err != nil {
			return err
		}
		switch st {
		case cloud.LinkActive:
			cl, kind := c.State().Current(time.Now())
			if sent != "" {
				// The control plane pinned this account in the handle's CAA
				// record at link; the daemon needn't send it again.
				_ = reach.NotePinned(cfg.DataDir, cl.Handle, cl.Gen, sent)
			}
			host := cl.Handle
			if len(cl.Hosts) > 0 {
				host = cl.Hosts[0]
			}
			fmt.Fprintf(out, "Linked: this machine is %s (%s), paid through %s.\n", host, kind, cl.PaidThrough.Local().Format(time.DateOnly))
			fmt.Fprintln(out, "Nothing else changes until you choose Mirrin Cloud for reach (mirrin reach use cloud) or backup.")
			return nil
		case cloud.LinkExpired:
			if !resumed {
				return errors.New("the checkout expired before it was paid; run mirrin cloud link to start again")
			}
			resumed = false
			fmt.Fprintln(out, "That checkout expired unpaid; starting a new one.")
			if ls, err = startLink(ctx, c, handle, acme, out); err != nil {
				return err
			}
			sent = acme
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(linkPollEvery):
		}
	}
}

// startLink starts a checkout and opens it in the browser.
func startLink(ctx context.Context, c *cloud.Client, handle, acme string, out io.Writer) (cloud.LinkStart, error) {
	ls, err := c.StartLink(ctx, handle, acme)
	if err != nil {
		return ls, err
	}
	fmt.Fprintln(out, "Sent this machine's public key to "+c.API()+"; nothing else about it.")
	fmt.Fprintf(out, "Opening checkout in your browser. If it does not open, visit:\n  %s\n", ls.CheckoutURL)
	fmt.Fprintln(out, "Waiting for payment (Ctrl-C stops waiting; mirrin cloud link resumes)…")
	openBrowser(ls.CheckoutURL)
	return ls, nil
}

func cloudStatus(cfg *config.Config, out io.Writer) error {
	st, err := cloud.OpenStateWithKeys(cfg.DataDir, cloudKeys())
	if err != nil {
		return err
	}
	info, linked, err := st.Info()
	if err != nil {
		return err
	}
	if !linked {
		fmt.Fprintln(out, "Not linked. Mirrin is complete without Cloud, and this machine sends it nothing.")
		return nil
	}
	now := time.Now()
	cl, kind := st.Current(now)
	day := func(t time.Time) string { return t.Local().Format(time.DateOnly) }
	fmt.Fprintf(out, "State:          %s\n", kind)
	switch kind {
	case cloud.Grace:
		fmt.Fprintf(out, "                payment lapsed on %s; everything works until %s\n", day(cl.PaidThrough), day(cl.Exp))
	case cloud.Superseded:
		fmt.Fprintf(out, "                another machine took over this handle on %s; this one is standing by\n", day(info.Superseded.At))
	case cloud.Expired:
		switch {
		case !info.RevokedAt.IsZero():
			fmt.Fprintf(out, "                Mirrin Cloud revoked this machine's link on %s\n", day(info.RevokedAt))
		case cl.Exp.IsZero():
			fmt.Fprintln(out, "                the stored entitlement does not verify in this build")
		default:
			fmt.Fprintf(out, "                the entitlement expired on %s\n", day(cl.Exp))
		}
	}
	fmt.Fprintf(out, "Handle:         %s (generation %d)\n", info.Handle, info.Gen)
	for _, h := range cl.Hosts {
		fmt.Fprintf(out, "Address:        https://%s\n", h)
	}
	if !cl.PaidThrough.IsZero() {
		fmt.Fprintf(out, "Paid through:   %s\n", day(cl.PaidThrough))
		fmt.Fprintf(out, "Valid until:    %s (refreshed about daily)\n", day(cl.Exp))
	}
	fmt.Fprintf(out, "Control plane:  %s\n", info.API)
	fmt.Fprintf(out, "Linked:         %s\n", day(info.LinkedAt))
	if !info.DeleteAt.IsZero() {
		fmt.Fprintf(out, "Deletion:       scheduled for %s (mirrin cloud delete-account --undo cancels)\n", day(info.DeleteAt))
	}
	return nil
}

func cloudEgress(cfg *config.Config, out io.Writer) error {
	es, bad, err := cloud.ReadLedger(cfg.DataDir)
	if err != nil {
		return err
	}
	if len(es) == 0 && bad == 0 {
		fmt.Fprintln(out, "Nothing has been sent to Mirrin Cloud from this machine.")
		return nil
	}
	var sent, recv int64
	for _, e := range es {
		status := fmt.Sprint(e.Status)
		if e.Status == 0 {
			status = "no answer"
		}
		fmt.Fprintf(out, "%s  %-6s %s%s  sent %s, received %s  %s\n", e.At.Local().Format(time.DateTime), e.Method, e.Host, e.Path,
			size(e.ReqBytes), size(e.RespBytes), status)
		sent, recv = sent+e.ReqBytes, recv+e.RespBytes
	}
	fmt.Fprintf(out, "%d requests; %s sent and %s received, request and answer bodies only. Bodies are never kept here.\n", len(es), size(sent), size(recv))
	if bad > 0 {
		fmt.Fprintf(out, "(%d unreadable lines skipped)\n", bad)
	}
	return nil
}

func size(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}

func cloudUnlink(ctx context.Context, cfg *config.Config, local, yes bool, ask func(string) string, out io.Writer) error {
	c, err := cloud.OpenLinked(cfg.DataDir)
	if err != nil {
		return err
	}
	if c == nil {
		fmt.Fprintln(out, "Not linked.")
		return nil
	}
	info, _, _ := c.State().Info()
	if !yes && !strings.EqualFold(ask(fmt.Sprintf("Unlink this machine from %s? Reach through Mirrin Cloud stops here. "+
		"The subscription continues until you cancel it (mirrin cloud billing). [y/N] ", info.Handle)), "y") {
		return errors.New("not unlinked")
	}
	if local {
		err = c.Forget()
	} else if err = c.Unlink(ctx); err != nil {
		return fmt.Errorf("%w (if Mirrin Cloud cannot be reached or keeps refusing, mirrin cloud unlink --local forgets the link here only; "+
			"the key then stays valid there until its entitlement expires)", err)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Unlinked. This machine's cloud key is gone; the egress ledger stays.")
	return nil
}

func cloudDelete(ctx context.Context, cfg *config.Config, undo, yes bool, ask func(string) string, out io.Writer) error {
	c, err := linkedClient(cfg)
	if err != nil {
		return err
	}
	if undo {
		if err := c.RestoreAccount(ctx); err != nil {
			return err
		}
		fmt.Fprintln(out, "Deletion cancelled. Nothing was removed.")
		return nil
	}
	info, _, _ := c.State().Info()
	if !yes && ask(fmt.Sprintf("This deletes the Mirrin Cloud account for %s in seven days: its records, DNS and backups. "+
		"The handle is never given to anyone else. Type the handle to confirm: ", info.Handle)) != info.Handle {
		return errors.New("not deleted")
	}
	if err := c.DeleteAccount(ctx); err != nil {
		return err
	}
	info, _, _ = c.State().Info()
	fmt.Fprintf(out, "Scheduled for deletion on %s. Until then, mirrin cloud delete-account --undo cancels it.\n", info.DeleteAt.Local().Format(time.DateOnly))
	return nil
}

// graphicOnly returns an indented JSON document fit for a terminal: every
// rune that is not graphic (C0 and C1 controls, bidi overrides and other
// format characters, separators, private use) becomes a \u escape. JSON
// allows such runes only inside strings, where the escape means the same.
func graphicOnly(b []byte) string {
	var sb strings.Builder
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == '\n' || unicode.IsGraphic(r) && !(r == utf8.RuneError && n == 1) {
			sb.Write(b[:n])
		} else {
			for _, u := range utf16.Encode([]rune{r}) {
				fmt.Fprintf(&sb, `\u%04x`, u)
			}
		}
		b = b[n:]
	}
	return sb.String()
}

// openURL opens u, an http or https URL the cloud client has checked, in
// the user's browser.
func openURL(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	}
	return exec.Command("xdg-open", u).Start()
}
