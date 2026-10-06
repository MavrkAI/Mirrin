// Package protocols loads user-defined routines. A protocol is a named task
// with an optional schedule, in the spirit of "run the morning briefing".
package protocols

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/persona"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Protocol is one routine. The file format is the contribution format: a
// protocol is a YAML file anyone can write, share in a pack, and install.
type Protocol struct {
	// Name is the identifier the user says ("morning briefing").
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Version, Author and Tags describe a shared protocol.
	Version string   `yaml:"version,omitempty"`
	Author  string   `yaml:"author,omitempty"`
	Tags    []string `yaml:"tags,omitempty"`
	// Schedule is a cron expression (5 fields) or empty for on-demand only.
	Schedule string `yaml:"schedule,omitempty"`
	// Requires lists tools (or skills: calendar, email, browser, web, system) the prompt relies on.
	Requires []string `yaml:"requires,omitempty"`
	// Vars are inputs the user fills in; {{name}} in the prompt is replaced with the value.
	Vars map[string]Var `yaml:"vars,omitempty"`
	// Prompt is the instruction Mirrin executes ({{vars}} substituted at load).
	Prompt string `yaml:"prompt"`
	// Enabled lets a protocol be parked without deleting it.
	Enabled *bool `yaml:"enabled,omitempty"`

	// Source is where it came from (file path); Pack is the pack name, "" for local.
	Source string `yaml:"-"`
	Pack   string `yaml:"-"`
	// RawPrompt is the prompt before variable substitution.
	RawPrompt string `yaml:"-"`
	// Unset lists the {{vars}} left with no value or default after loading.
	Unset []string `yaml:"-"`
}

// Var is one user-fillable input.
type Var struct {
	Description string `yaml:"description,omitempty"`
	Default     string `yaml:"default,omitempty"`
	Required    bool   `yaml:"required,omitempty"`
}

var reVar = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

// Render substitutes {{vars}} into the raw prompt using values (falling back to defaults).
func (p Protocol) Render(values map[string]string) (string, []string) {
	var missing []string
	out := reVar.ReplaceAllStringFunc(p.RawPrompt, func(m string) string {
		key := reVar.FindStringSubmatch(m)[1]
		if v, ok := values[key]; ok && v != "" {
			return v
		}
		if d, ok := p.Vars[key]; ok && d.Default != "" {
			return d.Default
		}
		missing = append(missing, key)
		return m
	})
	return out, missing
}

// skillTools maps each skill name a protocol can require to the tools that
// provide it; any one of them is enough (email is IMAP or Gmail).
var skillTools = map[string][]string{
	"calendar":  {"list_events"},
	"email":     {"list_emails", "gmail_search"},
	"browser":   {"browse_page", "browser_inspect"},
	"web":       {"fetch_url"},
	"system":    {"read_file"},
	"reminders": {"set_reminder"},
	"memory":    {"remember"},
	"protocols": {"list_protocols", "run_protocol"},
	"drive":     {"drive_search"},
	"phone":     {"phone_call", "send_sms"},
}

// Missing returns required tools/skills not present among available tool names.
func (p Protocol) Missing(available []string) []string {
	have := map[string]bool{}
	for _, a := range available {
		have[a] = true
	}
	var out []string
	for _, r := range p.Requires {
		if have[r] || slices.ContainsFunc(skillTools[r], func(t string) bool { return have[t] }) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// IsEnabled treats a missing flag as enabled.
func (p Protocol) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// PacksDir is where installed packs live under the protocols dir.
func PacksDir(dir string) string { return filepath.Join(dir, "packs") }

// VarsPath holds the user's values for protocol variables:
//
//	nightly headlines:
//	  source: https://www.abc.net.au/news
func VarsPath(dir string) string { return filepath.Join(dir, "vars.yaml") }

// Load reads the user's own protocols in dir plus every installed pack under
// dir/packs/<pack>/, substituting variables. Local protocols shadow pack
// protocols of the same name. A file that isn't a usable protocol is skipped
// rather than taking the others down with it; LoadAll says which and why. The
// error is kept for existing callers and is always nil.
func Load(dir string) ([]Protocol, error) {
	ps, _ := LoadAll(dir)
	return ps, nil
}

// LoadAll is Load plus the files it skipped. Each Problem's File is the full
// path, so the user can open it.
func LoadAll(dir string) ([]Protocol, []Problem) {
	values := map[string]map[string]string{}
	var problems []Problem
	if b, err := os.ReadFile(VarsPath(dir)); err == nil {
		if err := yaml.Unmarshal(b, &values); err != nil {
			problems = append(problems, Problem{VarsPath(dir), "error", "not valid YAML, so no vars were applied: " + err.Error()})
		}
	}
	var out []Protocol
	seen := map[string]Protocol{} // by lowercase name
	add := func(ps []Protocol) {
		for _, p := range ps {
			key := strings.ToLower(p.Name)
			if prev, ok := seen[key]; ok {
				// A local file shadowing a pack one is the documented way to fork it.
				if prev.Pack != "" || p.Pack == "" {
					problems = append(problems, Problem{p.Source, "error", fmt.Sprintf("%s already defines %q", prev.Source, p.Name)})
				}
				continue
			}
			seen[key] = p
			p.RawPrompt = p.Prompt
			p.Prompt, p.Unset = p.Render(values[p.Name])
			out = append(out, p)
		}
	}
	add(loadDir(dir, "", &problems))
	if packs, err := os.ReadDir(PacksDir(dir)); err == nil {
		for _, pk := range packs {
			// Dot directories are packs still being installed.
			if !pk.IsDir() || strings.HasPrefix(pk.Name(), ".") {
				continue
			}
			pdir, err := packProtocolsDir(filepath.Join(PacksDir(dir), pk.Name()))
			if err != nil {
				problems = append(problems, Problem{pdir, "error", err.Error()})
				continue
			}
			add(loadDir(pdir, pk.Name(), &problems))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, problems
}

// packProtocolsDir is where a pack keeps its protocols: its protocols/ folder,
// or, for packs made before that layout, its top level. Other YAML at the top
// of a pack (mkdocs.yml, CI config) is never read as a protocol. A protocols/
// that is a symbolic link is refused, since it could point anywhere on this
// machine; git clones links as they are.
func packProtocolsDir(root string) (string, error) {
	sub := filepath.Join(root, "protocols")
	st, err := os.Lstat(sub)
	switch {
	case err != nil:
		return root, nil
	case st.Mode()&fs.ModeSymlink != 0:
		return sub, errSymlinkInPack
	case st.IsDir():
		return sub, nil
	}
	return root, nil
}

var errSymlinkInPack = errors.New("symbolic links are not loaded from packs")

// loadDir reads the *.yaml files directly in dir, adding any it has to skip to problems.
func loadDir(dir, pack string, problems *[]Problem) []Protocol {
	skip := func(path, msg string) { *problems = append(*problems, Problem{path, "error", msg}) }
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			skip(dir, err.Error())
		}
		return nil
	}
	var out []Protocol
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || ext != ".yaml" && ext != ".yml" || e.Name() == "vars.yaml" || e.Name() == "pack.yaml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		// A pack's files are its own; a link could point anywhere on this machine.
		if pack != "" && e.Type()&fs.ModeSymlink != 0 {
			skip(path, errSymlinkInPack.Error())
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			skip(path, err.Error())
			continue
		}
		var p Protocol
		if err := yaml.Unmarshal(b, &p); err != nil {
			skip(path, "not valid YAML: "+err.Error())
			continue
		}
		if p.Name == "" {
			p.Name = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		}
		if strings.TrimSpace(p.Prompt) == "" {
			skip(path, "prompt is required")
			continue
		}
		p.Source, p.Pack = path, pack
		out = append(out, p)
	}
	return out
}

// Find returns a protocol by case-insensitive name.
func Find(ps []Protocol, name string) (Protocol, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, p := range ps {
		if strings.ToLower(p.Name) == name {
			return p, true
		}
	}
	return Protocol{}, false
}

// Check reports why a new protocol couldn't be saved or run, in plain words:
// no name or prompt, a name with no letters, a schedule cron can't read.
func Check(p Protocol) error {
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Prompt) == "" {
		return errors.New("a protocol needs a name and a prompt")
	}
	if Slug(p.Name) == "" {
		return errors.New("give the protocol a name with at least one letter or number")
	}
	if s := strings.TrimSpace(p.Schedule); s != "" {
		if _, err := cron.ParseStandard(s); err != nil {
			return scheduleError(s)
		}
	}
	return nil
}

// Write saves a new protocol as YAML in dir, in a file named after it. It
// checks it first (Check), so a protocol that could never run is not saved,
// and it never replaces an existing file.
func Write(dir string, p Protocol) (string, error) {
	if err := Check(p); err != nil {
		return "", err
	}
	slug := Slug(p.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := yaml.Marshal(p)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, slug+".yaml")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("%s already exists; edit it, or pick another name", path)
	}
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}

// scheduleError says what is wrong with a schedule cron can't read, in words
// (cron's own message lists the fields it parsed).
func scheduleError(s string) error {
	msg := fmt.Sprintf("the schedule %q can't be run; use a cron expression with 5 fields (minute hour day month weekday), like \"0 7 * * *\" for 7am daily", s)
	switch n := len(strings.Fields(s)); {
	case n == 6:
		msg += "; it has 6, so drop the seconds"
	case n != 5 && !strings.HasPrefix(s, "@"):
		msg += fmt.Sprintf("; it has %d", n)
	}
	return errors.New(msg)
}

// Slug is the file name a protocol name gets, the same as a persona's:
// lowercase letters and digits in any script ("朝のまとめ" stays readable),
// with each run of anything else turned into one hyphen.
func Slug(name string) string { return persona.Slug(name) }

// WriteExamples drops starter protocols into dir if it is empty.
func WriteExamples(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) > 0 {
		return nil
	}
	for name, body := range Examples {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Examples are the protocols shipped with a fresh install.
var Examples = map[string]string{
	"morning-briefing.yaml": `name: morning briefing
description: The day at a glance, delivered before the user asks.
version: 1.0.0
author: Mirrin
tags: [briefing, daily]
requires: [reminders]
schedule: "0 7 * * *"
prompt: |
  Prepare the morning briefing. Check today's calendar and any reminders due today.
  If email is available, note anything from the last 12 hours that needs a reply or a decision.
  Deliver it as a short message: greeting, the day's shape in two or three lines,
  then anything that needs a decision. Skip sections that are empty. If there is truly nothing
  (no events, no reminders, no mail), reply NOTHING_TO_REPORT.
`,
	"evening-wrap.yaml": `name: evening wrap
description: Close out the day and set up tomorrow.
version: 1.0.0
author: Mirrin
tags: [briefing, daily]
requires: [reminders]
schedule: "0 21 * * *"
prompt: |
  Look at tomorrow's calendar and any reminders due tomorrow. If the first event is early or
  unusual, say so. Ask at most one question if something needs preparing. Keep it to three lines.
  If tomorrow is empty and nothing is due, reply NOTHING_TO_REPORT.
`,
	"reply-like-me.yaml": `name: reply like me
description: Reply to an email the way the user would write it.
version: 1.0.0
author: Mirrin
tags: [email, chores]
requires: [email]
schedule: ""
prompt: |
  The user wants to reply to an email. If they named it, find it (list_emails, or gmail_search with Gmail);
  otherwise ask which one. Read it (read_email or gmail_read). Then read two or three of the user's own
  recent sent emails (list_sent_emails and read_sent_email, or gmail_search for "in:sent" and gmail_read)
  to match their tone, sign-off and length. Recall anything you know about the sender.
  Draft the reply in their voice, show it in full, and send it (reply_email or gmail_reply); the approval
  step lets them say yes or ask for changes. Never send without that approval.
`,
	"rebook.yaml": `name: rebook
description: Move or cancel an appointment and tell the other side.
version: 1.0.0
author: Mirrin
tags: [calendar, chores]
requires: [calendar]
schedule: ""
prompt: |
  The user wants to move or cancel an appointment. Find it on the calendar (list_events). If moving,
  propose a time that works around their other commitments and update it with update_event; if
  cancelling, delete_event. Then, if the appointment involved someone else, draft a short message to
  them (by email: reply_email or send_email, or gmail_reply or gmail_send with Gmail) explaining the change and
  offering the new time. If the booking lives on a website, use browser_inspect and browser_act to
  make the change there, taking a screenshot before the final submit so the user can approve it.
  If you wrote to someone about the change, set a follow_up for two business days from now to
  check for their reply.
`,
	"chase-refund.yaml": `name: chase refund
description: Get money back from a merchant, politely and persistently.
version: 1.0.0
author: Mirrin
tags: [email, browser, chores]
requires: [email]
schedule: ""
prompt: |
  The user wants a refund. Find the order or receipt email (list_emails and read_email, or gmail_search
  and gmail_read with Gmail) to get the merchant, order number, date, amount and what went wrong. Look up the merchant's refund or
  contact page with fetch_url or browser_inspect. Then either draft a firm, courteous refund request
  email citing the order details and the relevant consumer-law right to a remedy, or fill their
  refund form with browser_act, screenshotting before the final submit. Either way the user
  approves before anything is sent. Finally set a follow_up for five business days from now to
  check for their reply.
`,
	"inbox-triage.yaml": `name: inbox triage
description: Surface the emails that actually matter.
version: 1.0.0
author: Mirrin
tags: [email]
requires: [email]
schedule: ""
prompt: |
  Read the most recent 20 emails (list_emails, or gmail_search with Gmail). Group them into: needs a reply,
  needs a decision, FYI, ignore. Report only the first two groups, one line each with sender and what they want.
  If no email is connected (neither list_emails nor gmail_search is available), say so in one line and stop.
`,
}
