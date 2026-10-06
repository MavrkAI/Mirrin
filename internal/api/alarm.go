package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// Alarm is the certificate alarm's hold on remote listeners
// (docs/cloud-design.md §6.5; internal/reach implements it). While it is
// active, approvals from other devices are refused with 423 until the owner
// clears it on this computer or in their own chat. After it fires, every
// device's next response carries Clear-Site-Data, and the service worker
// version changes so installed apps fetch a fresh shell.
type Alarm interface {
	// Active reports whether remote approvals are paused.
	Active() bool
	// ClearSite reports, once per device, whether this response should
	// tell the browser to drop its cache and storage. deviceID is "" for a
	// request whose credential no longer authenticates.
	ClearSite(deviceID string) bool
	// Epoch changes each time the alarm fires; it is mixed into the
	// service worker's version.
	Epoch() string
}

// WithAlarm installs the alarm. Call it before the server starts.
func (s *Server) WithAlarm(a Alarm) *Server {
	s.alarm = a
	return s
}

func (s *Server) alarmEpoch() []byte {
	if s.alarm == nil {
		return nil
	}
	return []byte(s.alarm.Epoch())
}

// approvalRoute is anything that decides an approval or steps up for one.
func approvalRoute(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/stepup/"):
		return true
	case strings.HasPrefix(p, "/approvals/"), strings.HasPrefix(p, "/approve/"):
		return !safeMethod(r.Method)
	}
	return false
}

type alarmCtxKey struct{}

// ApprovalsPaused reports whether ctx is a request that arrived on a remote
// listener while the certificate alarm holds approvals from other devices.
// The routes that decide approvals answer 423 themselves; this is for the
// decisions made in words ("yes 12" by /message, or the model's
// resolve_approval), which the daemon refuses the same way. It reads the
// alarm live, so a stream that began before the alarm fired is held too.
func ApprovalsPaused(ctx context.Context) bool {
	a, ok := ctx.Value(alarmCtxKey{}).(Alarm)
	return ok && a.Active()
}

// WithAlarmHold notes on ctx that the request arrived on a remote listener
// while a may hold approvals, as alarmHold does: for code that hands a
// request on in process, and for tests.
func WithAlarmHold(ctx context.Context, a Alarm) context.Context {
	return context.WithValue(ctx, alarmCtxKey{}, a)
}

// alarmHold runs on remote listeners after authentication. It marks the
// request for ApprovalsPaused, and reports whether it answered the request
// itself.
func (s *Server) alarmHold(w http.ResponseWriter, r *http.Request, d devices.Device, valid bool) (*http.Request, bool) {
	a := s.alarm
	if a == nil {
		return r, false
	}
	r = r.WithContext(WithAlarmHold(r.Context(), a))
	// A device whose key the playbook rotated no longer authenticates; its
	// stale cookie still earns the clean slate ("" asks for that).
	if valid && a.ClearSite(d.ID) || !valid && carriesCookie(r) && a.ClearSite("") {
		w.Header().Set("Clear-Site-Data", `"cache", "storage"`)
	}
	if a.Active() && approvalRoute(r) {
		s.fail(w, r, http.StatusLocked, apiError{Error: "approvals_paused",
			Message: s.twinName() + " paused approvals from other devices: a certificate for its address turned up that it didn't ask for.",
			Fix:     "Approve on the computer " + s.twinName() + " runs on. When you've checked, clear the alarm there with `mirrin reach alarm clear`, or send /alarm clear in your own chat."})
		return r, true
	}
	return r, false
}
