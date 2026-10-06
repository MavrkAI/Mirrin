package api

import (
	"encoding/json"
	"net/http"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/push"
)

// WithPush adds scoped subscription management. Each subscription belongs to
// the authenticated device; a body can never select somebody else's device.
func (s *Server) WithPush(v *push.VAPID, store *push.Store, d *push.Dispatcher, resolve push.Resolver) *Server {
	s.Devices().OnChange(func(c devices.Change) {
		if c.Kind == devices.Revoked {
			_ = store.Delete(c.Device.ID, "")
		}
	})
	s.pushTest = func(id string) { d.Test(id, s.twinName()) }
	s.pushOn = func(id string) bool {
		for _, sub := range store.List() {
			if sub.DeviceID == id {
				return true
			}
		}
		return false
	}
	return s.Mount("push", Remote, func(m *http.ServeMux, a Authz) {
		require := func(h http.HandlerFunc) http.HandlerFunc {
			return a.Public(func(w http.ResponseWriter, r *http.Request) {
				p, ok := s.authenticate(w, r, listenerFrom(r.Context()))
				if !ok || p.Device == nil {
					http.Error(w, "Pair this device to enable notifications.", 401)
					return
				}
				if !p.Has(devices.View) && !p.Has(devices.Approve) {
					http.Error(w, "This device can't receive notifications. Pair it with screen access.", 403)
					return
				}
				// Authz.Require enforces the same origin and scoped context policy.
				scope := devices.View
				if !p.Has(scope) {
					scope = devices.Approve
				}
				a.Require(scope, h)(w, r)
			})
		}
		m.HandleFunc("GET /push/vapid", require(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]string{"key": v.PublicKey()}) }))
		m.HandleFunc("POST /push/subscribe", require(func(w http.ResponseWriter, r *http.Request) {
			var sub push.Subscription
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&sub) != nil || sub.Validate() != nil || push.AllowedEndpoint(sub.Endpoint, resolve) != nil {
				http.Error(w, "Couldn't use this subscription. Enable notifications again.", 400)
				return
			}
			sub.DeviceID = PeerFrom(r.Context()).Device.ID
			sub.Origin = "https://" + r.Host
			if r.TLS == nil {
				sub.Origin = "http://" + r.Host
			}
			if err := store.Put(sub); err != nil {
				http.Error(w, "Couldn't save notifications. Try again.", 500)
				return
			}
			if device, ok := s.Devices().Get(sub.DeviceID); !ok || device.Revoked() {
				_ = store.Delete(sub.DeviceID, "")
				http.Error(w, "Pair this device again.", 401)
				return
			}
			s.addProgress(sub.DeviceID, stepNotifications) // pages_add.go
			writeJSON(w, map[string]bool{"ok": true})
		}))
		// The app tells the twin its test notification arrived, so "Add
		// your phone" can light its last step.
		m.HandleFunc("POST /push/received", require(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Tag string `json:"tag"`
			}
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body)
			if body.Tag == "test" {
				s.addProgress(PeerFrom(r.Context()).Device.ID, stepTestReceived)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		m.HandleFunc("DELETE /push/subscribe", require(func(w http.ResponseWriter, r *http.Request) {
			if err := store.Delete(PeerFrom(r.Context()).Device.ID, ""); err != nil {
				http.Error(w, "Couldn't turn off notifications. Try again.", 500)
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
		}))
		m.HandleFunc("POST /push/test", require(func(w http.ResponseWriter, r *http.Request) {
			d.Test(PeerFrom(r.Context()).Device.ID, s.twinName())
			writeJSON(w, map[string]bool{"queued": true})
		}))
	})
}
