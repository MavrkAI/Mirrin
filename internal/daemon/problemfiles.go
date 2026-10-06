package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/persona"
	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// A protocol or persona file the twin can't use is skipped, and the rest
// load. The owner hears of each such file once (not at every load or
// restart), and the Health page lists them until they are fixed.

// problemFilesKey remembers, per kind of file, the problems the owner was
// told about. A problem that is fixed is forgotten, so if it comes back
// they hear of it again.
const problemFilesKey = "problem_files.v1"

// problemMu makes one notice at a time, so two loads close together can't
// both tell the owner of the same file.
var problemMu sync.Mutex

func protocolProblemList(ps []protocols.Problem) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.File+" ("+p.Message+")")
	}
	return out
}

func personaProblemList(ps []persona.Problem) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.File+" ("+p.Message+")")
	}
	return out
}

// protocolProblems tells the owner of protocol files a load skipped.
func (d *Daemon) protocolProblems(ps []protocols.Problem) {
	d.problemFiles("protocol", protocolProblemList(ps))
}

// personaProblems tells the owner of persona files a load skipped.
func (d *Daemon) personaProblems(ps []persona.Problem) {
	d.problemFiles("persona", personaProblemList(ps))
}

// problemFiles tells the owner, in the background, of the files of one kind
// they haven't heard about. Only a running twin tells: at startup the
// Health check does, once the channels are up.
func (d *Daemon) problemFiles(kind string, problems []string) {
	if d.runCtx == nil || d.store == nil {
		return
	}
	go d.noticeProblemFiles(kind, problems)
}

// noticeProblemFiles is problemFiles's work: one message for what is new,
// and the record of what the owner knows, kept to the problems that remain.
func (d *Daemon) noticeProblemFiles(kind string, problems []string) {
	problemMu.Lock()
	defer problemMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	told := map[string][]string{}
	if raw, _ := d.store.Get(ctx, problemFilesKey); raw != "" {
		_ = json.Unmarshal([]byte(raw), &told)
	}
	var fresh, known []string
	for _, p := range problems {
		if slices.Contains(told[kind], p) {
			known = append(known, p)
		} else {
			fresh = append(fresh, p)
		}
	}
	if len(fresh) > 0 && d.tellProblemFiles(ctx, kind, fresh) {
		known = append(known, fresh...)
	}
	if slices.Equal(known, told[kind]) {
		return
	}
	if len(known) == 0 {
		delete(told, kind)
	} else {
		told[kind] = known
	}
	b, _ := json.Marshal(told)
	if err := d.store.Set(ctx, problemFilesKey, string(b)); err != nil {
		d.log.Warn("remember problem files", "err", err)
	}
}

// tellProblemFiles sends the notice, reporting whether it went out, or had
// nowhere to go (then the Health page tells).
func (d *Daemon) tellProblemFiles(ctx context.Context, kind string, fresh []string) bool {
	word := map[string]string{"protocol": "routine"}[kind] // the files are protocols; people know them as routines
	if word == "" {
		word = kind
	}
	text := fmt.Sprintf("I skipped a %s file I couldn't use: %s. Fix it, then say /reload.", word, fresh[0])
	if len(fresh) > 1 {
		text = fmt.Sprintf("I skipped %d %s files I couldn't use: %s. Fix them, then say /reload.", len(fresh), word, strings.Join(fresh, "; "))
	}
	if owner := d.ownerChatKey(); owner != "" {
		if err := d.Notify(ctx, owner, text); err != nil {
			d.log.Warn("tell owner about skipped files", "err", err)
			return false
		}
		return true
	}
	if err := desktopNotify(d.Config().Name, text); err != nil {
		// No chat and no desktop (a headless host): the Health page is the
		// only place to tell, so count it told rather than try every round.
		d.log.Warn("tell owner about skipped files: no chat or desktop to tell; see the Health page", "err", err)
	}
	return true
}

// filesCheck is the Health page's line for protocol and persona files the
// twin skipped. A running twin also tells the owner of new ones here, which
// covers the files found at startup, before the channels were up.
func (d *Daemon) filesCheck() health.Check {
	return health.Func("files", "Routine and persona files", func(context.Context) (health.State, string, string) {
		c := d.Config()
		_, pskip := protocols.LoadAll(c.ProtocolsDir)
		_, rskip := persona.LoadAll(config.Home(), c.ProtocolsDir)
		protos, personas := protocolProblemList(pskip), personaProblemList(rskip)
		if d.runCtx != nil && d.store != nil {
			go func() { // a notice can take a while; the health round doesn't wait
				d.noticeProblemFiles("protocol", protos)
				d.noticeProblemFiles("persona", personas)
			}()
		}
		all := append(protos, personas...)
		switch len(all) {
		case 0:
			return health.OK, "all read", ""
		case 1:
			return health.Warn, "skipped " + all[0], "fix or remove it, then say /reload"
		}
		return health.Warn, fmt.Sprintf("skipped %d files: %s", len(all), strings.Join(all, "; ")), "fix or remove them, then say /reload"
	}, nil)
}
