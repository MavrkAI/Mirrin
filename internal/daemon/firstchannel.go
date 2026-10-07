package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// The first message on a new channel. The first time the owner connects a
// phone messaging app (WhatsApp, Telegram, iMessage or Signal) and the twin
// can see their chat, it says who it is there: once ever, to the owner's
// own chat only. Mail and the desktop chats (Slack, Discord and the rest)
// aren't the owner's phone, so they get no hello and don't count as the
// first. A twin that starts with a phone app already set up (an upgrade)
// has been reaching the owner there all along, and says nothing new.

// firstHelloKey is the kv entry saying the twin has introduced itself on a
// channel: "<channel> <when>", or "already" for an upgrade.
const firstHelloKey = "first_channel_hello"

// Timing; variables so tests can stand in. A new channel is checked every
// firstHelloEvery for being up with the owner's chat known. A hello that
// couldn't be sent (a Telegram bot the owner hasn't started yet) is tried
// again after firstHelloRetry, then less and less often, up to hourly.
var (
	firstHelloEvery = time.Second
	firstHelloRetry = time.Minute
)

// justIntroduced is how long after the first hello on a channel it counts
// as just said there (introduce).
const justIntroduced = time.Minute

// firstHelloMu keeps two channels coming up at once from both saying it.
var firstHelloMu sync.Mutex

// phoneChannels are the messaging channels on the owner's phone, where
// the first hello's "on your phone now" is true.
var phoneChannels = []string{"whatsapp", "telegram", "imessage", "signal"}

// onPhone reports whether the named channel reaches the owner's phone.
func onPhone(name string) bool { return slices.Contains(phoneChannels, name) }

// firstHelloText is what the twin says on its first phone channel.
func firstHelloText(name string) string {
	return fmt.Sprintf("This is %s, on your phone now. Message me here any time; only you can give me instructions here.", name)
}

// watchFirstChannel runs alongside the supervisor of a messaging channel,
// until ctx ends. The first time a phone channel is connected with the
// owner's chat known, the twin introduces itself there, unless it has on
// some channel before. Nothing is said while the twin is paused.
func (d *Daemon) watchFirstChannel(ctx context.Context, name string, every, retryAfter time.Duration) {
	if d.store == nil || !onPhone(name) || d.introduced(ctx) {
		return
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	retry := channels.Backoff{Min: retryAfter, Max: time.Hour}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if d.paused.Load() {
			continue
		}
		ch, ok := d.channel(name)
		if !ok || ch.OwnerChatID() == "" {
			continue
		}
		if st, _ := d.channelStatus(name); st.State != channels.Connected {
			continue
		}
		if d.introduce(ctx, name) || d.introduced(ctx) {
			return
		}
		if !retry.Wait(ctx, time.Time{}) {
			return
		}
	}
}

// introduced reports whether the twin has introduced itself on a channel.
// A store it can't read counts as yes: better silent than said twice.
func (d *Daemon) introduced(ctx context.Context) bool {
	said, err := d.store.Get(ctx, firstHelloKey)
	return err != nil || said != ""
}

// introduce has the twin introduce itself in the owner's chat on the named
// channel, unless it has on some channel before. It reports whether that
// happened there just now (within justIntroduced), so another hello on the
// same channel (WhatsApp's after pairing) needn't follow it.
func (d *Daemon) introduce(ctx context.Context, name string) bool {
	firstHelloMu.Lock()
	defer firstHelloMu.Unlock()
	said, err := d.store.Get(ctx, firstHelloKey)
	if err != nil {
		return false
	}
	if said != "" {
		on, at, _ := strings.Cut(said, " ")
		when, err := time.Parse(time.RFC3339, at)
		return on == name && err == nil && time.Since(when) < justIntroduced
	}
	ch, ok := d.channel(name)
	if !ok {
		return false
	}
	to := ch.OwnerChatID()
	if to == "" {
		return false
	}
	text := firstHelloText(d.Config().Name)
	if err := ch.Send(ctx, to, text); err != nil {
		d.log.Info("first hello not sent yet", "channel", name, "err", err)
		return false
	}
	// It was said: record it even if the channel is stopping right now, or
	// the channel reconnected says it all over again.
	ctx = context.WithoutCancel(ctx)
	d.store.Audit(ctx, "message.out", name+":"+to, text)
	if err := d.store.Set(ctx, firstHelloKey, name+" "+time.Now().UTC().Format(time.RFC3339)); err != nil {
		d.log.Warn("first hello: save", "err", err)
	}
	return true
}

// introducedBefore marks a twin that starts with a phone channel set up
// as introduced: it has been reaching the owner there since before the
// first hello existed (an upgrade), so it says nothing new.
func (d *Daemon) introducedBefore(ctx context.Context, startup []channels.Channel) {
	if d.store == nil || !slices.ContainsFunc(startup, func(ch channels.Channel) bool { return onPhone(ch.Name()) }) {
		return
	}
	firstHelloMu.Lock()
	defer firstHelloMu.Unlock()
	if said, err := d.store.Get(ctx, firstHelloKey); err == nil && said == "" {
		if err := d.store.Set(ctx, firstHelloKey, "already"); err != nil {
			d.log.Warn("first hello: save", "err", err)
		}
	}
}
