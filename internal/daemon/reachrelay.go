package daemon

import (
	"context"
	"sync"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/reach"
)

// relayAlarms holds each running twin's certificate alarm (relay mode only).
var relayAlarms sync.Map // *Daemon → *reach.Alarm

// startReach starts remote access: relay mode here (the self-hosted relay,
// its certificate, CT/CAA watch and the alarm playbook), every other mode
// in reach.Start.
func (d *Daemon) startReach(ctx context.Context, srv *api.Server) {
	c := d.Config()
	srv.WithStatusURLs(d.statusURLs) // cloudreach.go: the relays' status pages, for phones
	if d.phone != nil && d.phone.PublicURL == nil {
		// Twilio calls back at the address reach serves while
		// phone.public_url is empty.
		d.phone.PublicURL = func() string { return d.relayEndpoint().PublicURL() }
	}
	if c.Reach.Mode == "cloud" {
		d.startCloudReach(ctx, srv) // cloudreach.go
		return
	}
	if c.Reach.Mode != "relay" {
		d.noteFreeReach(ctx, c.Reach.Mode) // cloudreach.go
		reach.Start(ctx, srv, c.Reach, c.API.Remote, d.health)
		return
	}
	deps := reach.Deps{
		Server:  srv,
		DataDir: c.DataDir,
		Health:  d.health,
		Devices: d.deviceStore,
		Notify: func(ctx context.Context, text string) error {
			if owner := d.ownerChatKey(); owner != "" {
				return d.Notify(ctx, owner, text)
			}
			return desktopNotify(d.Config().Name, text)
		},
		Push:   d.PushSecurity,
		Banner: func(text string) { d.bus.Publish(events.Event{Kind: "notice", Text: text}) },
		Audit: func(kind, detail string) {
			d.store.Audit(context.Background(), kind, "", detail)
		},
		Log: d.log,
	}
	e, err := reach.StartRelay(ctx, c.Reach, deps)
	if err != nil {
		d.log.Warn("relay reach", "err", err)
		if d.health != nil {
			msg := err.Error()
			d.health.Add(health.Func("reach", "Remote access", func(context.Context) (health.State, string, string) {
				return health.Fail, msg, "Check reach.relay_url and reach.hostname, or run `mirrin reach use relay` again"
			}, nil))
		}
		return
	}
	relayAlarms.Store(d, e.Alarm)
	relayEndpoints.Store(d, e) // pages.go: routes and the Trust page
	context.AfterFunc(ctx, func() { relayAlarms.CompareAndDelete(d, e.Alarm); relayEndpoints.CompareAndDelete(d, e) })
}

// alarmCommand is /alarm and /alarm clear from the owner's own chat.
func (d *Daemon) alarmCommand(ctx context.Context, key string, fields []string) (string, error) {
	if forgeable(channelOf(key)) {
		return "I don't take /alarm by email or IRC, where anyone can claim to be you. Send it from your own chat app or `mirrin chat`, or run `mirrin reach alarm clear` on the computer I run on.", nil
	}
	// Only this computer or the owner's own chat may lift the pause
	// (§6.5): a device on a remote listener may hold a key the impostor
	// took, even one with the admin scope.
	if p := api.PeerFrom(ctx); (p.Device != nil || p.Master) && !p.Loopback {
		return "I don't take /alarm from another device while I can't be sure who holds its key. Clear it on the computer I run on with `mirrin reach alarm clear`, or send /alarm clear from your own chat app.", nil
	}
	if api.PeerFrom(ctx).Lacks(devices.Admin) {
		return "The certificate alarm is cleared on the computer I run on (`mirrin reach alarm clear`) or from your own chat app.", nil
	}
	v, ok := relayAlarms.Load(d)
	if !ok {
		return "There's no certificate alarm: I'm not reachable through a relay.", nil
	}
	a := v.(*reach.Alarm)
	st := a.State()
	if len(fields) < 2 || fields[1] != "clear" {
		if !st.Active {
			return "No certificate alarm. Approvals from other devices work as usual.", nil
		}
		return "Certificate alarm: " + st.Detail + " Approvals from other devices are paused. Send /alarm clear when you've checked.", nil
	}
	if !st.Active {
		return "There's no alarm to clear.", nil
	}
	if err := a.Clear(key); err != nil {
		return "", err
	}
	d.store.Audit(ctx, "reach.alarm.cleared", key, st.Detail)
	return "Alarm cleared. Approvals from other devices work again. Devices I signed out need pairing again with `mirrin pair`.", nil
}

// While the certificate alarm is active, a decision in words from another
// device (api.ApprovalsPaused) is refused as the approval routes' 423 is:
// whoever holds the rogue certificate may be holding that device's key too.
const (
	alarmPausedReply = "I've paused approvals from other devices: a certificate for my address turned up that I didn't ask for, so I didn't take that as a yes or no. Approve on the computer I run on, or in your own chat app. When you've checked, clear the alarm there with `mirrin reach alarm clear`."
	alarmPausedTool  = "approvals from other devices are paused (a certificate for the twin's address turned up that it didn't ask for), so words sent from this device can't settle a request; the user can approve on the computer the twin runs on or in their own chat app, and clear the alarm there with `mirrin reach alarm clear`"
)
