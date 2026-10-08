package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/protocols"
	"github.com/MavrkAI/Mirrin/internal/tools"
	"gopkg.in/yaml.v3"
)

// The first hello's offer: "Want me to brief you at seven tomorrow?" It is
// asked once, after the hello, as a request on the screen's conversation:
// the welcome page shows it as a Yes/Later card with a time to choose, the
// presence screen as a card, and a bare "yes" typed on the screen answers
// it. Nothing is installed without that yes. Once it is accepted, the first
// week's day-one tip about briefings is skipped (firstweek.go).

// Keys in the key-value store.
const (
	briefingOfferKey = "welcome.briefing"    // "" never asked; "offered"; "yes"; "later"
	briefingOfferID  = "welcome.briefing.id" // the request the offer raised
)

const (
	briefingName  = "morning briefing"
	briefingFile  = "morning-briefing.yaml" // the bundled one, protocols.Examples
	briefingTool  = "start_briefing"
	briefingAsk   = "Want me to brief you at seven tomorrow?"
	briefingAskAM = "Want me to brief you at seven, starting this morning?" // said before seven
	briefingAt    = "07:00"
	briefingLater = "No problem. Ask me for a morning briefing whenever you like."
)

// ownBriefing reports whether the owner already has a morning briefing of
// their own that runs: enabled, on a schedule, and not the bundled example
// exactly as it was shipped. Then there is nothing to offer.
func (d *Daemon) ownBriefing() bool {
	p, ok := protocols.Find(d.Protocols(), briefingName)
	if !ok || !p.IsEnabled() || strings.TrimSpace(p.Schedule) == "" {
		return false
	}
	if p.Pack != "" {
		return true
	}
	b, err := os.ReadFile(p.Source)
	return err != nil || string(b) != protocols.Examples[briefingFile]
}

// offeringBriefing reports whether the first hello will end with the
// offer, so the hello itself doesn't promise a briefing first.
func (d *Daemon) offeringBriefing(ctx context.Context) bool {
	switch state, _ := d.store.Get(ctx, briefingOfferKey); state {
	case "":
		return !d.ownBriefing()
	case "offered":
		_, ok := d.pendingBriefing(ctx)
		return ok
	}
	return false
}

// pendingBriefing is the request the offer raised, while it still waits.
func (d *Daemon) pendingBriefing(ctx context.Context) (int64, bool) {
	v, _ := d.store.Get(ctx, briefingOfferID)
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	ap, err := d.store.GetApproval(ctx, id)
	if err != nil || ap.Status != "pending" || d.lapsed(*ap) {
		return 0, false
	}
	return id, true
}

// offerBriefing ends the first hello with the offer, once: it raises the
// request on the screen's conversation and says the line there, so a bare
// "yes" typed on the screen answers it. A hello said again offers the same
// request again. nil when there is nothing to offer.
func (d *Daemon) offerBriefing(ctx context.Context) *api.BriefingOffer {
	if !d.offeringBriefing(ctx) {
		return nil
	}
	id, ok := d.pendingBriefing(ctx)
	if !ok {
		input, _ := json.Marshal(map[string]string{"time": briefingAt})
		var err error
		id, err = d.store.CreateApproval(ctx, screenChat, briefingTool, input, "brief you every morning at 7:00", tools.RiskWrite)
		if err != nil {
			d.log.Warn("briefing offer", "err", err)
			return nil
		}
		if ap, err := d.store.GetApproval(ctx, id); err == nil {
			d.approvalRaised(*ap)
		}
		_ = d.store.Set(ctx, briefingOfferID, strconv.FormatInt(id, 10))
		_ = d.store.Set(ctx, briefingOfferKey, "offered")
	}
	ask := briefingAsk
	if clock().In(d.location()).Hour() < 7 {
		ask = briefingAskAM // the first one is today
	}
	d.noticed(ctx, screenChat, ask, ask)
	d.bus.Publish(events.Event{Kind: "said", Text: ask, Data: map[string]string{"channel": "screen"}})
	// The line asks about this request, as the daemon's own words do, so a
	// bare yes there decides it.
	c := d.conv(screenChat)
	c.mu.Lock()
	c.asked, c.askedAt, c.vouched, c.askedLean = []int64{id}, clock(), true, 0
	c.mu.Unlock()
	return &api.BriefingOffer{ID: id, Text: ask, Time: briefingAt}
}

// AnswerBriefing is the welcome card's answer to the offer: yes at the
// time chosen ("07:30"), or later. Later installs nothing and isn't asked
// again. It returns what the card says next.
func (d *Daemon) AnswerBriefing(ctx context.Context, yes bool, at string) (string, error) {
	id, pending := d.pendingBriefing(ctx)
	if !yes {
		if pending {
			_, _ = d.agent.DecideBy(ctx, screenChat, id, false, "the welcome page")
		}
		d.briefingTurnedDown(ctx) // also when the request had already gone
		return briefingLater, nil
	}
	out, err := d.startBriefing(ctx, at)
	if err != nil {
		return "", err
	}
	if pending {
		if ap, err := d.store.GetApproval(ctx, id); err == nil {
			_ = d.agent.Settle(ctx, ap, "superseded", "answered on the welcome page")
		}
	}
	return out, nil
}

var _ api.WelcomeBriefing = (*Daemon)(nil)

// briefingSettled hears every request as it is settled (approvalEvent): the
// offer turned down anywhere (Later on the welcome card, no on the presence
// screen's card, a bare "no" typed there) or left to lapse is a later.
func (d *Daemon) briefingSettled(e agent.ApprovalEvent) {
	if e.Tool == briefingTool && (e.Status == "denied" || e.Status == "expired") {
		d.briefingTurnedDown(context.Background())
	}
}

// briefingTurnedDown is the owner's later: the offer isn't made again, and
// the morning briefing a fresh install ships turned on is parked, so none
// comes at seven after all. A briefing the owner has changed in any way is
// theirs and stays as it is.
func (d *Daemon) briefingTurnedDown(ctx context.Context) {
	if state, _ := d.store.Get(ctx, briefingOfferKey); state == "yes" {
		return
	}
	_ = d.store.Set(ctx, briefingOfferKey, "later")
	p, ok := protocols.Find(d.Protocols(), briefingName)
	if !ok || p.Pack != "" || !p.IsEnabled() {
		return
	}
	if b, err := os.ReadFile(p.Source); err != nil || string(b) != protocols.Examples[briefingFile] {
		return
	}
	off := false
	if _, err := protocols.Edit(d.Config().ProtocolsDir, p, nil, &off); err != nil {
		d.log.Warn("briefing: park the bundled one", "err", err)
		return
	}
	_ = d.ReloadProtocols()
}

// briefingTools is start_briefing: what the owner's yes to the offer
// carries out. The model is never offered it; only the request the offer
// raises calls it.
func (d *Daemon) briefingTools() []tools.Tool {
	return []tools.Tool{tools.New(briefingTool,
		"Start the morning briefing at the time the owner chose, after they said yes to the first hello's offer. Only that offer calls it.",
		tools.Schema(map[string]tools.Prop{
			"time": {Type: "string", Description: "when, as HH:MM on a 24-hour clock", Required: true},
		}), tools.RiskWrite,
		func(ctx context.Context, call tools.Call) (string, error) {
			var in struct{ Time string }
			if err := tools.Decode(call, &in); err != nil {
				return "", err
			}
			return d.startBriefing(ctx, in.Time)
		}).Hide()}
}

// startBriefing installs the morning briefing to run every day at at
// ("07:00"): the owner's own, rescheduled and turned on, or else the
// bundled one. It says when the first one comes.
func (d *Daemon) startBriefing(ctx context.Context, at string) (string, error) {
	hh, mm, ok := parseClock(at)
	if !ok {
		return "", &api.HumanError{Sentence: "That time doesn't look right.", Fix: "Pick a time like 7:00."}
	}
	schedule, on := fmt.Sprintf("%d %d * * *", mm, hh), true
	dir := d.Config().ProtocolsDir
	if p, ok := protocols.Find(d.Protocols(), briefingName); ok {
		if _, err := protocols.Edit(dir, p, &schedule, &on); err != nil {
			return "", err
		}
	} else {
		var p protocols.Protocol
		if err := yaml.Unmarshal([]byte(protocols.Examples[briefingFile]), &p); err != nil {
			return "", err
		}
		p.Schedule = schedule
		if _, err := protocols.Write(dir, p); err != nil {
			return "", err
		}
	}
	if err := d.ReloadProtocols(); err != nil {
		return "", err
	}
	_ = d.store.Set(ctx, briefingOfferKey, "yes")
	d.store.Audit(ctx, "briefing.started", screenChat, schedule)
	now := clock().In(d.location())
	first := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, d.location())
	when := "starting tomorrow"
	if first.After(now) {
		when = "starting today"
	}
	return fmt.Sprintf("Done. I'll brief you at %s every morning, %s.", first.Format("3:04 pm"), when), nil
}

// parseClock reads "7:30" or "07:30" on a 24-hour clock.
func parseClock(s string) (hh, mm int, ok bool) {
	h, m, found := strings.Cut(strings.TrimSpace(s), ":")
	if !found || len(m) != 2 {
		return 0, 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

// CalendarConnected reports whether the twin can read a calendar now: the
// welcome page offers to connect one before the first hello until it can.
func (d *Daemon) CalendarConnected(context.Context) bool { return d.connected().Calendar }

// briefingAccepted reports whether the owner said yes to the offer.
func (d *Daemon) briefingAccepted() bool {
	v, _ := d.store.Get(context.Background(), briefingOfferKey)
	return v == "yes"
}
