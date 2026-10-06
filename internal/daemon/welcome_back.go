package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// After "I already have a twin", the welcome page meets the owner in
// character: "Welcome back, Akshay. Same Mirrin, new Mac: 214 things I
// remember, your morning briefing and 2 more routines, and 3 reminders came
// along." Then what is still to do on this Mac, the device review first.
// It is read from the restored twin when the page asks, never from the
// model, so it only claims what is really there.

// restoreWindow is how soon after a restore (backup.State.RestoredAt) its
// report is written. A report written later comes from a restore that
// didn't finish, which left the twin that was here.
const restoreWindow = 15 * time.Minute

// welcomeBack reads the restored twin for the welcome page. ok is false
// when it can't be read, or the report at path (welcome-restore.txt) isn't
// from the restore that put it here.
func (d *Daemon) welcomeBack(ctx context.Context, path string) (info api.RestoreWelcomeInfo, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return info, false
	}
	c := d.Config()
	st, err := backup.LoadState(c.DataDir)
	if err != nil || st.RestoredAt.IsZero() {
		return info, false
	}
	if at := fi.ModTime(); at.Before(st.RestoredAt.Add(-time.Minute)) || at.After(st.RestoredAt.Add(restoreWindow)) {
		return info, false
	}
	_, facts, err := d.store.FactsPage(ctx, 0, 1, "")
	if err != nil {
		return info, false
	}
	pending, err := d.store.AllPendingReminders(ctx, 1000)
	if err != nil {
		return info, false
	}
	// Scheduled routines first: one of those leads the sentence.
	var scheduled, onDemand []string
	for _, p := range d.Protocols() {
		switch {
		case !p.IsEnabled():
		case strings.TrimSpace(p.Schedule) != "":
			scheduled = append(scheduled, p.Name)
		default:
			onDemand = append(onDemand, p.Name)
		}
	}
	d.cmu.RLock()
	pr := d.persona
	address := addressOf(d.cfg, pr)
	d.cmu.RUnlock()
	twin := c.Name
	if strings.TrimSpace(twin) == "" {
		twin = pr.Name
	}
	info = api.RestoreWelcomeInfo{Persona: pr.ID, Facts: facts, Routines: append(scheduled, onDemand...), Reminders: len(pending)}
	info.Line = welcomeBackLine(address, twin, info.Facts, info.Routines, info.Reminders)
	info.Todo = d.stillToDo(&c, st.From)
	if st.From != nil && st.From.StandsBy {
		info.StandBy = oldMac(st.From.Host) + " stands by once it sees the move, and stays paused until you run `mirrin backup resume` there."
	}
	return info, true
}

// welcomeBackLine is the welcome, from a template rather than the model. A
// part that is zero is left out.
func welcomeBackLine(address, twin string, facts int, routines []string, reminders int) string {
	var parts []string
	if facts > 0 {
		parts = append(parts, plural(facts, "thing", "things")+" I remember")
	}
	if len(routines) > 0 {
		r := "your " + strings.TrimSpace(routines[0])
		if more := len(routines) - 1; more > 0 {
			r += " and " + plural(more, "more routine", "more routines")
		}
		parts = append(parts, r)
	}
	if reminders > 0 {
		parts = append(parts, plural(reminders, "reminder", "reminders"))
	}
	line := withAddress("Welcome back.", address) + " Same " + twin + ", new Mac"
	if len(parts) == 0 {
		return line + "."
	}
	return line + ": " + listed(parts) + " came along."
}

// plural is "1 reminder" or "3 reminders".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// listed joins parts as a sentence does: "a and b", "a, b, and c". A part
// with its own "and" takes a comma before the last.
func listed(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	sep := " and "
	if len(parts) > 2 || strings.Contains(parts[0], " and ") {
		sep = ", and "
	}
	return strings.Join(parts[:len(parts)-1], ", ") + sep + parts[len(parts)-1]
}

// oldMac names the machine the twin moved from: its label, or "Your old
// Mac" when the snapshot didn't say.
func oldMac(host string) string {
	if host = strings.TrimSpace(host); host != "" {
		return host
	}
	return "Your old Mac"
}

// stillToDo is what to do on this Mac, each with the page that does it.
// Reviewing the devices always comes first: it is the security step. Then
// a chat app that is on but not connected here, a model key this Mac
// doesn't have, and website sign-ins the old Mac kept.
func (d *Daemon) stillToDo(c *config.Config, from *backup.MovedFrom) []api.RestoreTodo {
	todo := []api.RestoreTodo{{Text: "Review your devices.", URL: "/restore/review"}}
	for _, k := range c.Connectors() {
		switch {
		case !k.Enabled, k.Name == "imessage": // iMessage wants a permission, not a sign-in
		case k.Name == "whatsapp":
			if whatsapp.Built && !whatsapp.Paired(c.DataDir) { // a snapshot never takes the link
				todo = append(todo, api.RestoreTodo{Text: k.Label + " needs one scan.", URL: "/channels"})
			}
		default:
			// Only one that has stopped for good: one still connecting may
			// be fine in a moment.
			if st, _ := d.channelStatus(k.Name); st.State == channels.Failed {
				text := k.Label + " needs signing in again."
				if k.Name == "signal" {
					text = k.Label + " needs one scan."
				}
				todo = append(todo, api.RestoreTodo{Text: text, URL: "/channels"})
			}
		}
	}
	if llm.MissingKey(providerSettings(c)) != nil || d.modelDown.Load() {
		todo = append(todo, api.RestoreTodo{Text: "Add this Mac's model key.", URL: "/accounts#model"})
	}
	if from != nil && from.SignIns && c.Skills.Browser.Enabled {
		if _, err := os.Stat(filepath.Join(c.DataDir, "chrome-profile")); err != nil {
			// The twin's browser asks when a site wants it; no page does this.
			todo = append(todo, api.RestoreTodo{Text: "Sign in to sites again in the browser; sign-ins stay on the old Mac."})
		}
	}
	return todo
}
