package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/channels/discord"
	"github.com/MavrkAI/Mirrin/internal/channels/imessage"
	"github.com/MavrkAI/Mirrin/internal/channels/irc"
	"github.com/MavrkAI/Mirrin/internal/channels/mail"
	"github.com/MavrkAI/Mirrin/internal/channels/matrix"
	"github.com/MavrkAI/Mirrin/internal/channels/mattermost"
	"github.com/MavrkAI/Mirrin/internal/channels/signal"
	"github.com/MavrkAI/Mirrin/internal/channels/slack"
	"github.com/MavrkAI/Mirrin/internal/channels/telegram"
	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
	"github.com/MavrkAI/Mirrin/internal/channels/zulip"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/skills/email"
)

// buildChannel constructs a messaging channel from config (nil if not enabled).
func (d *Daemon) buildChannel(name string, cfg *config.Config) channels.Channel {
	log := d.log
	switch name {
	case "whatsapp":
		if c := cfg.Channels.WhatsApp; c.Enabled {
			return whatsapp.New(cfg.DataDir, c.Owner, c.ReplyToOthers, log)
		}
	case "telegram":
		if c := cfg.Channels.Telegram; c.Enabled {
			return telegram.New(cfg.TelegramToken(), c.Owner, c.ReplyToOthers, log)
		}
	case "imessage":
		if c := cfg.Channels.IMessage; c.Enabled {
			return imessage.New(c.Owner, c.ReplyToOthers, log)
		}
	case "discord":
		if c := cfg.Channels.Discord; c.Enabled {
			return discord.New(cfg.DiscordToken(), c.Owner, c.ReplyToOthers, c.Channels, log)
		}
	case "slack":
		if c := cfg.Channels.Slack; c.Enabled {
			bot, app := cfg.SlackTokens()
			return slack.New(bot, app, c.Owner, c.ReplyToOthers, log)
		}
	case "signal":
		if c := cfg.Channels.Signal; c.Enabled {
			return signal.New(c.Bin, c.HTTP, c.Account, c.Owner, c.ReplyToOthers, log)
		}
	case "matrix":
		if c := cfg.Channels.Matrix; c.Enabled {
			return matrix.New(c.Homeserver, c.UserID, cfg.MatrixToken(), c.Owner, c.ReplyToOthers, log)
		}
	case "mattermost":
		if c := cfg.Channels.Mattermost; c.Enabled {
			return mattermost.New(c.URL, cfg.MattermostToken(), c.Owner, c.ReplyToOthers, log)
		}
	case "irc":
		if c := cfg.Channels.IRC; c.Enabled {
			return irc.New(c.Server, c.TLS, c.Nick, cfg.IRCPassword(), c.Owner, c.ReplyToOthers, c.Channels, log)
		}
	case "zulip":
		if c := cfg.Channels.Zulip; c.Enabled {
			return zulip.New(c.Site, c.Email, cfg.ZulipKey(), c.Owner, c.ReplyToOthers, log)
		}
	case "mail":
		if c := cfg.Channels.Mail; c.Enabled && cfg.Skills.Email.Enabled {
			// The mailbox as the config it starts with has it, so what the
			// Channels page asks for (skills.email.auth_servers, a new
			// password) applies when it reconnects.
			em := email.New(cfg.Skills.Email, cfg.EmailPassword(), d.location())
			return mail.New(em, c.Owner, c.ReplyToOthers, c.SubjectTag, time.Duration(c.PollSeconds)*time.Second, log)
		}
	}
	return nil
}

// messagingNames are the channels the Channels page manages (not voice/cli).
func messagingNames() []string {
	var out []string
	for _, n := range channels.MessagingOrder {
		if n != "voice" && n != "cli" {
			out = append(out, n)
		}
	}
	return out
}

// Supervision and delivery timing, and the rebuild step; variables so tests
// can stand in.
var (
	restartMin = 2 * time.Second
	restartMax = 5 * time.Minute
	startWait  = 4 * time.Second
	sendRetry  = time.Second
	rebuild    = func(d *Daemon, name string, cfg *config.Config) channels.Channel { return d.buildChannel(name, cfg) }
	// A channel says it is reconnecting only once it has been down this
	// long: a laptop that wakes before its Wi-Fi heals in seconds, and
	// saying so popped up for nothing. Up again for backAfter, it is back.
	reconnectQuiet = 3 * time.Minute
	backAfter      = channels.StableAfter
)

// launch supervises a messaging channel until its context ends. Messages go
// through a per-chat queue, so a long turn never holds up the transport. When
// Start returns early the channel is rebuilt from the current config and
// started again after a pause that grows with each failure: a laptop that
// wakes before its Wi-Fi, a server restart or a dropped socket heal on their
// own. A Fatal error (a rejected token, a missing permission) stops it and is
// shown on the Channels page instead.
func (d *Daemon) launch(name string, ch channels.Channel) {
	if messaging(name) && d.standingBy() {
		d.log.Info("standing by: not starting a messaging channel", "channel", name) // cloudreach.go
		return
	}
	ctx, cancel := context.WithCancel(d.runCtx)
	d.chmu.Lock()
	if d.chanCancel == nil {
		d.chanCancel = map[string]context.CancelFunc{}
		d.chanErr = map[string]string{}
		d.chanSince = map[string]time.Time{}
	}
	if old := d.chanCancel[name]; old != nil {
		old() // started twice at once (two Connect clicks): only the newest runs
	}
	d.channels[name] = ch
	d.chanCancel[name] = cancel
	d.chanSince[name] = time.Now()
	delete(d.chanErr, name)
	d.chmu.Unlock()
	go d.supervise(ctx, cancel, name, ch, channels.Backoff{Min: restartMin, Max: restartMax}, rebuild, reconnectQuiet, backAfter)
	go d.watchFirstChannel(ctx, name, firstHelloEvery, firstHelloRetry) // firstchannel.go: the first one up says hello, once ever
}

func (d *Daemon) supervise(ctx context.Context, cancel context.CancelFunc, name string, ch channels.Channel,
	backoff channels.Backoff, rebuild func(*Daemon, string, *config.Config) channels.Channel, quietFor, backFor time.Duration) {
	defer cancel()
	defer func() { release(ch) }()
	announced := false
	var quiet *time.Timer // says it is reconnecting, if it is still down by then
	defer func() {
		if quiet != nil {
			quiet.Stop()
		}
	}()
	for {
		started := time.Now()
		var back *time.Timer
		if q := quiet; q != nil {
			back = time.AfterFunc(backFor, func() { q.Stop() }) // it came back: nothing to say
		}
		err := ch.Start(ctx, d.handle) // into the conversation's mailbox (conversations.go)
		if back != nil {
			back.Stop()
		}
		if ctx.Err() != nil {
			return // stopped on purpose
		}
		if err == nil {
			err = errors.New("stopped unexpectedly")
		}
		fatal := d.fatal(name, err)
		wasUp := time.Since(started) >= backFor
		d.chmu.Lock()
		if d.channels[name] != ch {
			d.chmu.Unlock()
			return
		}
		delete(d.channels, name)
		d.chanErr[name] = channels.Plain(err)
		if fatal {
			delete(d.chanCancel, name)
		}
		d.chmu.Unlock()
		label := channelLabel(name)
		if fatal {
			d.log.Error("channel stopped", "channel", name, "err", err)
			d.bus.Publish(events.Event{Kind: "notice", Text: label + " stopped: " + channels.Plain(err)})
			if wasUp {
				// It was working a minute ago: say so where the owner will see it.
				msg := fmt.Sprintf("I can't reach you on %s any more: %s.", label, channels.Plain(err))
				if err := d.Send(context.WithoutCancel(ctx), d.ownerChatKey(), msg); err != nil {
					d.log.Warn("tell owner", "err", err)
				}
			}
			return
		}
		d.log.Warn("channel dropped; reconnecting", "channel", name, "err", err)
		if wasUp {
			announced = false
		}
		if !announced {
			if quiet != nil {
				quiet.Stop()
			}
			text := label + " is reconnecting: " + channels.Plain(err)
			quiet = time.AfterFunc(quietFor, func() { d.bus.Publish(events.Event{Kind: "notice", Text: text}) })
			announced = true
		}
		if !backoff.Wait(ctx, started) {
			return
		}
		cfg := d.Config()
		next := rebuild(d, name, &cfg)
		d.chmu.Lock()
		if ctx.Err() != nil || next == nil {
			// Stopped or disabled while waiting.
			if next == nil && ctx.Err() == nil {
				delete(d.chanCancel, name)
			}
			d.chmu.Unlock()
			release(next)
			return
		}
		d.channels[name] = next
		d.chanSince[name] = time.Now()
		delete(d.chanErr, name)
		d.chmu.Unlock()
		release(ch)
		ch = next
	}
}

// release lets go of what a finished channel instance holds (WhatsApp's
// device store), so each reconnect attempt doesn't leave a handle open.
func release(ch channels.Channel) {
	if c, ok := ch.(io.Closer); ok {
		_ = c.Close()
	}
}

// fatal reports whether retrying err can't help, so the channel should stop.
func (d *Daemon) fatal(name string, err error) bool {
	if channels.IsFatal(err) {
		return true
	}
	// WhatsApp's Start can't say so itself: without a linked device there is
	// nothing to reconnect until the owner pairs from the Channels page.
	return name == "whatsapp" && !whatsapp.Paired(d.Config().DataDir)
}

// answeringKey marks a context as the reply to one inbound message, so Send
// knows whom the text is for.
type answeringKey struct{}

// pausedReply is what the twin says to anything while paused: to the
// owner in its form of address (address.go), plainly to anyone else.
func (d *Daemon) pausedReply(owner bool) string {
	address := ""
	if owner {
		address = d.address()
	}
	return withAddress("I'm paused from the menu bar.", address) + " Resume me there and I'll get straight back to it."
}

// pausedText is what the twin says while paused: pausedReply, or, on a
// machine standing by because the twin moved to another one (backup.go),
// where it went and how to bring it back here. Resuming from the menu
// wouldn't: the standby keeps it paused.
func (d *Daemon) pausedText(owner bool) string {
	st, err := backup.LoadState(d.Config().DataDir)
	if err != nil || st.Standby == nil {
		return d.pausedReply(owner)
	}
	return backup.StandbyMessage(st.Standby) + ", so this copy is paused. To bring me back here, run `mirrin backup resume` on this computer."
}

// handleQueued answers one message from a conversation's mailbox (drain),
// with "typing…" showing in the chat meanwhile where the platform has it, a
// holding line if the turn runs long (holding.go), and what came with the
// message opened first (media.go).
func (d *Daemon) handleQueued(ctx context.Context, in channels.Inbound) {
	go pruneMedia(d.Config().DataDir, mediaKeep) // old photos go even when none arrive (media.go)
	ctx = context.WithValue(ctx, answeringKey{}, in)
	if ch, ok := d.channel(in.Channel); ok {
		if t, ok := ch.(channels.Typer); ok {
			defer channels.KeepTyping(ctx, t, in.ChatID)()
		}
	}
	ctx, done := d.holdOn(ctx, in)
	defer done()
	if in.Media != "" {
		var answered bool
		if ctx, in, answered = d.takeMedia(ctx, in); answered {
			return
		}
		if d.spokenStop(ctx, in) { // voicestop.go
			return
		}
	}
	d.answer(ctx, in)
}

// channelLabel is a channel's name as the owner knows it ("Telegram").
func channelLabel(name string) string {
	if k, ok := config.ConnectorByName(name); ok && k.Label != "" {
		return k.Label
	}
	return name
}

// channelStatus is a channel's connection as the owner should see it, and
// whether it is running at all. Channels that don't report their own state
// count as connected while they run. One that has stopped shows why, and one
// waiting to be restarted reads as connecting with the error that stopped it.
func (d *Daemon) channelStatus(name string) (channels.Status, bool) {
	d.chmu.RLock()
	ch, running := d.channels[name]
	errText := d.chanErr[name]
	_, supervised := d.chanCancel[name]
	since := d.chanSince[name]
	d.chmu.RUnlock()
	switch {
	case running:
		if r, ok := ch.(channels.Reporter); ok {
			st := r.Status()
			if st.Since.IsZero() {
				st.Since = since // nothing reported yet: connecting since launch
			}
			return st, true
		}
		return channels.Status{State: channels.Connected}, true
	case supervised:
		return channels.Status{State: channels.Connecting, Err: errText}, false
	case errText != "":
		return channels.Status{State: channels.Failed, Err: errText}, false
	}
	return channels.Status{}, false
}

// channelHealth is channelStatus for the self-check: whether the channel is
// up (or still coming up with nothing wrong yet) and, if not, why.
func (d *Daemon) channelHealth(name string) (bool, string) {
	st, _ := d.channelStatus(name)
	switch {
	case st.State == channels.Connected:
		return true, ""
	case st.State == channels.Connecting && st.Err == "":
		return st.Since.IsZero() || time.Since(st.Since) < time.Minute, "still connecting"
	}
	return false, describeStatus(st)
}

// describeStatus words a channel that isn't connected for the owner.
func describeStatus(st channels.Status) string {
	switch {
	case st.State == channels.Connecting && st.Err != "":
		return "reconnecting: " + st.Err
	case st.State == channels.Connecting:
		return "connecting"
	case st.State == "":
		return "not running"
	}
	return st.Err
}

// StartChannel starts a messaging channel from the current config and waits
// briefly, so a bad token or an unreachable server is reported to the caller
// rather than to the log.
func (d *Daemon) StartChannel(name string) error {
	if d.runCtx == nil {
		return errors.New("daemon not running")
	}
	if messaging(name) && d.standingBy() {
		return errors.New("this copy is standing by because I moved to another machine, so its chat apps stay off")
	}
	d.StopChannel(name)
	d.cmu.RLock()
	ch := d.buildChannel(name, d.cfg)
	d.cmu.RUnlock()
	if ch == nil {
		return fmt.Errorf("%s is not enabled", name)
	}
	d.launch(name, ch)
	deadline := time.After(startWait)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			// Still trying after a while: say what is in the way, if anything.
			if st, running := d.channelStatus(name); running && st.State != channels.Connected && st.Err != "" {
				d.StopChannel(name)
				return errors.New(st.Err)
			}
			return nil
		case <-tick.C:
			st, running := d.channelStatus(name)
			if !running {
				errText := st.Err
				if errText == "" {
					errText = "stopped straight away; check the log"
				}
				d.StopChannel(name)
				return errors.New(errText)
			}
			if st.State == channels.Connected {
				if _, reports := ch.(channels.Reporter); reports {
					return nil
				}
			}
		}
	}
}

// StopChannel cancels a running messaging channel. The cancel happens under
// the lock so a supervisor between attempts can't bring it back.
func (d *Daemon) StopChannel(name string) {
	d.chmu.Lock()
	defer d.chmu.Unlock()
	if cancel := d.chanCancel[name]; cancel != nil {
		cancel()
	}
	delete(d.chanCancel, name)
	delete(d.channels, name)
}

// deliver sends text on ch. When it asks for approval of something on a
// website, the page as it is goes along with it.
func (d *Daemon) deliver(ctx context.Context, ch channels.Channel, chatKey, chatID, text string) error {
	spoke(ctx, chatKey) // no holding line after this (holding.go)
	if t, ok := ch.(channels.Typer); ok {
		channels.StopTyping(t, chatID)
	}
	cfg := d.Config()
	d.store.Audit(ctx, "message.out", chatKey, truncate(text, 300))
	if ch.Name() == "voice" {
		text = d.forVoice(ctx, text) // spoken.go: said as a person would
	}
	if err := ch.Send(ctx, chatID, text); err != nil {
		return err
	}
	// Screenshots show the owner's own browser: they go to the owner's chat only.
	if img, ok := ch.(channels.ImageSender); ok && chatID != "" && chatID == ch.OwnerChatID() {
		paths := screenshotPaths(text, cfg.DataDir)
		if len(paths) == 0 && reApprovalAsk.MatchString(text) {
			// Asking for approval of something on a website: show the page as it is.
			if p := latestBrowserShot(cfg.DataDir, 3*time.Minute); p != "" {
				paths = []string{p}
			}
		}
		for _, p := range paths {
			if err := img.SendImage(ctx, chatID, p, "What "+cfg.Name+" is about to submit"); err != nil {
				d.log.Warn("send image", "err", err)
			}
		}
	}
	return nil
}

// route is Send. Text goes out on its own channel when it can. When that
// channel isn't running, or refuses a message for the owner's own chat, the
// owner is reached on their next channel instead, and failing every one of
// them by a desktop notification. A message for the screen, which has no
// channel, goes to the owner's messaging apps, else stays on the screen
// (toScreen). A reply to anyone else is never handed to the owner. where
// is the conversation the text reached (chatKey itself for a desktop
// notification or the screen), so Notify can record it there.
func (d *Daemon) route(ctx context.Context, chatKey, text string) (where string, err error) {
	name, chatID := channels.SplitKey(chatKey)
	in, replying := ctx.Value(answeringKey{}).(channels.Inbound)
	someoneElse := replying && in.Key() == chatKey && !in.IsOwner
	ch, ok := d.channel(name)
	if !ok {
		if someoneElse {
			d.log.Warn("reply not sent: its channel is down", "chat", chatKey)
			return "", fmt.Errorf("%s is not connected", name)
		}
		if name == "screen" {
			// The screen has no channel of its own: the owner's messaging
			// apps first, then the screen itself (proactive.go).
			if key, ok := d.ownerApps(ctx, chatKey, text, name, false); ok {
				return key, nil
			}
			return d.toScreen(ctx, chatKey, text)
		}
		// The originating channel isn't running (e.g. a reminder set over
		// WhatsApp while in voice mode): reach the owner wherever they are.
		// A reply to the owner's message (its channel is reconnecting) is not
		// read out loud in the room instead, as for a refused one below.
		return d.sendToOwner(ctx, chatKey, text, name, !replying)
	}
	err = d.deliver(ctx, ch, chatKey, chatID, text)
	if err == nil {
		return chatKey, nil
	}
	if someoneElse || chatID != ch.OwnerChatID() {
		return "", err
	}
	// The owner's chat refused it. Try once more, then the owner's other
	// channels, with only what didn't already arrive.
	text = channels.Unsent(err, text)
	if !channels.IsFatal(err) && sleepOrDone(ctx, sendRetry) {
		if err = d.deliver(ctx, ch, chatKey, chatID, text); err == nil {
			return chatKey, nil
		}
		text = channels.Unsent(err, text)
	}
	d.log.Warn("send failed; trying the owner's other channels", "chat", chatKey, "err", err)
	return d.sendToOwner(ctx, chatKey, text, name, false)
}

// sendToOwner delivers text wherever the owner can be reached: each connected
// channel in MessagingOrder except skip, then a desktop notification. The
// voice and terminal channels are included only when local is set, for a
// message whose own channel isn't running at all: a reply that a messaging
// channel refused is not read out loud in the room instead.
func (d *Daemon) sendToOwner(ctx context.Context, chatKey, text, skip string, local bool) (string, error) {
	if key, ok := d.ownerApps(ctx, chatKey, text, skip, local); ok {
		return key, nil
	}
	// Nothing can deliver it: fall back to a desktop notification.
	d.store.Audit(ctx, "message.out.notification", chatKey, truncate(text, 300))
	return chatKey, desktopNotify(d.Config().Name, text)
}

// ownerApps is sendToOwner without the desktop notification: it reports
// which of the owner's channels took text, if one did.
func (d *Daemon) ownerApps(ctx context.Context, chatKey, text, skip string, local bool) (string, bool) {
	for _, name := range channels.MessagingOrder {
		if name == skip || (!local && (name == "voice" || name == "cli")) {
			continue
		}
		ch, ok := d.channel(name)
		if !ok || ch.OwnerChatID() == "" {
			continue
		}
		if up, _ := d.channelHealth(name); !up {
			continue
		}
		to := ch.OwnerChatID()
		key := name + ":" + to
		// What it asks is asked here once it arrives (below); until then a
		// yes here isn't an answer to what this chat was asked before.
		d.conv(homeKey(key)).dropQuestion()
		if err := d.deliver(ctx, ch, key, to, text); err != nil {
			d.log.Warn("send to owner", "channel", name, "err", err)
			continue
		}
		d.fwd.add(key, chatKey)
		// What it asks ("reply yes 3") is now asked here too, so the owner
		// can answer where it reached them (approvals.go).
		d.conv(homeKey(key)).noteSpoke("", text, true)
		return key, true
	}
	return "", false
}

// sleepOrDone pauses for delay, or less if ctx ends first (then it returns false).
func sleepOrDone(ctx context.Context, delay time.Duration) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ConnectorStates implements api.ChannelsBackend.
func (d *Daemon) ConnectorStates(ctx context.Context) []api.ConnectorState {
	cfg := d.Config()
	var out []api.ConnectorState
	for _, k := range cfg.Connectors() {
		d.chmu.RLock()
		ch, running := d.channels[k.Name]
		d.chmu.RUnlock()
		cs, _ := d.channelStatus(k.Name)
		connected := running && cs.State == channels.Connected
		st := api.ConnectorState{Connector: k, Running: connected, Since: cs.Since}
		if !connected && cs.Err != "" {
			st.Error = describeStatus(cs) // with no error yet the page says "starting…"
		}
		if !connected && cs.State == channels.Connecting && cs.Err != "" {
			st.State = "reconnecting" // trying again, as a Reporter (WhatsApp) or the supervisor says
		}
		if connected {
			st.OwnerChat = ch.OwnerChatID()
			if l, ok := ch.(channels.Linker); ok {
				if label, u := l.ChatLink(); u != "" {
					st.Links = append(st.Links, api.Link{Label: label, URL: u})
				}
			}
		}
		for i := range st.Steps {
			if st.Steps[i].URL == "SLACK_MANIFEST" {
				st.Steps[i].URL = slack.ManifestURL(cfg.Name)
			}
		}
		switch k.Name {
		case "discord":
			if u := discord.InviteURL(cfg.DiscordToken()); u != "" {
				st.Links = append(st.Links, api.Link{Label: "Add the bot to your server", URL: u})
			}
		case "signal":
			if _, err := exec.LookPath(cfg.Channels.Signal.Bin); err != nil {
				st.Notice = "signal-cli isn't installed yet. On a Mac, type brew install signal-cli in Terminal; otherwise follow the install guide below."
			}
		case "mail":
			if !cfg.Skills.Email.Enabled {
				st.Notice = "Set up the twin's mailbox first: in the settings file, fill in skills.email (mail server, username and app password). This channel answers from that mailbox."
			}
		case "whatsapp":
			st.Paired = whatsapp.Paired(cfg.DataDir)
			if p := d.WhatsAppPairing(); p != nil {
				st.Pairing = p
			}
		}
		if w, ok := ch.(channels.Warner); ok && st.Notice == "" {
			st.Notice = w.Warning()
		}
		out = append(out, st)
	}
	return out
}

// ConnectChannel saves the submitted fields, enables the channel and starts
// it. If it fails to connect, the fields are kept but the channel is left
// disabled so the next restart is clean.
func (d *Daemon) ConnectChannel(ctx context.Context, name string, fields map[string]string) error {
	k, ok := config.ConnectorByName(name)
	if !ok {
		return fmt.Errorf("unknown channel %q", name)
	}
	if name == "matrix" {
		if pw := strings.TrimSpace(fields["password"]); pw != "" {
			hs, user := strings.TrimSpace(fields["homeserver"]), strings.TrimSpace(fields["user_id"])
			if hs == "" {
				hs = d.Config().Channels.Matrix.Homeserver
			}
			tok, uid, err := matrix.Login(ctx, hs, user, pw)
			if err != nil {
				return err
			}
			fields["access_token"] = tok
			if uid != "" {
				fields["user_id"] = uid
			}
			delete(fields, "password")
		}
	}
	var missing []string
	if err := d.UpdateConfig(func(c *config.Config) {
		k.Apply(c, fields, true)
		missing = k.MissingRequired(c)
	}); err != nil {
		return err
	}
	if len(missing) > 0 {
		_ = d.UpdateConfig(func(c *config.Config) { k.Apply(c, nil, false) })
		return fmt.Errorf("missing: %s", strings.Join(missing, ", "))
	}
	if err := d.StartChannel(name); err != nil {
		_ = d.UpdateConfig(func(c *config.Config) { k.Apply(c, nil, false) })
		return err
	}
	d.store.Audit(context.Background(), "channel.connected", "", name)
	return nil
}

// DisconnectChannel stops a channel and disables it in config.
func (d *Daemon) DisconnectChannel(ctx context.Context, name string) error {
	k, ok := config.ConnectorByName(name)
	if !ok {
		return fmt.Errorf("unknown channel %q", name)
	}
	d.StopChannel(name)
	if err := d.UpdateConfig(func(c *config.Config) { k.Apply(c, nil, false) }); err != nil {
		return err
	}
	d.store.Audit(context.Background(), "channel.disconnected", "", name)
	return nil
}

// PairWhatsApp starts linking this machine to the owner's WhatsApp. byPhone
// asks for a code to type instead of a QR to scan. When the phone accepts,
// the channel is enabled, started, and the twin says hello in the owner's chat.
func (d *Daemon) PairWhatsApp(ctx context.Context, byPhone bool) error {
	if d.runCtx == nil {
		return errors.New("daemon not running")
	}
	cfg := d.Config()
	if cfg.Channels.WhatsApp.Owner == "" {
		return errors.New("enter your number first")
	}
	d.chmu.Lock()
	if d.waPair != nil {
		select {
		case <-d.waPair.Done():
		default:
			d.chmu.Unlock()
			return nil // already in progress; the page just polls
		}
	}
	d.chmu.Unlock()
	d.StopChannel("whatsapp")
	ch := whatsapp.New(cfg.DataDir, cfg.Channels.WhatsApp.Owner, cfg.Channels.WhatsApp.ReplyToOthers, d.log)
	phone := ""
	if byPhone {
		phone = cfg.Channels.WhatsApp.Owner
	}
	p, err := ch.StartPairing(d.runCtx, phone, cfg.Name)
	if err != nil {
		_ = ch.Close()
		if strings.HasPrefix(err.Error(), "already paired") {
			return d.StartChannel("whatsapp")
		}
		return err
	}
	d.chmu.Lock()
	d.waPair = p
	d.chmu.Unlock()
	go func() {
		<-p.Done()
		if !p.Paired() {
			return
		}
		if err := d.UpdateConfig(func(c *config.Config) { c.Channels.WhatsApp.Enabled = true }); err != nil {
			d.log.Warn("whatsapp: save after pairing", "err", err)
		}
		if err := d.StartChannel("whatsapp"); err != nil {
			d.log.Warn("whatsapp: start after pairing", "err", err)
			return
		}
		d.store.Audit(context.Background(), "channel.connected", "", "whatsapp")
		time.Sleep(2 * time.Second)
		if d.introduce(context.Background(), "whatsapp") {
			return // its first words there are hello enough (firstchannel.go)
		}
		if err := d.TestChannel(context.Background(), "whatsapp"); err != nil {
			d.log.Warn("whatsapp: hello", "err", err)
		}
	}()
	return nil
}

// WhatsAppPairing is the current pairing state for the page (nil if none).
func (d *Daemon) WhatsAppPairing() *whatsapp.Snapshot {
	d.chmu.RLock()
	p := d.waPair
	d.chmu.RUnlock()
	if p == nil {
		return nil
	}
	s := p.Snapshot()
	return &s
}

// UnpairWhatsApp forgets the linked device and disables the channel.
func (d *Daemon) UnpairWhatsApp(ctx context.Context) error {
	d.chmu.Lock()
	if d.waPair != nil {
		d.waPair.Stop()
		d.waPair = nil
	}
	d.chmu.Unlock()
	d.StopChannel("whatsapp")
	cfg := d.Config()
	ch := whatsapp.New(cfg.DataDir, cfg.Channels.WhatsApp.Owner, false, d.log)
	defer ch.Close()
	if err := ch.Unpair(ctx); err != nil {
		return err
	}
	return d.UpdateConfig(func(c *config.Config) { c.Channels.WhatsApp.Enabled = false })
}

// TestChannel has the twin send a short hello to the owner on that channel.
func (d *Daemon) TestChannel(ctx context.Context, name string) error {
	ch, ok := d.channel(name)
	if !ok {
		return fmt.Errorf("%s is not connected", name)
	}
	to := ch.OwnerChatID()
	if to == "" {
		return errors.New("I don't know your chat yet; send me a message there first")
	}
	cfg := d.Config()
	text := fmt.Sprintf("Hi, it's %s. This channel works; talk to me here whenever you like.", cfg.Name)
	if err := ch.Send(ctx, to, text); err != nil {
		return err
	}
	d.store.Audit(context.Background(), "channel.test", "", name)
	return nil
}

// ConnectFields saves a connector's fields without enabling or starting it.
func (d *Daemon) ConnectFields(ctx context.Context, name string, fields map[string]string) error {
	k, ok := config.ConnectorByName(name)
	if !ok {
		return fmt.Errorf("unknown channel %q", name)
	}
	return d.UpdateConfig(func(c *config.Config) {
		enabled := *connectorEnabled(c, k)
		k.Apply(c, fields, enabled)
	})
}

func connectorEnabled(c *config.Config, k config.Connector) *bool {
	for _, kk := range c.Connectors() {
		if kk.Name == k.Name {
			b := kk.Enabled
			return &b
		}
	}
	b := false
	return &b
}
