package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/stepup"
)

// Step-up: a paired device proves with a passkey (Face ID, a fingerprint)
// that its owner is holding it, before it approves anything that can't
// easily be undone (docs/cloud-design.md §8). This computer never needs
// one, and neither does the owner's "yes 12" in their own chat app.

type stepUpState struct {
	v     *stepup.Verifier
	level func() string // reach.step_up
}

// WithStepUp turns on passkeys for approvals from other devices, checked by
// v, at level (reach.step_up, read on each request). It adds the focused
// approval page (/approve/{id}), its data (/approvals/{id}), and passkey
// enrolment (/stepup/*). Without it, another device can still approve what
// doesn't need a passkey, and is refused what does.
func (s *Server) WithStepUp(v *stepup.Verifier, level func() string) *Server {
	if v == nil {
		return s
	}
	if level == nil {
		level = func() string { return "" }
	}
	v.SetName(s.twinName())
	s.stepUp = &stepUpState{v: v, level: level}
	s.Mount("stepup", Remote, func(mux *http.ServeMux, a Authz) {
		s.approveRoutes(mux) // approve.go
		mux.HandleFunc("GET /stepup/status", a.Require(devices.View, s.stepUpStatus))
		mux.HandleFunc("POST /stepup/register/begin", a.Require(devices.Approve, s.registerBegin))
		mux.HandleFunc("POST /stepup/register/finish", a.Require(devices.Approve, s.registerFinish))
	})
	s.Mount("stepup-owner", LoopbackOnly, func(mux *http.ServeMux, a Authz) {
		mux.HandleFunc("POST /stepup/allow", a.Local(s.allowEnrolment))
	})
	return s
}

// StepUp is the verifier WithStepUp gave, or nil.
func (s *Server) StepUp() *stepup.Verifier {
	if s.stepUp == nil {
		return nil
	}
	return s.stepUp.v
}

func (s *Server) stepUpLevel() stepup.Level {
	if s.stepUp == nil {
		return stepup.Dangerous
	}
	return stepup.ParseLevel(s.stepUp.level())
}

// rpFor is the relying party for a request, or false where passkeys can't
// work: this computer (which never needs one), and the old plain-HTTP
// listener (a passkey needs HTTPS and a name).
func rpFor(r *http.Request) (stepup.RP, bool) {
	if listenerFrom(r.Context()).kind != kindRemote {
		return stepup.RP{}, false
	}
	return stepup.RPFor(r.Host, r.TLS != nil), true
}

// passkeyState says whether this device has a passkey here, and what would
// let it enrol one now ("" for nothing).
func (s *Server) passkeyState(r *http.Request, p Peer) (bool, string) {
	rp, ok := rpFor(r)
	if !ok || p.Device == nil || s.stepUp == nil {
		return false, ""
	}
	has := s.stepUp.v.HasPasskey(*p.Device, rp)
	g, err := s.stepUp.v.Grant(*p.Device, rp)
	switch {
	case err == nil:
		return has, string(g.Kind)
	case errors.Is(err, stepup.ErrNeedProof):
		return has, string(stepup.GrantPasskey)
	}
	return has, ""
}

// stepUpApproval runs the passkey check for deciding det from another
// device. It reports whether the decision may go ahead; if not, it has
// answered: 428 with the check to sign, or why not.
func (s *Server) stepUpApproval(w http.ResponseWriter, r *http.Request, det ApprovalDetail, decision string) bool {
	p := PeerFrom(r.Context())
	rp, ok := rpFor(r)
	if !ok || p.Device == nil || s.stepUp == nil {
		s.passkeyRequired(w, r, det.ID)
		return false
	}
	v := s.stepUp.v
	if sid := r.Header.Get(stepup.Header); sid != "" {
		err := v.FinishApproval(*p.Device, sid, det.ID, decision, det.Input, http.MaxBytesReader(w, r.Body, 64<<10))
		if err == nil {
			return true
		}
		s.stepUpFailed(w, r, err, det.ID)
		return false
	}
	if !v.HasPasskey(*p.Device, rp) {
		s.passkeyRequired(w, r, det.ID)
		return false
	}
	opts, sid, err := v.BeginApproval(rp, *p.Device, det.ID, decision, det.Input)
	if err != nil {
		s.stepUpFailed(w, r, err, det.ID)
		return false
	}
	s.challenge(w, opts, sid)
	return false
}

// challenge is 428 {stepup, session}: sign this, then send the same
// request again with Mirrin-Stepup: session and the result as its body.
func (s *Server) challenge(w http.ResponseWriter, opts json.RawMessage, sid string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPreconditionRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{"stepup": opts, "session": sid, "message": "Confirm it's you with Face ID or your passkey."})
}

// stepUpFailed answers a check that didn't pass.
func (s *Server) stepUpFailed(w http.ResponseWriter, r *http.Request, err error, id int64) {
	switch {
	case errors.Is(err, stepup.ErrSessionUsed):
		s.refuse(w, r, http.StatusConflict, "stepup_used", "That check was already used, so I didn't act on it again.", "Try again from the request.")
	case errors.Is(err, stepup.ErrSessionExpired), errors.Is(err, stepup.ErrSessionUnknown):
		s.refuse(w, r, http.StatusForbidden, "stepup_expired", "That check took too long, so I didn't act on it.", "Try again, and confirm within two minutes.")
	case errors.Is(err, stepup.ErrNoPasskey), errors.Is(err, stepup.ErrHost):
		s.passkeyRequired(w, r, id)
	case errors.Is(err, stepup.ErrRevoked):
		s.fail(w, r, http.StatusUnauthorized, s.notPaired(listenerFrom(r.Context()), false))
	case errors.Is(err, stepup.ErrMismatch), errors.Is(err, stepup.ErrAssertion):
		slog.Warn("a passkey check didn't pass", "device", deviceID(r), "err", err)
		s.refuse(w, r, http.StatusForbidden, "stepup_failed", "That passkey check didn't match this request, so I didn't act on it.", "Try again from the request itself.")
	default:
		slog.Warn("passkey check", "err", err)
		s.refuse(w, r, http.StatusInternalServerError, "stepup_error", "I couldn't check the passkey.", "Try again in a moment.")
	}
}

func deviceID(r *http.Request) string {
	if p := PeerFrom(r.Context()); p.Device != nil {
		return p.Device.ID
	}
	return ""
}

// stepUpStatus is GET /stepup/status: whether this device has a passkey
// here and could enrol one now, for the page to offer "Set up Face ID".
func (s *Server) stepUpStatus(w http.ResponseWriter, r *http.Request) {
	p := PeerFrom(r.Context())
	has, can := s.passkeyState(r, p)
	_, possible := rpFor(r)
	writeJSON(w, map[string]any{"level": s.stepUpLevel(), "passkey": has, "can_enrol": can, "possible": possible && p.Device != nil})
}

// registerBegin is POST /stepup/register/begin: the options to create a
// passkey with, if something allows it (just after pairing, the owner's
// say-so, or, retried after a 428, an existing passkey's signature).
func (s *Server) registerBegin(w http.ResponseWriter, r *http.Request) {
	p := PeerFrom(r.Context())
	rp, ok := rpFor(r)
	if !ok || p.Device == nil {
		s.refuse(w, r, http.StatusBadRequest, "passkey_unavailable", "Passkeys work only at "+s.twinName()+"'s secure address, from a paired device.", "Open "+s.twinName()+" from its app on your phone and try there.")
		return
	}
	v := s.stepUp.v
	var g stepup.EnrolGrant
	var err error
	if sid := r.Header.Get(stepup.Header); sid != "" {
		g, err = v.FinishEnrolProof(*p.Device, sid, http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			s.stepUpFailed(w, r, err, 0)
			return
		}
	} else {
		g, err = v.Grant(*p.Device, rp)
		switch {
		case errors.Is(err, stepup.ErrNeedProof):
			opts, sid, err := v.BeginEnrolProof(rp, *p.Device)
			if err != nil {
				s.stepUpFailed(w, r, err, 0)
				return
			}
			s.challenge(w, opts, sid)
			return
		case errors.Is(err, stepup.ErrNoGrant):
			s.enrolNotAllowed(w, r, *p.Device)
			return
		case err != nil:
			s.stepUpFailed(w, r, err, 0)
			return
		}
	}
	opts, sid, err := v.BeginRegistration(rp, *p.Device, g)
	if errors.Is(err, stepup.ErrNoGrant) {
		s.enrolNotAllowed(w, r, *p.Device)
		return
	}
	if err != nil {
		s.stepUpFailed(w, r, err, 0)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"publicKey": opts, "session": sid})
}

func (s *Server) enrolNotAllowed(w http.ResponseWriter, r *http.Request, d devices.Device) {
	id := d.ID
	if len(id) > 8 {
		id = id[:8]
	}
	s.refuse(w, r, http.StatusForbidden, "enrol_not_allowed",
		"To keep a stolen phone from adding its own Face ID, a passkey can be set up only just after pairing, or once you say so.",
		"Send /passkey "+id+" to "+s.twinName()+" from your own chat app or from `mirrin chat` on "+computer()+", then try again within 15 minutes.")
}

// registerFinish is POST /stepup/register/finish, with Mirrin-Stepup: the
// session from begin, and the new credential as the body.
func (s *Server) registerFinish(w http.ResponseWriter, r *http.Request) {
	p := PeerFrom(r.Context())
	sid := r.Header.Get(stepup.Header)
	if _, ok := rpFor(r); !ok || p.Device == nil || sid == "" {
		s.refuse(w, r, http.StatusBadRequest, "passkey_unavailable", "Start setting up the passkey again.", "")
		return
	}
	pk, err := s.stepUp.v.FinishRegistration(*p.Device, sid, http.MaxBytesReader(w, r.Body, 64<<10))
	switch {
	case errors.Is(err, stepup.ErrNoGrant):
		s.enrolNotAllowed(w, r, *p.Device)
		return
	case err != nil:
		s.stepUpFailed(w, r, err, 0)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "passkey": map[string]any{"id": pk.ID, "created": pk.Created, "rp_id": pk.RPID}})
}

// allowEnrolment is POST /stepup/allow {"device": id or its first
// characters}, on this computer: the owner lets that device enrol a
// passkey in the next 15 minutes.
func (s *Server) allowEnrolment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device string `json:"device"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	d, err := s.Devices().Find(req.Device)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, apiError{Error: "no_device", Message: "I don't have a paired device like that.", Fix: "Open Devices from the menu bar to see them."})
		return
	}
	if err := s.stepUp.v.AllowEnrolment(d.ID); err != nil {
		s.fail(w, r, http.StatusNotFound, apiError{Error: "no_device", Message: "I don't have a paired device like that.", Fix: "Open Devices from the menu bar to see them."})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "device": d.Public(), "minutes": int(stepup.OwnerGrantTTL.Minutes())})
}
