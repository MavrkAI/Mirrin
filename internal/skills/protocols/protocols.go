// Package protocols exposes user-defined routines as tools.
package protocols

import (
	"context"
	"fmt"
	"strings"

	proto "github.com/MavrkAI/Mirrin/internal/protocols"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Loader returns the current protocol list.
type Loader func() []proto.Protocol

// Registry access for finding and installing community packs; may be nil.
type Registry struct {
	Search  func(ctx context.Context, term string) ([]proto.Pack, error)
	Install func(ctx context.Context, nameOrURL string) (string, error)
}

// Tools returns list_protocols, run_protocol and create_protocol (and, with a
// registry, find_protocols and install_pack). save persists a new protocol and
// reschedules; it may be nil.
//
// Running and creating protocols are write actions. A protocol is standing
// orders: if reading a web page or an email could create or fire one without
// a yes, injected text would get a lasting foothold with no gate at all.
func Tools(load Loader, run func(ctx context.Context, chatKey string, p proto.Protocol) (string, error), save func(p proto.Protocol) (string, error), reg *Registry) []tools.Tool {
	runSummary := func(call tools.Call) string {
		var in struct{ Name string }
		_ = tools.Decode(call, &in)
		if p, ok := proto.Find(load(), in.Name); ok && p.Description != "" {
			return fmt.Sprintf("run_protocol %q now: %s", p.Name, p.Description)
		}
		return fmt.Sprintf("run_protocol %q now", in.Name)
	}
	ts := []tools.Tool{
		tools.New("list_protocols", "List the user's protocols (named routines) and their schedules.", nil, tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				ps := load()
				if len(ps) == 0 {
					return "no protocols defined", nil
				}
				var b strings.Builder
				for _, p := range ps {
					sched := p.Schedule
					if sched == "" {
						sched = "on demand"
					}
					state := ""
					if !p.IsEnabled() {
						state = " (disabled)"
					}
					fmt.Fprintf(&b, "- %s [%s]%s: %s\n", p.Name, sched, state, p.Description)
				}
				return b.String(), nil
			}),
		tools.WithSummary(tools.New("run_protocol", "Run one of the user's protocols now and return its output.",
			tools.Schema(map[string]tools.Prop{"name": {Type: "string", Description: "Protocol name", Required: true}}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Name string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				p, ok := proto.Find(load(), in.Name)
				if !ok {
					return "", fmt.Errorf("no protocol named %q", in.Name)
				}
				return run(ctx, call.ChatKey, p)
			}), runSummary),
	}
	if save != nil {
		ts = append(ts, tools.WithSummaryAndCheck(tools.New("create_protocol",
			"Create a new protocol: a named routine with an optional schedule (cron, 5 fields, in the user's timezone) and the prompt you will follow when it runs. Use when the user agrees to automate something recurring.",
			tools.Schema(map[string]tools.Prop{
				"name":        {Type: "string", Description: "Short name, e.g. 'friday briefing'", Required: true},
				"description": {Type: "string", Description: "One line on what it does", Required: true},
				"schedule":    {Type: "string", Description: "Cron expression like '0 17 * * 5' (Fridays 5pm), or empty for on-demand"},
				"prompt":      {Type: "string", Description: "The instructions to follow when it runs, written to yourself", Required: true},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Name, Description, Schedule, Prompt string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if err := checkNew(load, in.Name, in.Schedule, in.Prompt); err != nil {
					return "", err
				}
				prompt := in.Prompt
				if strings.TrimSpace(in.Schedule) != "" && !strings.Contains(prompt, "NOTHING_TO_REPORT") {
					prompt = strings.TrimRight(prompt, "\n ") + "\nIf there is nothing worth saying, reply NOTHING_TO_REPORT.\n"
				}
				path, err := save(proto.Protocol{Name: strings.ToLower(strings.TrimSpace(in.Name)), Description: in.Description, Version: "0.1.0", Author: "Mirrin (from a conversation)", Schedule: strings.TrimSpace(in.Schedule), Prompt: prompt})
				if err != nil {
					return "", err
				}
				when := "on demand"
				if in.Schedule != "" {
					when = "cron " + in.Schedule
				}
				return fmt.Sprintf("created protocol %q (%s) at %s", in.Name, when, path), nil
			}), createSummary, func(_ context.Context, call tools.Call) error {
			// Before the owner is asked: a routine that can't be saved isn't put to them.
			var in struct{ Name, Schedule, Prompt string }
			if err := tools.Decode(call, &in); err != nil {
				return err
			}
			return checkNew(load, in.Name, in.Schedule, in.Prompt)
		}))
	}
	if reg != nil {
		ts = append(ts,
			tools.New("find_protocols", "Search the community registry for packs of protocols (routines other people wrote) by topic, e.g. 'travel', 'news', 'fitness'. Use when the user wants you to do something recurring that isn't set up yet.",
				tools.Schema(map[string]tools.Prop{"query": {Type: "string", Description: "Topic or words to search for", Required: true}}), tools.RiskRead,
				func(ctx context.Context, call tools.Call) (string, error) {
					var in struct{ Query string }
					if err := tools.Decode(call, &in); err != nil {
						return "", err
					}
					hits, err := reg.Search(ctx, in.Query)
					if err != nil {
						return "", err
					}
					if len(hits) == 0 {
						return "no packs match; offer to create a protocol with create_protocol instead", nil
					}
					var b strings.Builder
					for _, p := range hits {
						fmt.Fprintf(&b, "- %s: %s (%s) tags: %s\n", p.Name, p.Description, p.Repo, strings.Join(p.Tags, ", "))
					}
					return b.String(), nil
				}),
			tools.New("install_pack", "Install a pack of protocols from the registry by name (or a git URL). First call it with preview=true: that installs nothing and shows each protocol's schedule, what it needs and its instructions, so you can tell the user what they would get. Installing needs the user's approval. After installing, tell them what protocols arrived and which need setup.",
				tools.Schema(map[string]tools.Prop{
					"name":    {Type: "string", Description: "Pack name from find_protocols, or a git URL", Required: true},
					"preview": {Type: "boolean", Description: "true to see what the pack would add, installing nothing"},
				}), tools.RiskRead,
				func(ctx context.Context, call tools.Call) (string, error) {
					var in struct {
						Name    string
						Preview bool
					}
					if err := tools.Decode(call, &in); err != nil {
						return "", err
					}
					if in.Preview { // preview.go: fetched into a scratch folder, installs nothing
						src, err := previewSource(ctx, reg, in.Name)
						if err != nil {
							return "", err
						}
						name, ps, err := PreviewPack(ctx, src)
						if err != nil {
							return "", err
						}
						return DescribePreview(name, ps) + "\nTo install it, call install_pack again without preview; the user approves first.", nil
					}
					name, err := reg.Install(ctx, in.Name)
					if err != nil {
						return "", err
					}
					var b strings.Builder
					fmt.Fprintf(&b, "installed pack %q. Protocols now available:\n", name)
					for _, p := range load() {
						if p.Pack == name {
							fmt.Fprintf(&b, "- %s: %s", p.Name, p.Description)
							if len(p.Requires) > 0 {
								fmt.Fprintf(&b, " (requires %s)", strings.Join(p.Requires, ", "))
							}
							b.WriteString("\n")
						}
					}
					return b.String(), nil
				}).WithRiskFor(installRisk))
	}
	return ts
}

// installRisk: installing a pack adds standing orders, a write that needs a
// yes. A preview of the registry or an https:// address installs nothing.
// A preview over ssh (git@, ssh://) still needs a yes: it signs in to that
// host with the owner's own SSH keys.
func installRisk(_ context.Context, call tools.Call) tools.Risk {
	var in struct {
		Name    string
		Preview bool
	}
	if tools.Decode(call, &in) == nil && in.Preview && (!strings.Contains(in.Name, "/") || strings.HasPrefix(in.Name, "https://")) {
		return tools.RiskRead
	}
	return tools.RiskWrite
}

// createSummary is the approval for create_protocol: when it will run and
// every word of what it will do, since it keeps running after this yes.
// checkNew reports why a protocol by these could not be created.
func checkNew(load Loader, name, schedule, prompt string) error {
	if _, exists := proto.Find(load(), name); exists {
		return fmt.Errorf("a protocol named %q already exists; to move it, turn it off or skip a run, use update_protocol", name)
	}
	return proto.Check(proto.Protocol{Name: strings.TrimSpace(name), Schedule: strings.TrimSpace(schedule), Prompt: prompt})
}

func createSummary(call tools.Call) string {
	var in struct{ Name, Description, Schedule, Prompt string }
	_ = tools.Decode(call, &in)
	when := "only when asked"
	if s := strings.TrimSpace(in.Schedule); s != "" {
		when = "on the schedule " + s + " (cron, your timezone)"
	}
	return fmt.Sprintf("create_protocol %q: %s\nRuns %s.\nInstructions it will follow:\n%s",
		strings.TrimSpace(in.Name), strings.TrimSpace(in.Description), when, strings.TrimSpace(in.Prompt))
}
