package daemon

import (
	"context"

	"github.com/MavrkAI/Mirrin/internal/api"
)

// Why says what a reply drew on (api.WhyTeller), from the notes the agent
// keeps for the owner's replies (agent/why.go).
func (m memoryAdapter) Why(ctx context.Context, reply string) (api.Why, error) {
	w, ok, err := m.store.WhyFor(ctx, reply)
	if err != nil || !ok {
		return api.Why{}, err
	}
	out := api.Why{Known: true, Everything: w.Everything, Facts: make([]api.WhyFact, 0, len(w.Facts))}
	for _, f := range w.Facts {
		out.Facts = append(out.Facts, api.WhyFact{ID: f.ID, Subject: f.Subject, Content: f.Content, How: f.How})
	}
	return out, nil
}

var _ api.WhyTeller = memoryAdapter{}
