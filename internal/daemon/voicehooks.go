package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// What the daemon does for the voice channel: tells it whether an approval
// is waiting (so a bare "yes" in the follow-up window is kept), hears about
// trouble it reports, gives the self-checks their voice rows, picks up
// `mirrin voice setup` without a restart, and keeps a voice in the room
// from approving a dangerous action.

// voiceChat is the voice channel's conversation.
const voiceChat = "voice:local"

// wireVoice gives a voice channel what it needs from the daemon.
func (d *Daemon) wireVoice(ch *voice.Channel) {
	// The event hook keeps only this small counter, not the channel, so a
	// replaced microphone pipeline isn't retained by the agent's hooks.
	var revision atomic.Uint64
	d.agent.OnApproval(func(_ context.Context, e agent.ApprovalEvent) {
		if e.ChatKey == voiceChat {
			revision.Add(1)
		}
	})
	ch.ApprovalRevision = revision.Load
	ch.ContextWords = d.wordsInPlay
	ch.Pending = func() bool { return len(d.pendingFor(context.Background(), voiceChat)) > 0 }
	ch.ApprovalPrompt = func() (int64, string) {
		var newest memory.Approval
		for _, ap := range d.pendingFor(context.Background(), voiceChat) {
			if ap.ChatKey == voiceChat && ap.ID > newest.ID && d.dangerous(context.Background(), ap) {
				newest = ap
			}
		}
		if newest.ID == 0 {
			return 0, ""
		}
		return newest.ID, voice.AskByHand(newest.ID, label(newest))
	}
	ch.Screen = d.screenAnswers() // showanswer.go
	ch.OnProblem = d.voiceProblem
	ch.OnInterrupt = d.stopVoiceTurn
	// Compatibility for callers of the original hook; normal voice input
	// uses OnInterrupt so its words and the stop summary are preserved.
	ch.OnStop = func() { d.stopVoiceTurn("") }
}

// stopVoiceTurn cuts voice work first. A barge-in or a soft stop never
// cancels unrelated work; only a whole, firm stop may reach another chat.
func (d *Daemon) stopVoiceTurn(text string) func() string {
	ctx := context.Background()
	c := d.conv(voiceChat)
	c.mu.Lock()
	asked := append([]int64(nil), c.raised[voiceChat]...)
	c.mu.Unlock()
	cut := c.stop(false, false)
	if len(cut) == 0 {
		stop, whole, firm := d.isStop(text)
		if !stop || !whole || !firm {
			return nil
		}
		cut = d.stopFrom(ctx, voiceChat, true)
	} else {
		d.store.Audit(ctx, "turn.stopped", voiceChat, truncate(strings.Join(labels(cut), "; "), 300))
	}
	if len(cut) == 0 {
		return nil
	}
	notice := "OK, I've stopped work on " + truncate(strings.Join(labels(cut), "; "), 160) + "."
	// The voice channel calls this after the cancelled handler has finished,
	// when its pending requests have been withdrawn. Query stored outcomes
	// rather than reading a running turn's mutable withdrawal count.
	return func() string {
		dropped := 0
		for _, id := range asked {
			if ap, err := d.store.GetApproval(ctx, id); err == nil && ap != nil && ap.Status == "denied" {
				dropped++
			}
		}
		switch dropped {
		case 0:
			return notice
		case 1:
			return notice + " I've dropped the request I asked you about, too."
		default:
			return notice + " I've dropped the requests I asked you about, too."
		}
	}
}

// voiceProblem tells the owner that part of the voice stopped working, on
// the presence screen and as a desktop notification. The channel reports
// each kind of trouble once, until it recovers.
func (d *Daemon) voiceProblem(msg string) {
	d.log.Warn("voice trouble", "detail", msg)
	d.store.Audit(context.Background(), "voice.problem", voiceChat, msg)
	d.bus.Publish(events.Event{Kind: "notice", Text: msg})
	_ = desktopNotify(d.Config().Name, msg)
}

// voiceChecks are the voice rows of the self-checks, for push-to-talk and
// always-on alike.
func (d *Daemon) voiceChecks() []health.Check {
	return voice.Checks(voice.Probe{
		Config:    func() config.Voice { return d.Config().Channels.Voice },
		Listening: d.Listening,
		Start:     d.StartVoice,
	})
}

// ReloadVoice picks up what `mirrin voice setup` saved: the settings on
// disk and, when the twin listens all the time, a microphone pipeline
// rebuilt from them. Nothing else restarts.
func (d *Daemon) ReloadVoice() error {
	d.chmu.RLock()
	always := d.voiceCancel != nil
	d.chmu.RUnlock()
	was := d.Config().Channels.Voice
	if err := d.UpdateConfig(func(*config.Config) {}); err != nil {
		return err // it starts listening when that's wanted and wasn't running
	}
	if !always || !reflect.DeepEqual(was, d.Config().Channels.Voice) {
		return nil // changed settings were switched to by UpdateConfig (voiceswitch.go)
	}
	// The same settings, with new files behind them: start again anyway.
	d.StopVoice()
	if v := d.Config().Channels.Voice; v.Enabled && v.Mode == "wake" {
		return d.StartVoice()
	}
	return nil
}

// refuseAloud keeps a voice in the room from approving a dangerous action:
// anyone nearby can say "yes". The request goes to the owner's own chat
// (it stays on the presence screen too) with its number, and the twin says
// where. A no, and a yes to anything less, is taken as usual.
func (d *Daemon) refuseAloud(ctx context.Context, in channels.Inbound, ap memory.Approval, approve bool) (string, bool) {
	if reply, ok := d.refuseRemoteYes(ctx, ap, approve); ok { // stepup.go: another device needs a passkey
		return reply, true
	}
	if !approve || !spoken(in) || ap.Status != "pending" || !d.dangerous(ctx, ap) {
		return "", false
	}
	d.store.Audit(ctx, "approval.refused_aloud", in.Key(), fmt.Sprintf("#%d %s", ap.ID, ap.Tool))
	aloudMu.Lock()
	defer aloudMu.Unlock()
	sent := d.aloudSent(ctx)
	sentTo, told := sent[ap.ID]
	if !told {
		// Only a request that reached the owner's chat counts as told: with
		// the phone down for a moment (or none at all), the next spoken yes
		// tries again, and meanwhile the presence screen has it.
		if where, err := d.sendToOwner(ctx, ap.ChatKey, voice.ApproveByHand(ap.ID, label(ap)), "voice", false); err == nil && where != ap.ChatKey {
			sentTo = channelLabel(channelOf(where))
			sent[ap.ID] = sentTo
		}
		d.saveAloudSent(ctx, sent)
	}
	return voice.RefusedAloud(ap.ID, sentTo), true
}

// aloudKey keeps, for each dangerous request refused out loud, the chat it
// reached, so the owner's phone hears about it once.
const aloudKey = "approval.aloud"

var aloudMu sync.Mutex

// aloudSent reads what aloudKey keeps, without the requests decided since.
func (d *Daemon) aloudSent(ctx context.Context) map[int64]string {
	sent := map[int64]string{}
	if raw, err := d.store.Get(ctx, aloudKey); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &sent)
	}
	for id := range sent {
		if ap, err := d.store.GetApproval(ctx, id); err != nil || ap == nil || ap.Status != "pending" {
			delete(sent, id)
		}
	}
	return sent
}

func (d *Daemon) saveAloudSent(ctx context.Context, sent map[int64]string) {
	b, _ := json.Marshal(sent)
	_ = d.store.Set(ctx, aloudKey, string(b))
}

// dangerous reports whether an approval's call is a dangerous one.
func (d *Daemon) dangerous(ctx context.Context, ap memory.Approval) bool {
	if ap.Risk >= tools.RiskDangerous {
		return true // as it was asked (approvals keep the risk; older ones count as dangerous)
	}
	t, ok := d.agent.Tools().Get(ap.Tool)
	if !ok {
		return true // not a tool any more: don't take it on a voice's word
	}
	risk := t.Risk()
	if cr, ok := t.(tools.CallRisker); ok {
		risk = cr.RiskFor(ctx, tools.Call{ChatKey: ap.ChatKey, Input: ap.Input})
	}
	return risk >= tools.RiskDangerous
}

// wordsInPlay are the names the owner may say now: open tasks' titles and
// the site on screen ("uniqlo.com" → "Uniqlo"), for the voice to hear right.
func (d *Daemon) wordsInPlay() []string {
	var out []string
	if d.tasks != nil {
		for _, t := range d.tasks.List() {
			if t.Open() && len(out) < 4 {
				out = append(out, t.Title)
			}
		}
	}
	if d.pageURL != nil {
		if site := siteName(d.pageURL()); site != "" {
			out = append(out, site)
		}
	}
	return out
}

// siteName is a page's site as people say it: "https://www.uniqlo.com/au/"
// is "Uniqlo".
func siteName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(u.Hostname(), "www."), ".")
	name := parts[0]
	if name == "" || name == "localhost" || net.ParseIP(u.Hostname()) != nil {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
