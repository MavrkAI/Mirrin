package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// GoogleClientReplacer is an AccountsBackend that can set the saved Google
// OAuth client aside, so the owner can paste a different one.
type GoogleClientReplacer interface {
	UseDifferentGoogleClient(ctx context.Context) error
}

// ErrGoogleSignOutPending is wrapped by UseDifferentGoogleClient when the
// client was set aside but the Google sign-in couldn't be removed: the
// replace worked, and the sign-out is left to try again.
var ErrGoogleSignOutPending = errors.New("the old client was set aside, but its Google sign-in couldn't be removed yet")

// googleClientRoutes adds "Use a different client" to Accounts.
func (s *Server) googleClientRoutes(mux *http.ServeMux, a authz) {
	g, ok := s.accounts.(GoogleClientReplacer)
	if !ok {
		return
	}
	mux.HandleFunc("POST /accounts/google/client/replace", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		err := g.UseDifferentGoogleClient(r.Context())
		if errors.Is(err, ErrGoogleSignOutPending) {
			writeJSON(w, map[string]string{"replaced": "google", "note": "The old client is set aside. Its Google sign-in couldn't be removed yet; press Disconnect to try again."})
			return
		}
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "not_replaced", Message: "I couldn't set the old client aside: " + err.Error() + ".",
				Fix: "Check the twin's data folder is writable, then try again."})
			return
		}
		writeJSON(w, map[string]string{"replaced": "google"})
	}))
}
