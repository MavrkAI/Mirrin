package identity

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// reconnectPack clones a pack that arrived as a copy again from its source
// (protocols.ReconnectPack); tests replace it.
var reconnectPack = protocols.ReconnectPack

// fetchIndex reads a pack index (protocols.FetchRegistry); tests replace it.
var fetchIndex = protocols.FetchRegistry

// reconnectWait bounds all the clones of one import together, so an archive
// naming many servers that can't be reached never keeps the twin down long.
const reconnectWait = 2 * time.Minute

// reconnectPacks clones every pack that arrived as a copy again from its
// recorded, already validated source at its recorded commit, so it updates
// here as it did on the old machine. A registry pack is looked up again in
// this machine's index (index, "" for the default), never in one the
// archive names: it is reconnected only when that index lists it at the
// same repository, and then follows that index. A pack that can't be
// reached, or isn't listed here, stays a copy and remains in r.Packs, for
// the CLI to say how to reconnect it.
func reconnectPacks(protocolsDir, index string, r *Result) {
	if len(r.Packs) == 0 {
		return
	}
	if protocols.IsDefaultRegistry(index) {
		index = ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), reconnectWait)
	defer cancel()
	var reg *protocols.Registry
	var regErr error
	fetched := false
	var left []PackSource
	for _, p := range r.Packs {
		pv := p.provenance()
		if p.Registry != "" {
			if !fetched {
				reg, regErr = fetchIndex(ctx, index)
				fetched = true
			}
			if !listedHere(reg, regErr, p, index) {
				left = append(left, p)
				continue
			}
			pv.Index = index
		}
		if err := reconnectPack(ctx, protocolsDir, p.Dir, pv, p.Commit); err != nil {
			left = append(left, p)
			continue
		}
		r.Reconnected = append(r.Reconnected, p.Dir)
	}
	r.Packs = left
}

// listedHere reports whether p, a registry pack from an archive, may follow
// reg, this machine's index at index: the archive named the same index (or
// the default when this machine uses it), and reg lists p at its repository.
func listedHere(reg *protocols.Registry, err error, p PackSource, index string) bool {
	if err != nil || reg == nil {
		return false
	}
	from := p.Index
	if protocols.IsDefaultRegistry(from) {
		from = ""
	}
	if from != index {
		return false
	}
	entry, ok := reg.Find(p.Registry)
	return ok && protocols.SameRepo(entry.Repo, p.Repo)
}

// keepBackups is how many import backups (home/backups/<time>) are kept.
const keepBackups = 5

// backupName is the folder name backup.ensure makes: a time, and a count
// when two imports land in the same second.
var backupName = regexp.MustCompile(`^(\d{8}-\d{6})(?:-(\d+))?$`)

// nextBackupIndex is the count after the highest one used for a backup at
// ts, so a newer backup always sorts after older ones, even once pruning
// has removed the first of that second.
func nextBackupIndex(root, ts string) int {
	next := 1
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		m := backupName.FindStringSubmatch(e.Name())
		if m == nil || m[1] != ts {
			continue
		}
		n := 1
		if m[2] != "" {
			n, _ = strconv.Atoi(m[2])
		}
		if n >= next {
			next = n + 1
		}
	}
	return next
}

// pruneBackups removes all but the newest keepBackups import backups. Only
// folders named the way imports name them are touched.
func pruneBackups(home string) {
	root := filepath.Join(home, "backups")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type found struct {
		name, at string
		n        int
	}
	var all []found
	for _, e := range entries {
		m := backupName.FindStringSubmatch(e.Name())
		if !e.IsDir() || m == nil {
			continue
		}
		n := 1
		if m[2] != "" {
			n, _ = strconv.Atoi(m[2])
		}
		all = append(all, found{e.Name(), m[1], n})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].at != all[j].at {
			return all[i].at > all[j].at
		}
		return all[i].n > all[j].n
	})
	for i := keepBackups; i < len(all); i++ {
		_ = os.RemoveAll(filepath.Join(root, all[i].name))
	}
}
