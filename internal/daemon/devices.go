package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// deviceHub is a running twin's device registry and the API that checks keys
// against it: /revoke must cut devices off in the very instance the API uses.
type deviceHub struct {
	srv   *api.Server
	store *devices.Store // nil when devices.json couldn't be read
	err   error          // why it couldn't
	path  string
}

// deviceHubs holds each running twin's hub while its Run lasts.
var deviceHubs sync.Map // *Daemon → *deviceHub

// attachDevices gives the API the registry in data/devices.json and the file
// its master key lives in, and tells the owner about every device that gets
// a key of its own. The hub is dropped when ctx (the twin's run) ends.
func (d *Daemon) attachDevices(ctx context.Context, srv *api.Server) {
	c := d.Config()
	srv.WithName(c.Name).WithTokenFile(api.TokenPath(c.DataDir))
	h := &deviceHub{srv: srv, path: devices.Path(c.DataDir)}
	store, err := devices.Open(h.path)
	if err != nil {
		// Pages opened from the menu still work (in memory); paired devices
		// don't until the file is fixed. Never overwrite it.
		h.err = err
		d.log.Warn("paired devices can't be read; other devices can't reach me until it's fixed", "err", err)
		_ = desktopNotify(c.Name, "I couldn't read the list of paired devices, so they can't reach me for now. "+err.Error())
	} else {
		h.store = store
		srv.WithDevices(store).OnPaired(d.devicePaired)
		store.OnChange(d.deviceChanged)
	}
	deviceHubs.Store(d, h)
	context.AfterFunc(ctx, func() { deviceHubs.CompareAndDelete(d, h) })
}

func (d *Daemon) deviceHub() *deviceHub {
	if h, ok := deviceHubs.Load(d); ok {
		return h.(*deviceHub)
	}
	return nil
}

// deviceStore is the registry the API checks keys against, if it could be read.
func (d *Daemon) deviceStore() *devices.Store {
	if h := d.deviceHub(); h != nil {
		return h.store
	}
	return nil
}

// devicePaired tells the owner, where they'll see it, that a device now has a
// key, and how to cut it off if it wasn't them: in their own chat (which the
// presence screen shows too), or, with no chat to reach them in, on the
// presence screen and in a desktop notification.
func (d *Daemon) devicePaired(e api.PairEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := d.Config()
	d.store.Audit(ctx, "device.paired", "", fmt.Sprintf("%s %q (%s, %s) via %s from %s", e.Device.ID, e.Device.Name, e.Device.Kind, e.How, e.Via, e.IP))
	if owner := d.ownerChatKey(); owner != "" {
		err := d.Notify(ctx, owner, pairedNotice(e, true))
		if err == nil {
			return
		}
		d.log.Warn("tell the owner about a new device", "chat", owner, "err", err)
	}
	text := pairedNotice(e, false)
	d.bus.Publish(events.Event{Kind: "notice", Text: text}) // the presence screen
	if err := desktopNotify(c.Name, text); err != nil {
		d.log.Warn("tell the owner about a new device", "err", err)
	}
}

// deviceChanged asks for a backup soon after a device is paired or cut off
// (backup.go), so a restore never brings back a device the owner removed.
// A browser on this computer getting its own key is not a change of who can
// reach the twin.
func (d *Daemon) deviceChanged(c devices.Change) {
	if c.Device.Local() || (c.Kind != devices.Added && c.Kind != devices.Revoked) {
		return
	}
	d.BackupSoon("device " + string(c.Kind))
}

// pairedNotice is the owner's message about a device that got a key. inChat
// says whether it goes to their chat, where they can reply /revoke; a desktop
// notification can't be replied to.
func pairedNotice(e api.PairEvent, inChat bool) string {
	where := ""
	if e.IP != "" {
		where = " from " + e.IP
	}
	switch e.Via {
	case "tailscale":
		where += " over Tailscale"
	case "lan":
		where += " on your network"
	case "", "loopback":
	default:
		where += " through " + e.Via
	}
	can := "It can " + api.DescribeScopes(e.Device.Scopes) + "."
	id := shortID(e.Device.ID)
	revoke := "run `mirrin devices revoke " + id + "` on this computer (or send /revoke " + id + " in `mirrin chat`)"
	if inChat {
		revoke = "reply /revoke " + id
	}
	shared := "It came in with the old shared key. If you don't recognise it, " + revoke +
		": I'll cut it off and change that key, so anything else still paired the old way will need pairing again."
	switch e.How {
	case api.HowLegacyScreen:
		return fmt.Sprintf("A screen set up before devices had their own keys (%q%s) now has a key of its own. %s\n%s", e.Device.Name, where, can, shared)
	case api.HowLegacyCode:
		return fmt.Sprintf("A computer paired with an old-style code (%q%s) now has a key of its own. %s\n%s", e.Device.Name, where, can, shared)
	}
	return fmt.Sprintf("New device paired with me: %q, %s%s. %s\nIf that wasn't you, %s and I'll cut it off at once.", e.Device.Name, kindWords(e.Device.Kind), where, can, revoke)
}

func kindWords(kind string) string {
	switch kind {
	case devices.KindKiosk:
		return "a wall screen"
	case devices.KindCLI:
		return "another computer's terminal"
	}
	return "a phone or tablet"
}

// shortID is enough of a device id to type.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// revokeCommand is /revoke: with no id it lists the paired devices, with one
// (or its first few characters) it cuts that device off at once. It manages
// devices, so a paired device without the admin scope can't send it, and
// neither can a chat whose sender anyone could claim to be.
func (d *Daemon) revokeCommand(ctx context.Context, key string, fields []string) (string, error) {
	if forgeable(channelOf(key)) {
		return "I don't take /revoke by email or IRC, where anyone can claim to be you. Send it from your own chat app or `mirrin chat`, or run `mirrin devices` on the computer I run on.", nil
	}
	if api.PeerFrom(ctx).Lacks(devices.Admin) {
		return "Paired devices are managed from the computer I run on: send /revoke there (on the screen from my menu, or in `mirrin chat`), or from your own chat app, or run `mirrin devices`.", nil
	}
	h := d.deviceHub()
	switch {
	case h == nil:
		return "No devices can reach me right now: my pages for other devices are switched off, so there is nothing to cut off.", nil
	case h.store == nil:
		return fmt.Sprintf("I couldn't read the list of paired devices (%s), so I can't change it. Fix that file, or move it aside (which unpairs every device), then restart me.", h.path), nil
	}
	store := h.store
	if len(fields) < 2 {
		var b strings.Builder
		for _, dev := range store.List() {
			if dev.Revoked() || dev.Local() {
				continue
			}
			seen := "never used"
			if !dev.LastSeen.IsZero() {
				seen = "last used " + dev.LastSeen.In(d.location()).Format("02 Jan 15:04")
			}
			fmt.Fprintf(&b, "%s  %s (%s, %s)\n", shortID(dev.ID), dev.Name, kindWords(dev.Kind), seen)
		}
		if b.Len() == 0 {
			return "No devices are paired with me. Pair one with `mirrin pair`.", nil
		}
		return "Paired devices:\n" + b.String() + "Reply /revoke <id> to cut one off.", nil
	}
	dev, err := store.Find(fields[1])
	switch {
	case errors.Is(err, devices.ErrAmbiguous):
		return fmt.Sprintf("More than one device starts with %q; send more of its id (/revoke lists them).", fields[1]), nil
	case err != nil:
		return fmt.Sprintf("I don't have a paired device starting with %q. Send /revoke to see the list.", fields[1]), nil
	}
	res, err := h.srv.RevokeDevice(dev.ID)
	if err != nil {
		return "", err
	}
	detail := fmt.Sprintf("%s %q", dev.ID, dev.Name)
	msg := fmt.Sprintf("Done: %q can't reach me any more.", dev.Name)
	switch {
	case res.KeyChanged:
		detail += "; master key changed"
		msg += " It came in with the old shared key, so I changed that key too: anything else still paired the old way needs pairing again with `mirrin pair`. This computer's menu and terminal keep working."
	case res.KeyError != "":
		msg += fmt.Sprintf(" It came in with the old shared key, which I couldn't change (%s), so whoever holds that key can still reach me. To change it, delete %s and restart me.", res.KeyError, api.TokenPath(d.Config().DataDir))
	}
	d.store.Audit(ctx, "device.revoked", "", detail)
	return msg + " If it was yours after all, pair it again with `mirrin pair`.", nil
}

// commandScope is what a paired device must hold to send an owner command:
// view to read the owner's own information, and for what acts or clears
// the conversation every paired screen shares, approve, as a device that
// may act for the owner has (/revoke checks admin itself).
func commandScope(fields []string) (devices.Scope, bool) {
	switch strings.ToLower(fields[0]) {
	case "/status", "/pending", "/spend", "/protocols", "/audit":
		return devices.View, true
	case "/tasks":
		if len(fields) >= 2 && strings.EqualFold(fields[1], "retry") {
			return devices.Approve, true
		}
		return devices.View, true
	case "/reload", "/forget":
		return devices.Approve, true
	}
	return "", false
}

// deviceCantCommand answers an owner command from a paired device that
// doesn't hold the scope it needs. What arrives on the twin's own channels,
// or with this computer's own key, carries every scope.
func deviceCantCommand(ctx context.Context, fields []string) (string, bool) {
	sc, ok := commandScope(fields)
	p := api.PeerFrom(ctx)
	if !ok || !p.Lacks(sc) {
		return "", false
	}
	pair := "mirrin pair --screen"
	if p.Device != nil && p.Device.Kind == devices.KindCLI {
		pair = "mirrin pair"
	}
	if sc == devices.View {
		return "This device was paired without view, so it can't see that. Pair it again with view: `" + pair + " --scopes view,chat`.", true
	}
	return fmt.Sprintf("This device was paired to talk, not to act for you, so it can't send %s. Send it from a device that may approve, or your own chat app, or pair this device again with approve: `%s --scopes view,chat,approve`.", strings.ToLower(fields[0]), pair), true
}

// screenDecider is who decided on the presence screen's cards: the paired
// device the API let in (so the approval and the audit log name it), or the
// owner at this computer's screen or orb.
func screenDecider(ctx context.Context) Decider {
	return peerDecider(ctx, Decider{Method: "screen"})
}

// peerDecider is b with whoever the API let in: the paired device, named so
// the approval and the audit log say which one decided, and for a request
// that didn't come from this computer (a device, or the old shared key over
// api.remote), how and from where it came. A decision made in chat ("yes
// 12", the "yes, always" check, the owner's own words) is named the same
// way as one made on the screen's cards.
func peerDecider(ctx context.Context, b Decider) Decider {
	p := api.PeerFrom(ctx)
	if p.Device == nil && !p.Master {
		return b
	}
	if p.Device != nil {
		b.DeviceID, b.DeviceName = p.Device.ID, p.Device.Name
	}
	if !p.Loopback {
		b.Via = p.Via
		if p.ClientIP.IsValid() {
			b.IP = p.ClientIP.String()
		}
	}
	return b
}

// cantApprove reports whether the request comes from a device that may not
// decide requests: one paired without approve, or any device on a remote
// listener while the certificate alarm holds approvals (reachrelay.go).
func cantApprove(ctx context.Context) bool {
	return api.PeerFrom(ctx).Lacks(devices.Approve) || api.ApprovalsPaused(ctx)
}

// deviceCantApprove answers a yes or no sent from a paired device that may
// talk but not approve: it decides nothing, whichever chat it names. It lets
// everything else through (handled is false), including a bare yes when no
// request is waiting, which can only be conversation.
func (d *Daemon) deviceCantApprove(ctx context.Context, in channels.Inbound) (reply string, handled bool) {
	if !cantApprove(ctx) {
		return "", false
	}
	r, ok := d.parseReply(in.Text)
	if !ok {
		r, ok = parseAlways(in.Text, d.twinNames()) // "yes, always" (alwaysallow.go) is a yes too
	}
	if !ok {
		return "", false
	}
	if r.ID == 0 {
		// A bare yes with nothing waiting is conversation, unless it would
		// confirm the twin's "Before I stop asking" check (alwaysallow.go):
		// that yes turns on a standing permission, even once the request it
		// came with was decided elsewhere.
		c := d.conv(in.Key())
		c.mu.Lock()
		offered := c.offer != nil && clock().Sub(c.offer.at) <= askFresh
		c.mu.Unlock()
		if offered {
			return cantApproveReply(ctx), true
		}
		if waiting, err := d.store.AllPendingApprovals(ctx); err == nil && len(waiting) == 0 {
			return "", false
		}
		// A task's question the twin asked here last, with no request in
		// the same breath: the yes is that task's answer (decision leaves it
		// to answerWaitingTask), whatever waits elsewhere.
		if d.taskWaiting(in.Key()) != nil && !d.askedLive(ctx, c) {
			return "", false
		}
	}
	return cantApproveReply(ctx), true
}

// askedLive reports whether the twin's latest words in c, still fresh, asked
// about a request that is waiting: what a bare yes there would decide.
func (d *Daemon) askedLive(ctx context.Context, c *conversation) bool {
	asked, at, _ := c.lastAsk()
	if len(asked) == 0 || clock().Sub(at) > askFresh {
		return false
	}
	for _, id := range asked {
		if ap, err := d.store.GetApproval(ctx, id); err == nil && ap.Status == "pending" {
			return true
		}
	}
	return false
}

// cantApproveReply tells a device paired without approve that its yes or no
// decided nothing, and how to answer instead.
func cantApproveReply(ctx context.Context) string {
	if api.ApprovalsPaused(ctx) {
		return alarmPausedReply
	}
	p := api.PeerFrom(ctx)
	again := "mirrin pair --screen --scopes view,chat,approve"
	if p.Device != nil && p.Device.Kind == devices.KindCLI {
		again = "mirrin pair --scopes view,chat,approve"
	}
	return "This device was paired to talk, not to answer requests, so I didn't take that as a yes or no. Answer from a device that may approve, or in your own chat app, or pair this device again with approve: `" + again + "`."
}
