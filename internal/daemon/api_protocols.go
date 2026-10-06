package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/heartbeat"
	"github.com/MavrkAI/Mirrin/internal/protocols"
	protoskill "github.com/MavrkAI/Mirrin/internal/skills/protocols"
)

// The protocol store's pins, updates and install preview (api.PackUpdater).

// packCheck and packApply are protocols.CheckPackUpdates and
// ApplyPackUpdate; tests replace them.
var (
	packCheck = protocols.CheckPackUpdates
	packApply = protocols.ApplyPackUpdate
	// packPreview reads a pack's protocols from a source (seam for tests).
	packPreview = protoskill.PreviewPack
)

// PackCommits is the commit each installed pack is at.
func (d *Daemon) PackCommits(ctx context.Context) map[string]string {
	pks, _ := protocols.InstalledPacks(d.Config().ProtocolsDir)
	out := make(map[string]string, len(pks))
	for _, p := range pks {
		out[p.Dir] = p.Commit
	}
	return out
}

// CheckPackUpdates fetches what each pack would move to, changing nothing.
func (d *Daemon) CheckPackUpdates(ctx context.Context) ([]api.PackUpdate, error) {
	ups, err := packCheck(ctx, d.Config().ProtocolsDir)
	if err != nil {
		return nil, err
	}
	out := make([]api.PackUpdate, 0, len(ups))
	for _, u := range ups {
		pu := api.PackUpdate{Dir: u.Dir, From: u.From, To: u.To, Pending: u.Pending(), Changes: u.Changes, Note: u.Note}
		if u.Err != nil {
			pu.Error = u.Err.Error()
		}
		out = append(out, pu)
	}
	return out, nil
}

// ApplyPackUpdate checks again and moves the pack in dir to what it finds,
// then reloads the protocols.
func (d *Daemon) ApplyPackUpdate(ctx context.Context, dir string) error {
	pdir := d.Config().ProtocolsDir
	ups, err := packCheck(ctx, pdir)
	if err != nil {
		return err
	}
	for _, u := range ups {
		if u.Dir != dir {
			continue
		}
		switch {
		case u.Err != nil:
			return u.Err
		case u.Note != "":
			return fmt.Errorf("%s", u.Note)
		case !u.Pending():
			return nil // already current
		}
		if err := packApply(ctx, pdir, u); err != nil {
			return err
		}
		d.store.Audit(ctx, "pack.updated", "", dir+" "+protocols.ShortCommit(u.From)+" → "+protocols.ShortCommit(u.To))
		return d.ReloadProtocols()
	}
	return fmt.Errorf("no pack %q is installed", dir)
}

// PreviewPack is what a pack would add: a registry entry at its reviewed
// commit, or a git address. Nothing is installed.
func (d *Daemon) PreviewPack(ctx context.Context, nameOrURL string) (api.PackPreview, error) {
	src := strings.TrimSpace(nameOrURL)
	if !strings.Contains(src, "/") {
		reg, err := protocols.FetchRegistry(ctx, d.Config().ProtocolRegistry)
		if err != nil {
			return api.PackPreview{}, err
		}
		pk, ok := reg.Find(src)
		if !ok {
			return api.PackPreview{}, fmt.Errorf("no pack %q in the registry", src)
		}
		src = pk.Source()
	} else if !protoskill.RemoteSource(src) {
		// As in chat (install_pack): a preview never reads a folder on
		// this computer.
		return api.PackPreview{}, fmt.Errorf("only packs from the registry or a git address (https:// or git@) can be previewed")
	}
	name, ps, err := packPreview(ctx, src)
	if err != nil {
		return api.PackPreview{}, err
	}
	out := api.PackPreview{Name: name, Protocols: []api.ProtocolInfo{}}
	for _, p := range ps {
		prompt := p.RawPrompt
		if prompt == "" {
			prompt = p.Prompt
		}
		out.Protocols = append(out.Protocols, api.ProtocolInfo{Name: p.Name, Description: p.Description, Schedule: p.Schedule, Pack: name,
			Author: p.Author, Version: p.Version, Tags: p.Tags, Requires: p.Requires, Missing: p.Missing(d.Tools()), Enabled: p.IsEnabled(), Prompt: prompt})
	}
	return out, nil
}

var _ api.PackUpdater = (*Daemon)(nil)

// Changing a routine in a sentence (update_protocol) or on the Routines
// page (api.ProtocolEditor): a new schedule, on or off, skip the next run.

// protocolUpdater is what update_protocol changes protocols with.
func (d *Daemon) protocolUpdater() protoskill.Updater {
	return protoskill.Updater{Apply: d.changeProtocol, CheckSchedule: heartbeat.CheckSchedule,
		Next: func(p protocols.Protocol) time.Time { return d.beat.NextRun(p) }}
}

// changeProtocol makes a change the owner asked for. A new schedule or on
// or off is written to the protocol's file (to the owner's own copy, for
// one from a pack: protocols.Edit), then everything is rescheduled.
// Skipping the next run is kept in the store, for the heartbeat to skip
// that run and only that one. It says what it did, in words.
func (d *Daemon) changeProtocol(ctx context.Context, c protoskill.Change) (string, error) {
	p, ok := protocols.Find(d.Protocols(), c.Name)
	if !ok {
		return "", fmt.Errorf("no protocol named %q", c.Name)
	}
	if c.Schedule != nil {
		if strings.TrimSpace(*c.Schedule) == "" {
			return "", errors.New("pick a time for it to run")
		}
		if err := heartbeat.CheckSchedule(*c.Schedule); err != nil {
			return "", err
		}
	}
	after := p
	if c.Schedule != nil {
		after.Schedule = strings.TrimSpace(*c.Schedule)
	}
	if c.Enabled != nil {
		on := *c.Enabled
		after.Enabled = &on
	}
	if c.SkipNext && d.beat.NextRun(after).IsZero() {
		return "", fmt.Errorf("%q has no scheduled run to skip", p.Name)
	}
	var said []string
	if c.Schedule != nil || c.Enabled != nil {
		path, err := protocols.Edit(d.Config().ProtocolsDir, p, c.Schedule, c.Enabled)
		if err != nil {
			return "", err
		}
		what := "runs " + protocols.Describe(after.Schedule)
		if !after.IsEnabled() {
			what = "is off"
		}
		said = append(said, fmt.Sprintf("%q now %s.", p.Name, what))
		if p.Pack != "" {
			said = append(said, fmt.Sprintf("It's the owner's own copy now (%s), so it no longer follows the %q pack.", path, p.Pack))
		}
		d.store.Audit(ctx, "protocol.changed", "", p.Name+" "+what)
		if err := d.ReloadProtocols(); err != nil {
			return "", err
		}
	}
	if c.SkipNext {
		if fresh, ok := protocols.Find(d.Protocols(), p.Name); ok {
			after = fresh
		}
		next := d.beat.NextRun(after)
		if next.IsZero() {
			return "", fmt.Errorf("%q has no scheduled run to skip", p.Name)
		}
		if err := d.store.Set(ctx, protocols.SkipKey(p.Name), next.Format(time.DateOnly)); err != nil {
			return "", err
		}
		d.store.Audit(ctx, "protocol.skip", "", p.Name+": "+next.Format(time.DateOnly))
		said = append(said, fmt.Sprintf("The next %q (%s) won't run; it carries on as usual after that.", p.Name, protocols.WhenText(next, time.Now().In(next.Location()))))
	}
	return strings.Join(said, " "), nil
}

// SetProtocolEnabled turns a protocol on or off (the Routines page's switch).
func (d *Daemon) SetProtocolEnabled(ctx context.Context, name string, on bool) error {
	_, err := d.changeProtocol(ctx, protoskill.Change{Name: name, Enabled: &on})
	return err
}

// SkipNextProtocol skips a protocol's next scheduled run (Skip next).
func (d *Daemon) SkipNextProtocol(ctx context.Context, name string) error {
	_, err := d.changeProtocol(ctx, protoskill.Change{Name: name, SkipNext: true})
	return err
}

// RescheduleProtocol gives a protocol a new schedule (the page's time field).
func (d *Daemon) RescheduleProtocol(ctx context.Context, name, schedule string) error {
	_, err := d.changeProtocol(ctx, protoskill.Change{Name: name, Schedule: &schedule})
	return err
}

// protocolInfo is a protocol as the Routines page shows it: when it runs,
// in words, and whether its next run is being skipped.
func (d *Daemon) protocolInfo(ctx context.Context, p protocols.Protocol, have []string) api.ProtocolInfo {
	info := api.ProtocolInfo{Name: p.Name, Description: p.Description, Schedule: p.Schedule, Pack: p.Pack, Author: p.Author, Version: p.Version,
		Tags: p.Tags, Requires: p.Requires, Missing: p.Missing(have), Enabled: p.IsEnabled(), When: protocols.Describe(p.Schedule)}
	if d.beat != nil {
		if next := d.beat.NextRun(p); !next.IsZero() {
			day, _ := d.store.Get(ctx, protocols.SkipKey(p.Name))
			info.Skipping = day == next.Format(time.DateOnly)
		}
	}
	return info
}

var _ api.ProtocolEditor = (*Daemon)(nil)
