package daemon

import (
	"context"
	"regexp"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/channels"
	memskill "github.com/MavrkAI/Mirrin/internal/skills/memory"
	"github.com/MavrkAI/Mirrin/internal/watch"
)

// The calendar watcher's clash check (watch/clash.go) reads the owner's
// facts for a routine's time ("Maya's pickup is at 3:45 on weekdays"). The
// daemon decides which facts it may read: never a sensitive one, so a
// clash line can't put health, money, relationships or secrets in front
// of anyone unasked.

// sensitiveFactRe catches private facts kept under an ordinary subject.
var sensitiveFactRe = regexp.MustCompile(`(?i)\b(doctor|dr|gp|nurse|hospital|clinic|surgery|dentist|dental|orthodontist|optician|therapy|therapist|counsel\w*|psychiatrist|psychologist|mental health|medication|meds|prescription|chemo|dialysis|physio\w*|rehab|recovery|sober|sobriety|aa|na|sponsor|addiction|ivf|fertility|pregnan\w*|diagnos\w*|bank|loan|mortgage|debt|salary|payslip|benefits|divorce|affair|dating|password|pin|secret|private)\b`)

// clashFacts are the facts the clash check may read: the owner's, and none
// that is sensitive by subject or by what it says.
func (d *Daemon) clashFacts(ctx context.Context) []string {
	fs, err := d.store.AllFacts(ctx, 1000)
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range fs {
		if memskill.Sensitive(f.Subject) || sensitiveFactRe.MatchString(f.Content) {
			continue
		}
		out = append(out, f.Content)
	}
	return out
}

// The owner turns the clash check off and on by saying so: the whole
// message, so "stop telling me about clashes with Priya's diary" is still
// a request for the model.
var (
	clashOffPhrases = map[string]bool{
		"stop telling me about clashes": true, "stop warning me about clashes": true,
		"no more clash warnings": true, "stop the clash warnings": true, "stop checking for clashes": true,
	}
	clashOnPhrases = map[string]bool{
		"warn me about clashes again": true, "tell me about clashes again": true,
		"start warning me about clashes": true, "check for clashes again": true,
	}
)

// clashSwitch handles the owner's "stop telling me about clashes" and its
// opposite. ok is false for any other message.
func (d *Daemon) clashSwitch(ctx context.Context, in channels.Inbound, text string) (reply string, ok bool) {
	if !in.IsOwner {
		return "", false
	}
	t := strings.Trim(strings.ToLower(strings.TrimSpace(text)), " .,!?;:…")
	t = strings.Join(strings.Fields(t), " ")
	switch {
	case clashOffPhrases[t]:
		_ = d.store.Set(ctx, watch.ClashOffKey, "1")
		return withAddress("Understood", d.address()) + ". I won't point out calendar clashes any more. Say \"warn me about clashes again\" to turn it back on.", true
	case clashOnPhrases[t]:
		_ = d.store.Unset(ctx, watch.ClashOffKey)
		return withAddress("Done", d.address()) + ". I'll tell you when a calendar change clashes with something.", true
	}
	return "", false
}
