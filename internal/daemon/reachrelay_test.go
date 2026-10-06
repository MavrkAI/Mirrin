package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/reach"
)

func TestAlarmCommandClearsFromOwnerChat(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	ctx := context.Background()
	if got, _ := d.command(ctx, "cli:local", "/alarm"); !strings.Contains(got, "not reachable through a relay") {
		t.Fatalf("no relay: %q", got)
	}
	a, err := reach.OpenAlarm(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	relayAlarms.Store(d, a)
	defer relayAlarms.Delete(d)
	f := certwatch.Finding{Kind: certwatch.UnknownIssuance, Severity: certwatch.Critical, Host: "twin.example.com", Detail: "A stray certificate.", Issuance: &certwatch.Issuance{SPKI: "x"}, NotBefore: time.Now()}
	if err := reach.Playbook(ctx, f, reach.Deps{Alarm: a}); err != nil || !a.Active() {
		t.Fatal(err)
	}
	if got, _ := d.command(ctx, "mail:someone@example.com", "/alarm clear"); !strings.Contains(got, "don't take /alarm by email") || !a.Active() {
		t.Fatalf("forgeable channel: %q", got)
	}
	if got, _ := d.command(ctx, "cli:local", "/alarm"); !strings.Contains(got, "A stray certificate.") {
		t.Fatalf("status: %q", got)
	}
	if got, err := d.command(ctx, "cli:local", "/alarm clear"); err != nil || !strings.Contains(got, "Alarm cleared") || a.Active() {
		t.Fatalf("clear: %q %v", got, err)
	}
	if got, _ := d.command(ctx, "cli:local", "/alarm clear"); !strings.Contains(got, "no alarm to clear") {
		t.Fatalf("again: %q", got)
	}
}

// Regression (review of the certificate alarm): the pause covered only the
// approval routes, so a remote device that may approve still decided by
// saying "yes 1" in chat, or in its own words through resolve_approval.
// A token the impostor took over the rogue certificate would do the same.
func TestAlarmHoldsDecisionsInChatFromRemoteDevices(t *testing.T) {
	td := newTestDaemon(t, settler(map[string]string{"yep, send that one to him": `{"id":1,"decision":"approve"}`}))
	ctx := context.Background()
	td.owner(t, "email the boss")
	a, err := reach.OpenAlarm(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	phone := devices.Device{ID: "phone", Name: "Phone", Kind: devices.KindPWA, Scopes: []devices.Scope{devices.View, devices.Chat, devices.Approve}, Via: "relay"}
	remote := api.WithAlarmHold(api.WithPeer(ctx, api.Peer{Device: &phone, Via: "relay"}), a)
	f := certwatch.Finding{Kind: certwatch.UnknownIssuance, Severity: certwatch.Critical, Host: "twin.example.com", Detail: "A stray certificate.", Issuance: &certwatch.Issuance{SPKI: "x"}, NotBefore: time.Now()}
	if err := reach.Playbook(ctx, f, reach.Deps{Alarm: a}); err != nil || !a.Active() {
		t.Fatal(err)
	}
	for _, text := range []string{"yes 1", "yep, send that one to him", "yes always"} {
		got, err := td.message(remote, channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: text, IsOwner: true}, agent.Events{})
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if len(td.ran()) != 0 {
			t.Fatalf("%q decided during the alarm: reply %q", text, got)
		}
		if text == "yes 1" && !strings.Contains(got, "paused approvals from other devices") {
			t.Fatalf("%q: reply %q", text, got)
		}
	}
	if ap, _ := td.store.GetApproval(ctx, 1); ap.Status != "pending" {
		t.Fatalf("#1 is %s", ap.Status)
	}
	// Once the owner clears it, the same device's yes decides.
	if err := a.Clear("cli:local"); err != nil {
		t.Fatal(err)
	}
	if got, err := td.message(remote, channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: "yes 1", IsOwner: true}, agent.Events{}); err != nil || len(td.ran()) != 1 {
		t.Fatalf("after clear: %q %v, sent %v", got, err, td.ran())
	}
}

// Regression (review of the certificate alarm): a paired device with the
// admin scope cleared the alarm over the public name with "/alarm clear",
// though §6.5 lifts the pause only on this computer or the owner's chat.
func TestAlarmClearRefusedFromRemoteAdmin(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	ctx := context.Background()
	a, err := reach.OpenAlarm(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	relayAlarms.Store(d, a)
	defer relayAlarms.Delete(d)
	f := certwatch.Finding{Kind: certwatch.UnknownIssuance, Severity: certwatch.Critical, Host: "twin.example.com", Detail: "A stray certificate.", Issuance: &certwatch.Issuance{SPKI: "x"}, NotBefore: time.Now()}
	if err := reach.Playbook(ctx, f, reach.Deps{Alarm: a}); err != nil || !a.Active() {
		t.Fatal(err)
	}
	admin := devices.Device{ID: "admin-phone", Name: "Phone", Kind: devices.KindPWA, Scopes: []devices.Scope{devices.View, devices.Chat, devices.Approve, devices.Admin}}
	for _, via := range []string{"relay", "lan", "tailscale"} {
		remote := api.WithPeer(ctx, api.Peer{Device: &admin, Via: via})
		if got, _ := d.command(remote, "screen:local", "/alarm clear"); !strings.Contains(got, "don't take /alarm from another device") || !a.Active() {
			t.Fatalf("remote admin via %s: %q", via, got)
		}
	}
	// This computer's own key on loopback clears it.
	local := api.WithPeer(ctx, api.Peer{Master: true, Loopback: true, Via: "loopback"})
	if got, err := d.command(local, "cli:local", "/alarm clear"); err != nil || !strings.Contains(got, "Alarm cleared") || a.Active() {
		t.Fatalf("loopback: %q %v", got, err)
	}
}
