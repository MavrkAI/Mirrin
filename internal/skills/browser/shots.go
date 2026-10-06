package browser

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Screenshots show signed-in pages (a bank, a mailbox, a health portal), so
// they are kept only as long as they are useful: a couple of weeks, within a
// size cap, and not after the conversation that took them is cleared.
const (
	defaultKeepDays = 14
	defaultMaxMB    = 200
	// keepNewest are never removed for size: an approval being decided may
	// be showing one of them.
	keepNewest = 10
	// pruneEvery spaces out clean-ups as screenshots are taken.
	pruneEvery = 10 * time.Minute
)

// reShot matches the names the browser gives its screenshots (and only
// those: nothing else in the data directory is ever removed).
var reShot = regexp.MustCompile(`^(browser|screenshot)-[0-9-]+\.png$`)

// retention is how long screenshots are kept and how much space they may
// take; 0 means no limit of that kind.
func (s *Session) retention() (time.Duration, int64) {
	days, mb := s.cfg.KeepScreenshotsDays, s.cfg.ScreenshotsMaxMB
	if days == 0 {
		days = defaultKeepDays
	}
	if mb == 0 {
		mb = defaultMaxMB
	}
	var age time.Duration
	var size int64
	if days > 0 {
		age = time.Duration(days) * 24 * time.Hour
	}
	if mb > 0 {
		size = int64(mb) << 20
	}
	return age, size
}

// pruneShots clears old screenshots, at most every pruneEvery unless now.
func (s *Session) pruneShots(now bool) {
	s.mu.Lock()
	if !now && time.Since(s.pruned) < pruneEvery {
		s.mu.Unlock()
		return
	}
	s.pruned = time.Now()
	s.mu.Unlock()
	age, size := s.retention()
	if n := pruneShots(s.dataDir, age, size, time.Now()); n > 0 {
		s.log.Info("old browser screenshots removed", "count", n)
	}
}

type shotFile struct {
	path string
	size int64
	mod  time.Time
}

// ownShots lists the browser's screenshots in dataDir, newest first.
func ownShots(dataDir string) []shotFile {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil
	}
	var out []shotFile
	for _, e := range entries {
		if !e.Type().IsRegular() || !reShot.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, shotFile{path: filepath.Join(dataDir, e.Name()), size: info.Size(), mod: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
	return out
}

// pruneShots removes screenshots older than maxAge, then the oldest until the
// rest fit in maxBytes (sparing the newest few). Zero means no limit.
func pruneShots(dataDir string, maxAge time.Duration, maxBytes int64, now time.Time) int {
	removed := 0
	var total int64
	for i, f := range ownShots(dataDir) {
		old := maxAge > 0 && now.Sub(f.mod) > maxAge
		over := maxBytes > 0 && i >= keepNewest && total+f.size > maxBytes
		if old || over {
			if os.Remove(f.path) == nil {
				removed++
			}
			continue
		}
		total += f.size
	}
	return removed
}

// ForgetScreenshots removes the browser's screenshots in dataDir that texts
// mention (a "[[image:…]]" marker, or a path the twin passed on). Only the
// browser's own screenshot files directly in dataDir are touched. It returns
// how many were removed.
func ForgetScreenshots(dataDir string, texts ...string) int {
	if dataDir == "" {
		return 0
	}
	dir := filepath.Clean(dataDir)
	re := regexp.MustCompile(regexp.QuoteMeta(dir+string(filepath.Separator)) + `((?:browser|screenshot)-[0-9-]+\.png)`)
	seen := map[string]bool{}
	removed := 0
	for _, t := range texts {
		for _, m := range re.FindAllStringSubmatch(t, -1) {
			p := filepath.Join(dir, m[1])
			if seen[p] {
				continue
			}
			seen[p] = true
			if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
				continue
			}
			if os.Remove(p) == nil {
				removed++
			}
		}
	}
	return removed
}

// History is where a conversation's messages are kept (memory.Store).
type History interface {
	History(ctx context.Context, chatKey string, n int) ([]llm.Message, error)
}

// ForgetConversationScreenshots removes the screenshots a conversation
// showed, for when it is cleared: call it before the messages go.
func ForgetConversationScreenshots(ctx context.Context, h History, chatKey, dataDir string) int {
	msgs, err := h.History(ctx, chatKey, 1<<30)
	if err != nil {
		return 0
	}
	var texts []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			texts = append(texts, b.Text)
			if len(b.Input) > 0 {
				texts = append(texts, string(b.Input))
			}
		}
	}
	return ForgetScreenshots(dataDir, texts...)
}
