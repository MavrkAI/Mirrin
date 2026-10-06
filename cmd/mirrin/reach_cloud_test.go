package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/reach"
)

// `mirrin reach use cloud --handle NAME` links an unlinked machine (the
// checkout opens in the browser and is paid there), then saves the mode,
// keeping the free mode in use as the fallback and warning that phones
// need setting up again.
func TestReachUseCloudLinksThenSwitches(t *testing.T) {
	_, cfg, opened := fakeCloud(t)
	openBrowser = func(u string) error {
		*opened = append(*opened, u)
		res, err := http.Get(u) // the fake checkout pays
		if err == nil {
			res.Body.Close()
		}
		return err
	}
	cfg.Reach.Mode = "tailscale"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := useCloud(context.Background(), cfg, []string{"--handle", "Ember-Otter-42", "--api=" + cfg.Cloud.API}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(*opened) != 1 {
		t.Fatalf("checkout opened %d times", len(*opened))
	}
	for _, want := range []string{"Linked: this machine is ember-otter-42.", "Heads up: this changes the address your phones use", "Restart Mirrin to use it", "goes back to tailscale"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Reach.Mode != "cloud" || saved.Reach.Fallback != "tailscale" || saved.Reach.StepUp == "" {
		t.Fatalf("saved %+v", saved.Reach)
	}

	// Already linked: it only switches, and sends nothing.
	*opened = nil
	out.Reset()
	cfg.Reach.Mode = "off"
	if err := useCloud(context.Background(), cfg, nil, &out); err != nil || len(*opened) != 0 {
		t.Fatalf("linked: %v %v", err, *opened)
	}
	var st strings.Builder
	cloudReachStatus(saved, &st)
	if !strings.Contains(st.String(), "Address: https://ember-otter-42.") || !strings.Contains(st.String(), "mirrin reach verify") {
		t.Fatalf("status:\n%s", st.String())
	}
	// Verify needs the machine's own keys, made when the daemon starts.
	if _, err := cloudVerifyEndpoint(saved); err == nil || !strings.Contains(err.Error(), "no HTTPS keys yet") {
		t.Fatalf("verify before the daemon ran: %v", err)
	}
	if err := useCloud(context.Background(), cfg, []string{"--handle", "no"}, &out); err == nil {
		t.Fatal("a bad handle")
	}
}

// `mirrin cloud link` sends the ACME account this machine already has at
// the configured CA, so the handle's CAA record pins it from the start,
// and records that it did.
func TestCloudLinkSendsAKnownACMEAccount(t *testing.T) {
	f, cfg, _ := fakeCloud(t)
	openBrowser = func(u string) error {
		res, err := http.Get(u)
		if err == nil {
			res.Body.Close()
		}
		return err
	}
	const acct = "https://acme-v02.api.letsencrypt.org/acme/acct/123456789"
	if _, err := cloudCLI(t, cfg, "", "link", "--handle", "ember-otter-42", "--acme-account", acct); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.ACMEAccount("ember-otter-42"); got != acct {
		t.Fatalf("CAA would pin %q", got)
	}
	if reach.PinnedAccount(cfg.DataDir, "ember-otter-42") != acct {
		t.Fatal("not recorded; the daemon would send it again")
	}
}

// A checkout started without an ACME account (from the Reach page, before
// this machine had one) and finished here must not record a pin it never
// sent: the handle's CAA would block every certificate, and the daemon
// would never send the account.
func TestResumedLinkRecordsNoUnsentPin(t *testing.T) {
	f, cfg, opened := fakeCloud(t)
	c, err := cloud.New(cfg.DataDir, f.URL, f.Keys())
	if err != nil {
		t.Fatal(err)
	}
	ls, err := c.StartLink(t.Context(), "ember-otter-42", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Pay(ls.ID); err != nil {
		t.Fatal(err)
	}
	const acct = "https://acme-v02.api.letsencrypt.org/acme/acct/123456789"
	out, err := cloudCLI(t, cfg, "", "link", "--acme-account", acct)
	if err != nil || !strings.Contains(out, "Resuming the checkout") || len(*opened) != 0 {
		t.Fatalf("%v %v\n%s", err, *opened, out)
	}
	if got, _ := f.ACMEAccount("ember-otter-42"); got != "" {
		t.Fatalf("the control plane was told %q", got)
	}
	if got := reach.PinnedAccount(cfg.DataDir, "ember-otter-42"); got != "" {
		t.Fatalf("recorded a pin that was never sent: %q", got)
	}
}

// A copy standing by after a backup handover sends nothing, not even a
// checkout, when asked to use the paid address.
func TestReachUseCloudRefusesWhileStandingBy(t *testing.T) {
	f, cfg, opened := fakeCloud(t)
	if err := backup.StandBy(cfg.DataDir, backup.Handover{Name: "handover-20260927T090000Z-0a0b0c0d.age", HostLabel: "Akshay's Mac mini", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := useCloud(context.Background(), cfg, []string{"--api=" + cfg.Cloud.API}, &out)
	if err == nil || !strings.Contains(err.Error(), "mirrin backup resume") {
		t.Fatalf("use cloud while standing by: %v", err)
	}
	if n := len(f.Requests()); n != 0 || len(*opened) != 0 {
		t.Fatalf("%d requests, %d pages opened", n, len(*opened))
	}
}
