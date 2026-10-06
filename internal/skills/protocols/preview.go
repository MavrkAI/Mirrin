package protocols

import (
	"context"
	"fmt"
	"os"
	"strings"

	proto "github.com/MavrkAI/Mirrin/internal/protocols"
)

// PreviewPack fetches the pack at src (a git address, pinned with #ref, as
// Pack.Source gives) into a scratch folder and reads the protocols it would
// add. Nothing is installed: the folder is removed before it returns.
func PreviewPack(ctx context.Context, src string) (string, []proto.Protocol, error) {
	tmp, err := os.MkdirTemp("", "mirrin-pack-preview-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(tmp)
	name, err := proto.AddPack(ctx, tmp, src)
	if err != nil {
		return "", nil, err
	}
	ps, _ := proto.Load(tmp)
	var out []proto.Protocol
	for _, p := range ps {
		if p.Pack == name {
			out = append(out, p)
		}
	}
	return name, out, nil
}

// DescribePreview is what a pack would add, for the owner to read before
// saying yes: each protocol's schedule, what it needs, and every word of
// the instructions it would follow.
func DescribePreview(name string, ps []proto.Protocol) string {
	var b strings.Builder
	if len(ps) == 0 {
		fmt.Fprintf(&b, "Pack %q has no protocols in it.\n", name)
		return b.String()
	}
	fmt.Fprintf(&b, "Pack %q would add %d protocol(s). Nothing is installed yet.\n", name, len(ps))
	for _, p := range ps {
		when := "only when asked"
		if p.Schedule != "" {
			when = "on the schedule " + p.Schedule + " (cron, your timezone)"
		}
		fmt.Fprintf(&b, "\n- %s: %s\n  Runs %s.\n", p.Name, p.Description, when)
		if len(p.Requires) > 0 {
			fmt.Fprintf(&b, "  Needs: %s.\n", strings.Join(p.Requires, ", "))
		}
		prompt := p.RawPrompt
		if prompt == "" {
			prompt = p.Prompt
		}
		fmt.Fprintf(&b, "  Instructions it would follow:\n  %s\n", strings.ReplaceAll(strings.TrimSpace(prompt), "\n", "\n  "))
	}
	return b.String()
}

// previewSource is where install_pack's preview fetches a pack from: a
// registry entry at its reviewed commit, or a git address. A folder on
// this computer is not previewed (reading it would be reading local files).
func previewSource(ctx context.Context, reg *Registry, name string) (string, error) {
	name = strings.TrimSpace(name)
	if !strings.Contains(name, "/") {
		hits, err := reg.Search(ctx, name)
		if err != nil {
			return "", err
		}
		for _, p := range hits {
			if strings.EqualFold(p.Name, name) {
				return p.Source(), nil
			}
		}
		return "", fmt.Errorf("no pack %q in the registry", name)
	}
	if !remoteSource(name) {
		return "", fmt.Errorf("only packs from the registry or a git address (https:// or git@) can be previewed")
	}
	return name, nil
}

// RemoteSource reports whether s is a git address a preview may fetch
// (https://, git@ or ssh://), rather than a folder on this computer.
func RemoteSource(s string) bool { return remoteSource(s) }

func remoteSource(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "git@") || strings.HasPrefix(s, "ssh://")
}
