package daemon

import (
	"context"

	"github.com/MavrkAI/Mirrin/internal/api"
)

// FactsPage pages the memory page through every fact (api.FactPager), so a
// twin that knows more than a thousand things shows them all.
func (m memoryAdapter) FactsPage(ctx context.Context, offset, limit int, query string) ([]api.Fact, int, error) {
	fs, total, err := m.store.FactsPage(ctx, offset, limit, query)
	if err != nil {
		return nil, 0, err
	}
	out := make([]api.Fact, 0, len(fs))
	for _, f := range fs {
		out = append(out, api.Fact{ID: f.ID, Subject: f.Subject, Content: f.Content, Source: f.Source, CreatedAt: f.CreatedAt})
	}
	return out, total, nil
}

var _ api.FactPager = memoryAdapter{}
