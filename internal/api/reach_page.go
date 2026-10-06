package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

// The Reach page (/reach, loopback only): the three ways a phone reaches
// the twin from anywhere, as three equal cards (Tailscale, your own relay,
// then the paid handle, always last), and a security panel for the one in
// use: the key it serves, the CAA pin, the certificate logs, what was sent
// to the linked service, and Verify now. Cloud appears here because the
// owner asked how to reach the twin from anywhere; the twin never brings it
// up anywhere else.

//go:embed reach.html
var reachHTML []byte

// Reach card kinds, in the order the page shows them.
const (
	ReachTailscale = "tailscale"
	ReachRelay     = "relay"
	ReachCloud     = "cloud"
)

// ReachCard is one way to reach the twin from anywhere.
type ReachCard struct {
	Kind string `json:"kind"`
	// Using: reach.mode is this one.
	Using bool `json:"using"`
	// Ready: it carries a phone now, at Address.
	Ready   bool   `json:"ready"`
	Address string `json:"address,omitempty"`
	// Detail says where it stands; Fix and FixURL what to do next.
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
	FixURL string `json:"fix_url,omitempty"`
	// The paid handle only. Available: this build can link (it trusts the
	// service's signing keys; until launch it doesn't, and the card says
	// it is coming). Linked: this machine is. State: active, grace,
	// expired or superseded; Until: when grace ends.
	Available bool      `json:"available,omitempty"`
	Linked    bool      `json:"linked,omitempty"`
	State     string    `json:"state,omitempty"`
	Until     time.Time `json:"until,omitzero"`
	// Relays are the relays the handle is carried by, and whether each is
	// connected.
	Relays []RelayTunnel `json:"relays,omitempty"`
}

// RelayTunnel is one relay's tunnel.
type RelayTunnel struct {
	ID     string `json:"id"`
	Online bool   `json:"online"`
}

// ReachSecurity is the security panel for the address in use.
type ReachSecurity struct {
	Host string `json:"host"`
	// Served and Next are SPKI pins: the key other devices see now, and
	// the one it will move to.
	Served string `json:"served,omitempty"`
	Next   string `json:"next,omitempty"`
	// AccountURI is the ACME account the CAA record must name; CAA says
	// whether it does, in words.
	AccountURI string `json:"account_uri,omitempty"`
	CAA        string `json:"caa,omitempty"`
	CAAOK      bool   `json:"caa_ok"`
	// CT is the certificate log watch.
	CT *CTStatus `json:"ct,omitempty"`
	// Egress sums what this machine sent to the service it is linked to
	// (nil when it isn't).
	Egress *EgressSummary `json:"egress,omitempty"`
}

// EgressSummary sums the egress ledger.
type EgressSummary struct {
	Requests int       `json:"requests"`
	Sent     int64     `json:"sent"`
	Received int64     `json:"received"`
	Last     time.Time `json:"last,omitzero"`
}

// ReachInfo is what the Reach page shows.
type ReachInfo struct {
	Mode     string         `json:"mode"`
	Cards    []ReachCard    `json:"cards"`
	Security *ReachSecurity `json:"security,omitempty"`
	// Moved is set while this machine stands by because another took over
	// its address.
	Moved string `json:"moved,omitempty"`
}

// ReachCheck is one line of Verify now.
type ReachCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"`
	Detail string `json:"detail"`
}

// ReachBackend is what the Reach page needs from the twin.
type ReachBackend interface {
	ReachInfo(ctx context.Context) ReachInfo
	// ReachVerify checks the address in use from outside, as a phone
	// would.
	ReachVerify(ctx context.Context) ([]ReachCheck, error)
	// ReachUseCloud switches reach to the paid handle, linking first when
	// this machine isn't: then it returns the checkout page to open, and
	// finishes by itself once it is paid.
	ReachUseCloud(ctx context.Context, handle string) (checkoutURL string, err error)
}

// orderCards puts the cards in the page's order: Tailscale, your relay,
// then the paid handle last, whatever order the backend gave.
func orderCards(cards []ReachCard) []ReachCard {
	rank := map[string]int{ReachTailscale: 0, ReachRelay: 1, ReachCloud: 2}
	out := slices.Clone(cards)
	slices.SortStableFunc(out, func(a, b ReachCard) int {
		ra, ok := rank[a.Kind]
		if !ok {
			ra = 1
		}
		rb, ok := rank[b.Kind]
		if !ok {
			rb = 1
		}
		return ra - rb
	})
	return out
}

// WithReachPage adds the Reach page.
func (s *Server) WithReachPage(b ReachBackend) *Server {
	if b == nil {
		return s
	}
	return s.Mount("reach-page", LoopbackOnly, func(mux *http.ServeMux, a Authz) {
		mux.HandleFunc("GET /reach", s.localPage(reachHTML))
		mux.HandleFunc("GET /reach/info", a.Local(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			info := b.ReachInfo(ctx)
			info.Cards = orderCards(info.Cards)
			if info.Cards == nil {
				info.Cards = []ReachCard{}
			}
			writeJSON(w, info)
		}))
		mux.HandleFunc("POST /reach/verify", a.Local(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
			defer cancel()
			checks, err := b.ReachVerify(ctx)
			if err != nil {
				s.fail(w, r, http.StatusConflict, apiError{Error: "reach", Message: plainSentence(err)})
				return
			}
			ok := true
			for _, c := range checks {
				ok = ok && c.OK
			}
			if checks == nil {
				checks = []ReachCheck{}
			}
			writeJSON(w, map[string]any{"ok": ok, "checks": checks})
		}))
		mux.HandleFunc("POST /reach/cloud", a.Local(func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Handle string `json:"handle"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in) != nil {
				s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_request", Message: "I couldn't read that.", Fix: "Reload the page and try again."})
				return
			}
			u, err := b.ReachUseCloud(r.Context(), strings.TrimSpace(strings.ToLower(in.Handle)))
			if err != nil {
				var h *HumanError
				if e, ok := err.(*HumanError); ok {
					h = e
				}
				if h != nil {
					s.fail(w, r, http.StatusBadRequest, apiError{Error: "reach", Message: h.Sentence, Fix: h.Fix})
					return
				}
				s.fail(w, r, http.StatusBadRequest, apiError{Error: "reach", Message: plainSentence(err)})
				return
			}
			writeJSON(w, map[string]string{"checkout_url": u})
		}))
	})
}
