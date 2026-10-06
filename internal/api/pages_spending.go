package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"math"
	"net/http"
	"time"
)

// The Spending page: the two limits on what the twin may pay, this month's
// total against them, and the payments it has made. Limits are a safety
// setting, so they change on this computer only.

//go:embed spending.html
var spendingHTML []byte

// SpendingInfo is what the page shows.
type SpendingInfo struct {
	Currency   string            `json:"currency"`
	PerAction  float64           `json:"per_action_limit"`
	Monthly    float64           `json:"monthly_limit"`
	MonthTotal float64           `json:"month_total"`
	Payments   []SpendingPayment `json:"payments"` // newest first
}

// SpendingPayment is one payment the twin made.
type SpendingPayment struct {
	At       time.Time `json:"at"`
	Amount   float64   `json:"amount"`
	Currency string    `json:"currency"`
	Merchant string    `json:"merchant,omitempty"`
	Purpose  string    `json:"purpose,omitempty"`
}

// SpendingBackend is a LocalPages backend that keeps a purse.
type SpendingBackend interface {
	Spending(ctx context.Context) SpendingInfo
	SetSpendingLimits(ctx context.Context, perAction, monthly float64) error
}

// maxLimit keeps a typo (an extra zero or two) from becoming a limit.
const maxLimit = 1_000_000

func (s *Server) spendingRoutes(mux *http.ServeMux, a Authz, b SpendingBackend) {
	mux.HandleFunc("GET /spending", s.localPage(spendingHTML))
	mux.HandleFunc("GET /spending/info", a.Local(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.Spending(r.Context()))
	}))
	mux.HandleFunc("POST /spending/limits", a.Local(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			PerAction *float64 `json:"per_action_limit"`
			Monthly   *float64 `json:"monthly_limit"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil || in.PerAction == nil || in.Monthly == nil {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_limits", Message: "Both limits are needed, as numbers."})
			return
		}
		per, month := *in.PerAction, *in.Monthly
		ok := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= maxLimit }
		switch {
		case !ok(per) || !ok(month):
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_limits", Message: "Limits are amounts from 0 to 1,000,000.", Fix: "0 means no limit."})
			return
		case month > 0 && per > month:
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_limits", Message: "A single payment's limit can't be more than the monthly limit."})
			return
		}
		if err := b.SetSpendingLimits(r.Context(), per, month); err != nil {
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "not_saved", Message: "The limits couldn't be saved: " + err.Error()})
			return
		}
		writeJSON(w, b.Spending(r.Context()))
	}))
}
