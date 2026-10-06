package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

// FactPager is a memory backend that can page through its facts, for twins
// that know more than one page holds.
type FactPager interface {
	// FactsPage returns facts oldest first from offset, at most limit, whose
	// subject or content contains query (any, when empty), and how many
	// match in all.
	FactsPage(ctx context.Context, offset, limit int, query string) ([]Fact, int, error)
}

// FactsPage is GET /memory/facts?offset=&limit=&q= (without them, the
// route answers every fact as a plain list, as it always has).
type FactsPage struct {
	Facts  []Fact `json:"facts"`
	Total  int    `json:"total"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

const factsPageMax = 1000

func factsPage(w http.ResponseWriter, r *http.Request, mem MemoryBackend) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > factsPageMax {
		limit = factsPageMax
	}
	query := strings.TrimSpace(q.Get("q"))
	out := FactsPage{Offset: offset, Limit: limit}
	var err error
	if p, ok := mem.(FactPager); ok {
		out.Facts, out.Total, err = p.FactsPage(r.Context(), offset, limit, query)
	} else {
		out.Facts, out.Total, err = pageFacts(r.Context(), mem, offset, limit, query)
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if out.Facts == nil {
		out.Facts = []Fact{}
	}
	writeJSON(w, out)
}

// pageFacts pages a backend that can only list everything.
func pageFacts(ctx context.Context, mem MemoryBackend, offset, limit int, query string) ([]Fact, int, error) {
	all, err := mem.Facts(ctx)
	if err != nil {
		return nil, 0, err
	}
	if query != "" {
		q := strings.ToLower(query)
		kept := all[:0:0]
		for _, f := range all {
			if strings.Contains(strings.ToLower(f.Subject), q) || strings.Contains(strings.ToLower(f.Content), q) {
				kept = append(kept, f)
			}
		}
		all = kept
	}
	total := len(all)
	if offset > total {
		offset = total
	}
	return all[offset:min(offset+limit, total)], total, nil
}
