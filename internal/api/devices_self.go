package api

import (
	"context"
	"net/http"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// selfRoutes lets a paired device cut off its own key, so a terminal that
// runs `mirrin disconnect` leaves nothing behind on the twin it was paired
// with.
func (s *Server) selfRoutes(mux *http.ServeMux, remote authz) {
	mux.HandleFunc("POST /devices/self/revoke", remote.Require(devices.View, func(w http.ResponseWriter, r *http.Request) {
		p := PeerFrom(r.Context())
		if p.Device == nil {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "not_a_device", Message: "This connection isn't a paired device, so there's no key of its own to remove.",
				Fix: "Remove devices from the Devices page on " + s.twinName() + "'s computer."})
			return
		}
		res, err := s.RevokeDevice(p.Device.ID)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "cant_revoke", Message: "I couldn't remove this device's key: " + err.Error() + "."})
			return
		}
		writeJSON(w, res)
	}))
}

// RevokeSelf asks the twin at t to cut off t's own key.
func RevokeSelf(ctx context.Context, t Target) error {
	return newClient(t).do(ctx, http.MethodPost, "/devices/self/revoke", nil, nil)
}
