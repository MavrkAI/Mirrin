package protocols

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	proto "github.com/MavrkAI/Mirrin/internal/protocols"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Change is one change to a protocol: a new schedule, on or off, or skip
// only its next scheduled run. Nil and false leave that part as it is.
type Change struct {
	Name     string
	Schedule *string
	Enabled  *bool
	SkipNext bool
}

// Updater is what update_protocol needs from the twin.
type Updater struct {
	// Apply makes the change, reschedules, and says what it did.
	Apply func(ctx context.Context, c Change) (string, error)
	// Next is when a protocol next runs on its schedule, in the zone the
	// twin keeps: zero when it is off or runs only when asked.
	Next func(p proto.Protocol) time.Time
	// CheckSchedule says why a schedule can't run (heartbeat.CheckSchedule).
	CheckSchedule func(schedule string) error
}

// UpdateTool is update_protocol: move a routine, turn it off or on, or skip
// its next run, in one sentence from the owner. It is a write, so the
// owner says yes to the change in words ("Move “morning briefing” from
// every day at 7:00 to weekdays at 8:30."), and a change that couldn't be
// made is refused before they are asked.
func UpdateTool(load Loader, u Updater) tools.Tool {
	return tools.WithSummaryAndCheck(tools.New("update_protocol",
		"Change one of the user's protocols (routines): move it to a new schedule, turn it off or back on, or skip only its next scheduled run. "+
			"Use for \"move my briefing to 8:30 on weekdays\", \"skip tomorrow's briefing\", \"turn off the nightly headlines\". "+
			"Changing one that came in a pack makes the user's own copy, which stops getting the pack's updates.",
		tools.Schema(map[string]tools.Prop{
			"name":      {Type: "string", Description: "The protocol's name, as list_protocols shows it", Required: true},
			"schedule":  {Type: "string", Description: "New cron expression (5 fields, the user's timezone), e.g. '30 8 * * 1-5' for weekdays at 8:30; leave out to keep it"},
			"enabled":   {Type: "boolean", Description: "false turns it off, true turns it back on; leave out to keep it"},
			"skip_next": {Type: "boolean", Description: "true skips only its next scheduled run; it carries on after that"},
		}), tools.RiskWrite,
		func(ctx context.Context, call tools.Call) (string, error) {
			c, _, err := checkChange(load, u, call)
			if err != nil {
				return "", err
			}
			return u.Apply(ctx, c)
		}), func(call tools.Call) string {
		c, p, err := checkChange(load, u, call)
		if err != nil {
			var in struct{ Name string }
			_ = tools.Decode(call, &in)
			return fmt.Sprintf("Change “%s”.", strings.TrimSpace(in.Name))
		}
		return ChangeSummary(c, p, u.Next)
	}, func(_ context.Context, call tools.Call) error {
		_, _, err := checkChange(load, u, call)
		return err
	})
}

// checkChange reads an update_protocol call and says why it can't be made:
// no such protocol, a schedule that can't run, nothing to skip, or nothing
// that would change. A part already as asked is dropped.
func checkChange(load Loader, u Updater, call tools.Call) (Change, proto.Protocol, error) {
	var in struct {
		Name     string
		Schedule *string
		Enabled  *bool
		SkipNext bool `json:"skip_next"`
	}
	if err := tools.Decode(call, &in); err != nil {
		return Change{}, proto.Protocol{}, err
	}
	p, ok := proto.Find(load(), in.Name)
	if !ok {
		return Change{}, proto.Protocol{}, fmt.Errorf("no protocol named %q; list_protocols shows them", strings.TrimSpace(in.Name))
	}
	c := Change{Name: p.Name, SkipNext: in.SkipNext}
	var already []string
	if in.Schedule != nil {
		s := strings.TrimSpace(*in.Schedule)
		switch {
		case s == "": // left out, said as empty
		case s == strings.TrimSpace(p.Schedule):
			already = append(already, fmt.Sprintf("“%s” already runs %s", p.Name, proto.Describe(s)))
		default:
			if u.CheckSchedule != nil {
				if err := u.CheckSchedule(s); err != nil {
					return Change{}, p, err
				}
			}
			c.Schedule = &s
		}
	}
	if in.Enabled != nil {
		if *in.Enabled == p.IsEnabled() {
			already = append(already, fmt.Sprintf("“%s” is already %s", p.Name, onOff(*in.Enabled)))
		} else {
			c.Enabled = in.Enabled
		}
	}
	if c.SkipNext {
		if after := changed(p, c); u.Next == nil || u.Next(after).IsZero() {
			why := "it runs only when asked"
			if !after.IsEnabled() {
				why = "it's off"
			}
			return Change{}, p, fmt.Errorf("“%s” has no scheduled run to skip: %s", p.Name, why)
		}
	}
	if c.Schedule == nil && c.Enabled == nil && !c.SkipNext {
		if len(already) > 0 {
			return Change{}, p, errors.New(strings.Join(already, "; ") + ", so there is nothing to change")
		}
		return Change{}, p, errors.New("say what to change: a schedule, enabled, or skip_next")
	}
	return c, p, nil
}

// changed is p as it will be once c is made.
func changed(p proto.Protocol, c Change) proto.Protocol {
	if c.Schedule != nil {
		p.Schedule = *c.Schedule
	}
	if c.Enabled != nil {
		on := *c.Enabled
		p.Enabled = &on
	}
	return p
}

// ChangeSummary is what the owner says yes to, in words: "Move “morning
// briefing” from every day at 7:00 to weekdays at 8:30." next says when a
// protocol next runs (Updater.Next).
func ChangeSummary(c Change, p proto.Protocol, next func(proto.Protocol) time.Time) string {
	var s []string
	if c.Schedule != nil {
		if strings.TrimSpace(p.Schedule) == "" {
			s = append(s, fmt.Sprintf("Run “%s” %s.", p.Name, proto.Describe(*c.Schedule)))
		} else {
			s = append(s, fmt.Sprintf("Move “%s” from %s to %s.", p.Name, proto.Describe(p.Schedule), proto.Describe(*c.Schedule)))
		}
	}
	if c.Enabled != nil {
		s = append(s, fmt.Sprintf("Turn %s “%s”.", onOff(*c.Enabled), p.Name))
	}
	if c.SkipNext && next != nil {
		if t := next(changed(p, c)); !t.IsZero() {
			s = append(s, fmt.Sprintf("Skip the next “%s” (%s).", p.Name, proto.WhenText(t, time.Now().In(t.Location()))))
		}
	}
	if p.Pack != "" && (c.Schedule != nil || c.Enabled != nil) {
		s = append(s, fmt.Sprintf("This makes your own copy, so it stops getting updates from the “%s” pack.", p.Pack))
	}
	return strings.Join(s, " ")
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
