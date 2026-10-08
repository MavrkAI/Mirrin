package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/stepup"
)

//go:embed approve.html
var approveHTML []byte

// ApprovalDetail is one approval as the focused page and step-up see it.
type ApprovalDetail struct {
	ID         int64     `json:"id"`
	Tool       string    `json:"tool"`
	Summary    string    `json:"summary"`
	What       string    `json:"what"`             // one line: what it will do
	Why        string    `json:"why,omitempty"`    // who or what asked for it
	Detail     string    `json:"detail,omitempty"` // a command or script, whole, as stored
	Risk       string    `json:"risk"`             // read, write or dangerous
	Status     string    `json:"status"`           // pending, approved, denied, superseded, expired
	DecidedBy  string    `json:"decided_by,omitempty"`
	At         time.Time `json:"at,omitzero"` // when it was decided
	CreatedAt  time.Time `json:"created_at,omitzero"`
	Screenshot string    `json:"screenshot,omitempty"` // a /screen/shot link, while it waits
	// Input is the stored input a passkey's challenge is bound to.
	Input json.RawMessage `json:"-"`
}

// ErrNoApproval is ApprovalBackend's answer for an id it doesn't have.
var ErrNoApproval = errors.New("no such approval")

// ApprovalBackend is what the focused page and step-up need beyond
// ScreenBackend. A screen backend that isn't one can't show the page, and
// never lets another device decide anything without a passkey.
type ApprovalBackend interface {
	ApprovalDetail(ctx context.Context, id int64) (ApprovalDetail, error)
	// DecideApprovalVia is DecideApproval, saying how the device proved
	// itself (method: "passkey"), for the approval and the audit log.
	DecideApprovalVia(ctx context.Context, id int64, approve bool, method string) (string, error)
}

func (s *Server) approvals() (ApprovalBackend, bool) {
	ab, ok := s.screen.(ApprovalBackend)
	return ab, ok
}

// decideApproval is POST /approvals/{id}/{decision}. This computer decides
// as it always has. Another device's decision may need a passkey first
// (reach.step_up): a 428 carries the check, and the same request, retried
// with the Mirrin-Stepup header and the signed check as its body, decides.
func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request, id int64, decision string) {
	ctx := r.Context()
	approve := decision == "approve"
	ab, known := s.approvals()
	if PeerFrom(ctx).Loopback {
		reply, err := s.screen.DecideApproval(ctx, id, approve)
		if err != nil {
			if known && s.alreadyDecided(w, r, ab, id) {
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"reply": reply})
		return
	}
	if !known {
		// Nothing says how risky it is: never let a yes through unchecked.
		if stepup.Required(s.stepUpLevel(), "", decision) {
			s.passkeyRequired(w, r, id)
			return
		}
		reply, err := s.screen.DecideApproval(ctx, id, approve)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"reply": reply})
		return
	}
	det, err := ab.ApprovalDetail(ctx, id)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, apiError{Error: "no_approval", Message: fmt.Sprintf("There's no request #%d.", id), Fix: "Open the screen to see what's waiting."})
		return
	}
	if det.Status != "pending" {
		s.conflict(w, r, det)
		return
	}
	method := "screen"
	// A step-up sent with a decision that doesn't need one is still checked:
	// a check made for something else is refused, never ignored.
	if stepup.Required(s.stepUpLevel(), det.Risk, decision) || r.Header.Get(stepup.Header) != "" {
		if !s.stepUpApproval(w, r, det, decision) {
			return
		}
		method = "passkey"
	}
	reply, err := ab.DecideApprovalVia(ctx, id, approve, method)
	if err != nil {
		if s.alreadyDecided(w, r, ab, id) {
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"reply": reply})
}

// alreadyDecided answers 409 when the approval has its outcome already.
func (s *Server) alreadyDecided(w http.ResponseWriter, r *http.Request, ab ApprovalBackend, id int64) bool {
	det, err := ab.ApprovalDetail(r.Context(), id)
	if err != nil || det.Status == "pending" {
		return false
	}
	s.conflict(w, r, det)
	return true
}

// conflict is 409 {decided_by, at}: what the page shows as "Already
// approved on Akshay's iPhone at 14:05".
func (s *Server) conflict(w http.ResponseWriter, r *http.Request, det ApprovalDetail) {
	on := decidedOn(det.DecidedBy)
	msg := fmt.Sprintf("#%d was already %s.", det.ID, statusWord(det.Status))
	if det.Status == "approved" || det.Status == "denied" {
		msg = fmt.Sprintf("Already %s on %s", statusWord(det.Status), on)
		if !det.At.IsZero() {
			msg += " at " + det.At.Local().Format("15:04")
		}
		msg += "."
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": "already_decided", "message": msg, "status": det.Status,
		"decided_by": det.DecidedBy, "decided_on": on, "at": det.At,
	})
}

func statusWord(status string) string {
	switch status {
	case "superseded":
		return "replaced by a newer request"
	case "expired":
		return "too old to answer"
	}
	return status
}

// decidedOn is where a decision was made, from its decider ("Akshay's
// iPhone [d1] (passkey, relay r2, 203.0.113.9)"): the device's name, the
// owner's chat app, or this computer.
func decidedOn(by string) string {
	name, how := by, ""
	if i := strings.Index(by, " ("); i >= 0 && strings.HasSuffix(by, ")") {
		name, how = by[:i], by[i+2:len(by)-1]
	}
	if i := strings.Index(name, " ["); i >= 0 {
		name = name[:i]
	}
	if name != "" && name != "the owner" {
		return name
	}
	parts := strings.Split(how, ", ")
	if len(parts) >= 2 && parts[0] == "channel" && parts[1] != "" {
		return strings.ToUpper(parts[1][:1]) + parts[1][1:]
	}
	if by == "" {
		return "another device"
	}
	return "the computer I run on"
}

// computer names the machine the twin runs on, for hints.
func computer() string {
	if runtime.GOOS == "darwin" {
		return "the Mac"
	}
	return "the computer I run on"
}

// passkeyRequired is 403 {error: passkey_required, hint}.
func (s *Server) passkeyRequired(w http.ResponseWriter, r *http.Request, id int64) {
	s.refuse(w, r, http.StatusForbidden, "passkey_required",
		"Approving this from another device needs Face ID or a passkey, and this device hasn't set one up here.",
		fmt.Sprintf("Approve on %s or reply yes %d.", computer(), id))
}

// refuse is fail with the step-up answers' shape: error, message, hint.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, status int, code, msg, hint string) {
	if wantsPage(r) {
		s.fail(w, r, status, apiError{Error: code, Message: msg, Fix: hint})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg, "hint": hint})
}

// approvalJSON is GET /approvals/{id}: the approval, and what this device
// may do about it.
func (s *Server) approvalJSON(w http.ResponseWriter, r *http.Request) {
	ab, ok := s.approvals()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if !ok || err != nil {
		s.fail(w, r, http.StatusNotFound, apiError{Error: "no_approval", Message: "There's no such request.", Fix: "Open the screen to see what's waiting."})
		return
	}
	det, err := ab.ApprovalDetail(r.Context(), id)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, apiError{Error: "no_approval", Message: fmt.Sprintf("There's no request #%d.", id), Fix: "Open the screen to see what's waiting."})
		return
	}
	p := PeerFrom(r.Context())
	out := struct {
		ApprovalDetail
		Name      string `json:"name"`
		DecidedOn string `json:"decided_on,omitempty"`
		CanDecide bool   `json:"can_decide"`
		StepUp    bool   `json:"step_up"`             // approving here needs a passkey
		Passkey   bool   `json:"passkey"`             // this device has one for this address
		CanEnrol  string `json:"can_enrol,omitempty"` // what would let it enrol one now
	}{ApprovalDetail: det, Name: s.twinName(), CanDecide: p.Has(devices.Approve)}
	if det.Status != "pending" {
		out.DecidedOn = decidedOn(det.DecidedBy)
	}
	if !p.Loopback {
		out.StepUp = stepup.Required(s.stepUpLevel(), det.Risk, "approve")
		out.Passkey, out.CanEnrol = s.passkeyState(r, p)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// approveRoutes are the focused approval page and its data.
func (s *Server) approveRoutes(mux *http.ServeMux) {
	a := authz{s, Remote}
	mux.HandleFunc("GET /approve/{id}", s.page(a, devices.View, injectHead(approveHTML, s.twinName()), nil))
	// What a request would do, and who asked, is for a device that may
	// chat or approve: a wall screen paired to look has only that something
	// waits, on /screen. The page itself holds nothing until it asks here.
	mux.HandleFunc("GET /approvals/{id}", a.requireAny([]devices.Scope{devices.Chat, devices.Approve}, s.approvalJSON))
}
