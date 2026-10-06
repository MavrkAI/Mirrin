package daemon

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/push"
	"github.com/MavrkAI/Mirrin/internal/stepup"
)

// Step-up: another device approving something that can't easily be undone
// needs Face ID or a passkey (package stepup, docs/cloud-design.md §8).
// This computer and the owner's "yes 12" in their own chat app don't.

// stepUps holds each running twin's verifier while its Run lasts.
var stepUps sync.Map // *Daemon → *stepup.Verifier

// attachStepUp turns on passkeys for the API's approvals from other
// devices, at reach.step_up, and tells the owner about every passkey
// enrolled.
func (d *Daemon) attachStepUp(ctx context.Context, srv *api.Server) {
	v := stepup.New(srv.Devices())
	v.OnEnrol(d.passkeyEnrolled)
	srv.WithStepUp(v, func() string { return d.Config().Reach.StepUp })
	stepUps.Store(d, v)
	context.AfterFunc(ctx, func() { stepUps.CompareAndDelete(d, v) })
}

func (d *Daemon) stepUp() *stepup.Verifier {
	if v, ok := stepUps.Load(d); ok {
		return v.(*stepup.Verifier)
	}
	return nil
}

// PasskeyRevoker is what the certificate alarm's playbook uses to remove
// passkeys enrolled while an impostor could have held the twin's name; nil
// while the API is off.
func (d *Daemon) PasskeyRevoker() stepup.PasskeyRevoker {
	if v := d.stepUp(); v != nil {
		return v
	}
	return nil
}

// ApprovalDetail is one approval for the focused page and step-up.
func (d *Daemon) ApprovalDetail(ctx context.Context, id int64) (api.ApprovalDetail, error) {
	ap, err := d.store.GetApproval(ctx, id)
	if err != nil || ap == nil {
		return api.ApprovalDetail{}, api.ErrNoApproval
	}
	card := d.approvalCard(ctx, *ap) // approval_cards.go: the screenshot, as the screen shows it
	risk := ap.Risk.String()
	if d.dangerous(ctx, *ap) {
		risk = "dangerous" // what it could do now, if more than when it was asked
	}
	det := api.ApprovalDetail{
		ID: ap.ID, Tool: ap.Tool, Summary: ap.Summary, What: label(*ap), Risk: risk, Status: ap.Status,
		DecidedBy: ap.DecidedBy, At: ap.ResolvedAt, CreatedAt: ap.CreatedAt, Screenshot: card.Screenshot,
		Input: ap.Input, Why: d.approvalWhy(ctx, *ap),
	}
	if detail, ok := detailed(*ap); ok {
		det.Detail = detail
	}
	return det, nil
}

// approvalWhy says who or what asked for an approval.
func (d *Daemon) approvalWhy(ctx context.Context, ap memory.Approval) string {
	if who, ok := d.agent.ApprovalRequester(ctx, ap.ID); ok {
		return who + " asked for this."
	}
	if t, ok := d.tasks.ByKey(ap.ChatKey); ok {
		return "Part of the task " + t.Title + "."
	}
	switch ch := channelOf(homeKey(ap.ChatKey)); ch {
	case "", "screen", "api", "cli", "voice":
		return ""
	default:
		return "Asked in " + channelLabel(ch) + "."
	}
}

// DecideApprovalVia is DecideApproval by the device the API let in, saying
// how it proved itself (passkey), for the approval and the audit log.
func (d *Daemon) DecideApprovalVia(ctx context.Context, id int64, approve bool, method string) (string, error) {
	return d.DecideApprovalBy(ctx, id, approve, peerDecider(ctx, Decider{Method: method})) // devices.go
}

// stepUpNeeded reports whether the device the request came from would
// need a passkey to decide ap this way, under reach.step_up: never for
// this computer or the owner's own chat app, which carry no device. The
// risk is the tool's danger now (dangerous), not only as it was asked,
// as the approve button judges it.
func (d *Daemon) stepUpNeeded(ctx context.Context, ap memory.Approval, approve bool) bool {
	p := api.PeerFrom(ctx)
	if p.Loopback || p.Device == nil && !p.Master {
		return false
	}
	risk, decision := ap.Risk.String(), "deny"
	if d.dangerous(ctx, ap) {
		risk = "dangerous"
	}
	if approve {
		decision = "approve"
	}
	return stepup.Required(stepup.ParseLevel(d.Config().Reach.StepUp), risk, decision)
}

// refuseRemoteYes keeps another device from deciding in chat ("yes 12")
// what its approve button couldn't without a passkey. The yes goes to the
// focused page instead, where Face ID can confirm it. A no, and a yes to
// anything less, is taken as usual; so is anything from this computer or
// the owner's own chat app, which carries no device.
func (d *Daemon) refuseRemoteYes(ctx context.Context, ap memory.Approval, approve bool) (string, bool) {
	if ap.Status != "pending" || !d.stepUpNeeded(ctx, ap, approve) {
		return "", false
	}
	d.store.Audit(ctx, "approval.stepup_needed", ap.ChatKey, fmt.Sprintf("#%d %s from %s", ap.ID, ap.Tool, peerDecider(ctx, Decider{Method: "reply"})))
	where := "the Mac"
	if runtime.GOOS != "darwin" {
		where = "the computer I run on"
	}
	return fmt.Sprintf("#%d can't easily be undone, so from this device it needs Face ID or a passkey: open /approve/%d here and tap %s. Or approve it on %s, or reply \"yes %d\" in your own chat app.",
		ap.ID, ap.ID, map[bool]string{true: "Approve", false: "Deny"}[approve], where, ap.ID), true
}

// passkeyEnrolled tells the owner a device can now approve with a passkey:
// in the audit log, in their own chat (or on the screen and the desktop),
// and with a push to every other device. It asks for a backup soon, so a
// restore keeps it.
func (d *Daemon) passkeyEnrolled(e stepup.Enrolment) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	how := map[stepup.GrantKind]string{
		stepup.GrantPairing: "just after it paired",
		stepup.GrantPasskey: "with a passkey it already had",
		stepup.GrantOwner:   "because you allowed it",
	}[e.Grant]
	d.store.Audit(ctx, "passkey.enrolled", "", fmt.Sprintf("%s %q (%s) for %s", e.Device.ID, e.Device.Name, e.Grant, e.Passkey.RPID))
	d.BackupSoon("passkey enrolled")
	id := shortID(e.Device.ID)
	text := func(inChat bool) string {
		revoke := "run `mirrin devices revoke " + id + "` on this computer"
		if inChat {
			revoke = "reply /revoke " + id
		}
		return fmt.Sprintf("%q can now approve with Face ID or a passkey (set up %s). If that wasn't you, %s and I'll cut that device off at once.", e.Device.Name, how, revoke)
	}
	if p, ok := pushDispatchers.Load(d); ok {
		p.(*push.Dispatcher).Enqueue(push.Notification{Kind: "security", Title: d.Config().Name + ": new passkey", Body: fmt.Sprintf("%q can now approve with a passkey. Wasn't you? Open %s.", e.Device.Name, d.Config().Name), URL: "/ui", Tag: "passkey-" + id}, e.Device.ID)
	}
	if owner := d.ownerChatKey(); owner != "" {
		err := d.Notify(ctx, owner, text(true))
		if err == nil {
			return
		}
		d.log.Warn("tell the owner about a new passkey", "chat", owner, "err", err)
	}
	msg := text(false)
	d.bus.Publish(events.Event{Kind: "notice", Text: msg})
	if err := desktopNotify(d.Config().Name, msg); err != nil {
		d.log.Warn("tell the owner about a new passkey", "err", err)
	}
}

// passkeyCommand is /passkey <device>: the owner lets that device set up
// Face ID or a passkey for approvals in the next 15 minutes. Only the owner
// can: from this computer or their own chat app, never from a paired
// device, which could otherwise let itself.
func (d *Daemon) passkeyCommand(ctx context.Context, key string, fields []string) (string, error) {
	if forgeable(channelOf(key)) {
		return "I don't take /passkey by email or IRC, where anyone can claim to be you. Send it from your own chat app or `mirrin chat` on this computer.", nil
	}
	if p := api.PeerFrom(ctx); (p.Device != nil || p.Master) && !p.Loopback || p.Lacks(devices.Admin) {
		return "Only you can let a device set up a passkey: send /passkey from your own chat app, or in `mirrin chat` on the computer I run on.", nil
	}
	v, h := d.stepUp(), d.deviceHub()
	if v == nil || h == nil || h.store == nil {
		return "No device can reach me right now (my pages for other devices are switched off, or the list of paired devices can't be read), so there's nothing to set up.", nil
	}
	if len(fields) < 2 {
		var b strings.Builder
		for _, dev := range h.store.List() {
			if dev.Revoked() || dev.Local() || !dev.Has(devices.Approve) {
				continue
			}
			fmt.Fprintf(&b, "%s  %s (%d passkey%s)\n", shortID(dev.ID), dev.Name, dev.PasskeyCount, map[bool]string{true: "", false: "s"}[dev.PasskeyCount == 1])
		}
		if b.Len() == 0 {
			return "No paired device can approve requests. Pair one with `mirrin pair`.", nil
		}
		return "Devices that can approve:\n" + b.String() + "Reply /passkey <id> to let one set up Face ID or a passkey in the next 15 minutes.", nil
	}
	dev, err := h.store.Find(fields[1])
	switch {
	case errors.Is(err, devices.ErrAmbiguous):
		return fmt.Sprintf("More than one device starts with %q; send more of its id (/passkey lists them).", fields[1]), nil
	case err != nil:
		return fmt.Sprintf("I don't have a paired device starting with %q. Send /passkey to see the list.", fields[1]), nil
	}
	if err := v.AllowEnrolment(dev.ID); err != nil {
		return fmt.Sprintf("I don't have a paired device starting with %q. Send /passkey to see the list.", fields[1]), nil
	}
	d.store.Audit(ctx, "passkey.allowed", key, fmt.Sprintf("%s %q", dev.ID, dev.Name))
	return fmt.Sprintf("OK: for the next %d minutes, %q can set up Face ID or a passkey for approvals. On it, open me and choose App, then Set up Face ID for approvals.", int(stepup.OwnerGrantTTL.Minutes()), dev.Name), nil
}
