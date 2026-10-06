package api

import (
	"context"
	"net/http"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// UsageBackend is the twin's model spending, for the presence screen.
type UsageBackend interface {
	Spend(ctx context.Context) (memory.Spend, error)
}

// WithUsage serves GET /usage: today's and this month's estimated model spend
// (US$; budget 0 means none is set), with every field always present.
func (s *Server) WithUsage(b UsageBackend) *Server { s.usage = b; return s }

// usageRoutes answers where the screen does, to anything that may view it.
func (s *Server) usageRoutes(mux *http.ServeMux, a authz) {
	mux.HandleFunc("GET /usage", a.Require(devices.View, func(w http.ResponseWriter, r *http.Request) {
		sp, err := s.usage.Spend(r.Context())
		if err != nil {
			// In the shape every refusal has ({error, message, fix}), which
			// the pages show as words.
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "usage_failed", Message: "I couldn't add up what the model has cost: " + err.Error() + ".",
				Fix: "Try again in a moment; `mirrin usage` on this computer shows it too."})
			return
		}
		writeJSON(w, sp)
	}))
}
