package watch

import (
	"context"
	"sort"
)

// Item is one new item a poll found: its key in the source (an email's uid
// or message id) and its line ("unread from Sarah: Lunch?"). Two emails
// can share a line (a monthly bill), never a key.
type Item struct {
	Key, Line string
}

// Claimer is given the new items a poll found in a source before the agent
// is asked about them, and returns the ones it left for the agent. It is for
// something that handles some kinds of news itself (a bill in the inbox),
// so the owner hears about each item once, from one place.
type Claimer func(ctx context.Context, source string, added []Item) (rest []Item)

// SetClaim sets what sees new items first; nil lets every item through.
func (w *Watcher) SetClaim(c Claimer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.claim = c
}

// claimed is what of added is left for the agent once the Claimer has
// taken what it handles. prev and cur are the snapshots added came from.
func (w *Watcher) claimed(ctx context.Context, source string, prev, cur map[string]string, added []string) []string {
	w.mu.Lock()
	c := w.claim
	w.mu.Unlock()
	if c == nil || len(added) == 0 {
		return added
	}
	rest := c(ctx, source, addedItems(prev, cur, added))
	if len(rest) == 0 {
		return nil
	}
	out := make([]string, len(rest))
	for i, it := range rest {
		out[i] = it.Line
	}
	return out
}

// addedItems gives each added line the key it has in cur. Diff and arrived
// list new items in key order, so each line takes the next unused new key
// with that line.
func addedItems(prev, cur map[string]string, added []string) []Item {
	var keys []string
	for k := range cur {
		if _, known := prev[k]; !known {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	used := make(map[string]bool, len(keys))
	out := make([]Item, len(added))
	for i, line := range added {
		out[i] = Item{Line: line}
		for _, k := range keys {
			if !used[k] && cur[k] == line {
				used[k] = true
				out[i].Key = k
				break
			}
		}
	}
	return out
}
