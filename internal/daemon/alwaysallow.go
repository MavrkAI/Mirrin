package daemon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// "Yes, always": the owner can stop being asked about a kind of action from
// the conversation itself. The twin checks once, in one line, what it will
// then do without asking; the next "yes" approves the request and adds its
// tool to autonomy.always_allow, and the twin says how to take it back ("ask
// me first about send email", which the ask_before tool also understands).
// Never for something that can't easily be undone (the policy asks about
// dangerous calls whatever always_allow says), for a tool the owner listed
// under always_ask, for tools that change what the twin does on its own
// (writing tools or routines, installing packs), for someone else's
// request, or from a chat where it may not be the owner saying it (IRC or
// email, whose sender can be forged) or where others talk too. "Ask me
// first" works from anywhere: it only ever makes the twin ask more.

// alwaysOffer is the twin's one-line check before it stops asking.
type alwaysOffer struct {
	id   int64 // the request it came with
	tool string
	at   time.Time
}

// neverAlways are tools whose "always" would let a web page or an email the
// twin reads give it standing powers without the owner seeing them: a click
// or a typed answer on a signed-in site (browser_act, whose element check
// only runs when the owner is asked), and keeping something private
// (remember_sensitive, which promises nothing private is kept without a yes).
var neverAlways = map[string]bool{
	"create_tool": true, "create_protocol": true, "update_protocol": true, "install_pack": true, "remove_pack": true,
	"resolve_approval": true, "answer_task": true, "ask_before": true,
	"browser_act": true, "remember_sensitive": true,
}

// reAlways finds the owner saying not to ask again.
var reAlways = regexp.MustCompile(`(?i)\b(?:always(?:\s+allow(?:\s+it)?)?|from\s+now\s+on|every\s*time|(?:and\s+)?(?:you\s+)?(?:don'?t|do\s+not|no\s+need\s+to)\s+(?:ask|check)(?:\s+(?:with\s+)?me)?(?:\s+again)?|stop\s+asking(?:\s+me)?)\b`)

// parseAlways reads "yes, always", "always", "yes 12 and don't ask again":
// a yes, possibly with the request's number, and never to be asked again.
func parseAlways(text string, names []string) (approvals.Reply, bool) {
	if !reAlways.MatchString(text) {
		return approvals.Reply{}, false
	}
	rest := reAlways.ReplaceAllString(text, " ")
	if len(plainWords(rest)) == 0 {
		return approvals.Reply{Approve: true}, true
	}
	r, ok := approvals.ParseReply(rest, names...)
	if !ok || !r.Approve {
		return approvals.Reply{}, false
	}
	return r, true
}

// reAskFirst is the owner taking an "always" back: "ask me first about send
// email", "always ask before send_email again".
var reAskFirst = regexp.MustCompile(`^(?:please\s+)?(?:always\s+|again\s+)?(?:ask|check with)\s+(?:me\s+)?(?:first|before)\s+(?:about\s+|for\s+|on\s+|with\s+)?(.+?)(?:\s+again)?$`)

// alwaysReply handles the owner's "yes, always", their answer to the check
// that follows it, and "ask me first about ..." taking one back. handled is
// false when the message is none of these.
func (d *Daemon) alwaysReply(ctx context.Context, c *conversation, in channels.Inbound, onDelta func(string)) (reply string, handled bool, err error) {
	key := in.Key()
	if tool, ok := d.askFirstTarget(in.Text); ok {
		msg, err := d.askBefore(ctx, tool)
		if err != nil {
			return sentence(err.Error()), true, nil
		}
		return msg, true, nil
	}
	c.mu.Lock()
	offer := c.offer
	c.offer = nil // one answer, whatever it is
	c.mu.Unlock()
	if offer != nil && clock().Sub(offer.at) <= askFresh && !arrivedAt(ctx).Before(offer.at) {
		if r, ok := d.parseReply(in.Text); ok && r.ID == 0 {
			reply, err := d.confirmAlways(ctx, c, in, *offer, r.Approve, onDelta)
			return reply, true, err
		}
	}
	r, ok := parseAlways(in.Text, d.twinNames())
	if !ok {
		return "", false, nil
	}
	ap, ok := d.alwaysTarget(ctx, c, key, r.ID)
	if !ok {
		return "", false, nil // taken as a plain yes (decision), which says what's what
	}
	name := toolLabel(ap.Tool)
	if why := d.alwaysBlocked(ctx, key, *ap); why != "" {
		// A yes all the same: that part the owner meant.
		reply, err := d.decideInChat(ctx, in, ap, true, onDelta)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("%s\n\nI'll still check with you before %q each time: %s.", reply, name, why), true, nil
	}
	c.mu.Lock()
	c.offer = &alwaysOffer{id: ap.ID, tool: ap.Tool, at: clock()}
	c.mu.Unlock()
	c.willAsk(ap.ID) // "yes 12" still approves just this one
	if spoken(in) {
		return fmt.Sprintf("Just to check: from now on I'll go ahead with %s without asking you. Say yes to confirm, or yes %d for just this once.", name, ap.ID), true, nil
	}
	return fmt.Sprintf("Before I stop asking: from now on I'll go ahead with %q without checking with you first. Reply \"yes\" to confirm, or \"yes %d\" for just this once.", name, ap.ID), true, nil
}

// alwaysTarget is the request a "yes, always" is about: the one it names,
// or the one the twin has just asked about.
func (d *Daemon) alwaysTarget(ctx context.Context, c *conversation, key string, id int64) (*memory.Approval, bool) {
	if id != 0 {
		ap, err := d.store.GetApproval(ctx, id)
		if err != nil || ap.Status != "pending" || !d.answerable(key, *ap) || d.forSomeoneElse(ctx, id) {
			return nil, false
		}
		return ap, true
	}
	asked, at, _ := c.lastAsk()
	if clock().Sub(at) > askFresh || arrivedAt(ctx).Before(at) {
		return nil, false
	}
	var live []memory.Approval
	for _, id := range asked {
		if ap, err := d.store.GetApproval(ctx, id); err == nil && ap.Status == "pending" && d.answerable(key, *ap) && !d.forSomeoneElse(ctx, id) {
			live = append(live, *ap)
		}
	}
	if len(live) != 1 {
		return nil, false
	}
	return &live[0], true
}

// alwaysBlocked says why the owner can't stop being asked about ap's tool
// from chat key, or "" when they can.
func (d *Daemon) alwaysBlocked(ctx context.Context, key string, ap memory.Approval) string {
	tool, known := d.agent.Tools().Get(ap.Tool)
	cfg := d.Config()
	switch {
	case forgeable(channelOf(key)):
		// A forged "yes, always" would change the twin for good, unseen.
		return fmt.Sprintf("%s I can't be sure it's you, so say \"yes, always\" from your own chat next time", overChannel(channelOf(key)))
	case channelOf(key) == "voice":
		// Anyone in the room can say yes; a standing permission is given
		// where only the owner can give it.
		return "anyone nearby could say that out loud, so say \"yes, always\" from your own chat app next time"
	case !d.ownersOwnChat(key):
		return "others talk in this chat, so say \"yes, always\" from your own chat next time"
	case d.forSomeoneElse(ctx, ap.ID):
		return "it's someone else's request"
	case ap.Risk >= tools.RiskDangerous || known && tool.Risk() >= tools.RiskDangerous:
		return "it can't easily be undone"
	case d.stepUpNeeded(ctx, ap, true): // stepup.go: reach.step_up asks this device for a passkey
		return "from this device approving it needs Face ID or a passkey, so say \"yes, always\" from your own chat app or on this computer"
	case slices.Contains(cfg.Autonomy.AlwaysAsk, ap.Tool):
		return "your settings say to always ask about it (always_ask)"
	case neverAlways[ap.Tool] || !known:
		return "it changes what I can do on my own"
	}
	return ""
}

// overChannel says how a message came, for a sentence: "on IRC", "by email".
func overChannel(name string) string {
	if name == "mail" {
		return "by email"
	}
	return "on " + channelLabel(name)
}

// confirmAlways acts on the owner's answer to "Before I stop asking".
func (d *Daemon) confirmAlways(ctx context.Context, c *conversation, in channels.Inbound, offer alwaysOffer, yes bool, onDelta func(string)) (string, error) {
	name := toolLabel(offer.tool)
	ap, err := d.store.GetApproval(ctx, offer.id)
	if err != nil {
		return "", err
	}
	if !yes {
		c.willAsk(ap.ID)
		if ap.Status != "pending" {
			return fmt.Sprintf("OK, I'll keep asking before %q.", name), nil
		}
		return fmt.Sprintf("OK, I'll keep asking before %q. #%d is still waiting: reply \"yes %d\" to go ahead just this once.", name, ap.ID, ap.ID), nil
	}
	if cantApprove(ctx) {
		// deviceCantApprove stops this first; a standing permission is
		// never turned on from a device that may not even approve once.
		return cantApproveReply(ctx), nil
	}
	if why := d.alwaysBlocked(ctx, in.Key(), *ap); why != "" { // settings changed in between
		return fmt.Sprintf("I'll keep checking with you before %q: %s.", name, why), nil
	}
	if err := d.setAutonomy(func(a *config.Autonomy) {
		if !slices.Contains(a.AlwaysAllow, offer.tool) {
			a.AlwaysAllow = append(a.AlwaysAllow, offer.tool)
		}
	}); err != nil {
		d.log.Warn("always allow", "tool", offer.tool, "err", err)
		return fmt.Sprintf("I couldn't save that setting (%s), so I'll keep asking. Reply \"yes %d\" to go ahead with #%d just this once.", err, ap.ID, ap.ID), nil
	}
	d.store.Audit(ctx, "autonomy.always_allow", in.Key(), fmt.Sprintf("%s, from #%d", offer.tool, ap.ID))
	note := fmt.Sprintf("From now on I'll go ahead with %q without asking. To change that back, just say \"ask me first about %s\".", name, name)
	if ap.Status != "pending" {
		return sentence(statusLine(ap)) + "\n\n" + note, nil
	}
	reply, err := d.decideInChat(ctx, in, ap, true, onDelta)
	if err != nil {
		return note, err
	}
	return reply + "\n\n" + note, nil
}

// askFirstTarget reads "ask me first about send email" as the owner taking
// back an "always" for a tool they allowed.
func (d *Daemon) askFirstTarget(text string) (string, bool) {
	m := reAskFirst.FindStringSubmatch(strings.Join(plainWords(text), " "))
	if m == nil {
		return "", false
	}
	tool, err := d.allowedTool(m[1])
	return tool, err == nil
}

// allowedTool finds the tool in autonomy.always_allow that the owner means
// ("send email", "send_email", "the send email tool").
func (d *Daemon) allowedTool(said string) (string, error) {
	want := strings.Join(plainWords(strings.ReplaceAll(said, "_", " ")), " ")
	want = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(want, "the "), " tool"))
	var found []string
	for _, t := range d.Config().Autonomy.AlwaysAllow {
		if strings.Join(plainWords(strings.ReplaceAll(t, "_", " ")), " ") == want {
			found = append(found, t)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("I don't have %q among the things I do without asking", said)
	}
	return found[0], nil
}

// askBefore takes tool out of autonomy.always_allow.
func (d *Daemon) askBefore(ctx context.Context, tool string) (string, error) {
	err := d.setAutonomy(func(a *config.Autonomy) {
		a.AlwaysAllow = slices.DeleteFunc(a.AlwaysAllow, func(t string) bool { return t == tool })
	})
	if err != nil {
		return "", fmt.Errorf("I couldn't save that setting: %w", err)
	}
	d.store.Audit(ctx, "autonomy.always_ask_again", "", tool)
	return fmt.Sprintf("Done: I'll ask you before %q again.", toolLabel(tool)), nil
}

// setAutonomy changes the autonomy settings alone: saved to config.yaml,
// then applied to the live policy. Nothing else is reloaded (no model,
// persona or voice, so a "yes, always" can't restart the microphone), and
// an error means nothing changed, so what the twin then says is true.
func (d *Daemon) setAutonomy(mutate func(a *config.Autonomy)) error {
	d.cmu.Lock()
	defer d.cmu.Unlock()
	return config.Edit(func() error { return d.saveAutonomy(mutate) }) // one edit of config.yaml, as UpdateConfig
}

// saveAutonomy is setAutonomy's work, with d.cmu and the file's edit lock held.
func (d *Daemon) saveAutonomy(mutate func(a *config.Autonomy)) error {
	base, err := d.configBase() // the file as it stands, hand edits included
	if err != nil {
		return err
	}
	next := *base
	next.Autonomy = cloneAutonomy(base.Autonomy)
	mutate(&next.Autonomy)
	if err := next.Validate(); err != nil {
		return err
	}
	if err := next.Save(); err != nil {
		return err
	}
	live := *d.cfg
	live.Autonomy = cloneAutonomy(next.Autonomy)
	*d.cfg = live
	d.agent.SetConfig(live)
	d.agent.SetPolicy(approvals.New(live.Autonomy))
	d.store.Audit(context.Background(), "config.updated", "", fmt.Sprintf("autonomy=%s/%s/%s always_allow=%s always_ask=%s",
		live.Autonomy.Read, live.Autonomy.Write, live.Autonomy.Dangerous, strings.Join(live.Autonomy.AlwaysAllow, ","), strings.Join(live.Autonomy.AlwaysAsk, ",")))
	return nil
}

// cloneAutonomy copies a, lists included, so a change to one copy never shows in another.
func cloneAutonomy(a config.Autonomy) config.Autonomy {
	a.AlwaysAllow = slices.Clone(a.AlwaysAllow)
	a.AlwaysAsk = slices.Clone(a.AlwaysAsk)
	return a
}

// askBeforeTool lets the model take back an "always" the owner gave, when
// they ask in words the daemon didn't catch ("check with me before emailing
// people again"). It can only ever make the twin ask more.
func (d *Daemon) askBeforeTool() *tools.Func {
	return tools.New("ask_before",
		"Go back to asking the user before using a tool they had allowed you to use without asking (autonomy.always_allow), when they say so (\"ask me before sending emails again\").",
		tools.Schema(map[string]tools.Prop{"tool": {Type: "string", Description: "The tool's name, e.g. send_email", Required: true}}),
		tools.RiskRead,
		func(ctx context.Context, call tools.Call) (string, error) {
			var in struct{ Tool string }
			if err := tools.Decode(call, &in); err != nil {
				return "", err
			}
			if d.ownerWords(ctx, call.ChatKey) == "" {
				return "", errors.New("only the user can change what you do without asking")
			}
			tool, err := d.allowedTool(in.Tool)
			if err != nil {
				return "", err
			}
			return d.askBefore(ctx, tool)
		})
}

// toolLabel is a tool's name as a person reads it: "send email".
func toolLabel(tool string) string { return strings.ReplaceAll(tool, "_", " ") }
