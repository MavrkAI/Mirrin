package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/qr"
)

// "Add your phone": the page on this computer shows one QR code for the best
// route (BestRoute) and lights each step as the phone takes it. The steps
// arrive over GET /devices/add/state, a server-sent event stream:
//
//	offer          a new QR code (again every OfferRefresh while the page is open)
//	no_route       no route works yet: the fixes, Tailscale first
//	scanned        the phone opened the link (POST /pair/seen)
//	claimed        the phone paired (POST /pair/claim)
//	claimed_other  another device paired with a code this page showed
//	               (a race with a refresh): shown with its own revoke
//	installed      its Home Screen app took its key (POST /pair/ticket)
//	notifications  it turned on notifications (POST /push/subscribe)
//	passkey        it set up Face ID or a passkey (devices.PasskeyAdded)
//	test_received  its test notification arrived (POST /push/received)
//	revoked        the owner said "That wasn't me"
//
// The test notification goes by itself once notifications and Face ID are
// both on, in either order, once per page.
//
// The code carries the pairing link, whose single-use secret is in the
// #fragment; never the master key or any device's key. Only the code on
// screen pairs: a new code withdraws the one before, and the rest are
// withdrawn once the page's phone pairs or the page closes.

// The steps, in order.
const (
	stepOffer         = "offer"
	stepNoRoute       = "no_route"
	stepScanned       = "scanned"
	stepClaimed       = "claimed"
	stepClaimedOther  = "claimed_other"
	stepInstalled     = "installed"
	stepNotifications = "notifications"
	stepPasskey       = "passkey"
	stepTestReceived  = "test_received"
	stepRevoked       = "revoked"
)

// AddSteps are the steps a phone takes, in the order the page shows them.
var AddSteps = []string{stepScanned, stepClaimed, stepInstalled, stepNotifications, stepPasskey, stepTestReceived}

// OfferRefresh is how often the page gets a fresh code while it is open: a
// minute before the last one expires (devices.OfferTTL).
const OfferRefresh = 9 * time.Minute

// routeRecheck is how often a page with no working route looks again.
const routeRecheck = 15 * time.Second

// AddEvent is one message on the stream.
type AddEvent struct {
	Step    string          `json:"step"`
	Device  *devices.Device `json:"device,omitempty"`
	Route   *Route          `json:"route,omitempty"`
	Routes  []Route         `json:"routes,omitempty"` // every route, for "How phones reach …"
	Peers   []TailnetPeer   `json:"peers,omitempty"`
	QR      string          `json:"qr,omitempty"`   // an SVG
	Link    string          `json:"link,omitempty"` // what the code says, to copy instead
	Expires time.Time       `json:"expires,omitzero"`
	Twin    string          `json:"twin,omitempty"`
}

// addHub holds the "Add your phone" pages open now.
type addHub struct {
	mu       sync.Mutex
	byOffer  map[string]*addSession
	byDevice map[string]*addSession
	// refresh and recheck replace OfferRefresh and routeRecheck (tests).
	refresh, recheck time.Duration
}

// addSession is one open page: the offers it showed and the phone that
// claimed one.
type addSession struct {
	events chan AddEvent
	mu     sync.Mutex
	secret map[string][32]byte // offer id → SHA-256 of its secret
	live   map[string]bool     // offers shown and not yet withdrawn
	device string
	done   map[string]bool
	tested bool // the automatic test notification went
}

func (h *addHub) open() *addSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byOffer == nil {
		h.byOffer, h.byDevice = map[string]*addSession{}, map[string]*addSession{}
	}
	return &addSession{events: make(chan AddEvent, 32), secret: map[string][32]byte{}, live: map[string]bool{}, done: map[string]bool{}}
}

func (h *addHub) close(a *addSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, x := range h.byOffer {
		if x == a {
			delete(h.byOffer, id)
		}
	}
	for id, x := range h.byDevice {
		if x == a {
			delete(h.byDevice, id)
		}
	}
}

func (h *addHub) remember(a *addSession, o devices.Offer) {
	h.mu.Lock()
	h.byOffer[o.ID] = a
	h.mu.Unlock()
	a.mu.Lock()
	a.secret[o.ID] = sha256.Sum256([]byte(o.Secret))
	a.live[o.ID] = true
	a.mu.Unlock()
}

// withdraw cancels the page's unclaimed offers, so a code it no longer
// shows (a photo of an older one, or one from a closed page) pairs nothing.
func (s *Server) withdraw(a *addSession) {
	a.mu.Lock()
	ids := make([]string, 0, len(a.live))
	for id := range a.live {
		ids = append(ids, id)
	}
	clear(a.live)
	a.mu.Unlock()
	for _, id := range ids {
		s.Devices().CancelOffer(id)
	}
}

func (h *addHub) session(offer, device string) *addSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	if offer != "" {
		return h.byOffer[offer]
	}
	return h.byDevice[device]
}

// emit sends a step once. Each step lights only when it happened: an
// Android phone may turn on notifications without installing the app, and
// the page mustn't claim it did. The one exception is a claim, which needs
// the link, so it lights "Scanned" too if the phone's note of that was
// lost.
func (a *addSession) emit(step string, d *devices.Device) {
	a.mu.Lock()
	var out []AddEvent
	if step == stepClaimed && !a.done[stepScanned] {
		a.done[stepScanned] = true
		out = append(out, AddEvent{Step: stepScanned})
	}
	if !a.done[step] {
		a.done[step] = true
		out = append(out, AddEvent{Step: step, Device: d})
	}
	a.mu.Unlock()
	for _, ev := range out {
		select {
		case a.events <- ev:
		default: // a page that stopped reading misses it; its reload starts over
		}
	}
}

// addPayload is what a code says: the pairing link for the route. Nothing
// else goes in it.
func addPayload(r Route, o devices.Offer, twin string) string {
	return PairLink(r.BaseURL, o, twin)
}

// addClaimed is told about every claim; one from an offer an open page
// showed lights "claimed" there, and withdraws the page's other codes. A
// second device that got in with another of the page's codes (a claim that
// raced a refresh) never takes the first one's place: the page shows it
// separately, with its own revoke.
func (s *Server) addClaimed(offer string, d devices.Device) {
	a := s.adds.session(offer, "")
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.live, offer)
	first := a.device == ""
	if first {
		a.device = d.ID
	}
	a.mu.Unlock()
	s.withdraw(a)
	if !first {
		select {
		case a.events <- AddEvent{Step: stepClaimedOther, Device: &d}:
		default:
		}
		return
	}
	s.adds.mu.Lock()
	s.adds.byDevice[d.ID] = a
	s.adds.mu.Unlock()
	a.emit(stepClaimed, &d)
}

// addProgress lights a step for a device an open page paired.
func (s *Server) addProgress(deviceID, step string) {
	a := s.adds.session("", deviceID)
	if a == nil {
		return
	}
	d, _ := s.Devices().Get(deviceID)
	a.emit(step, &d)
	if step != stepNotifications && step != stepPasskey {
		return
	}
	// Notifications and Face ID are both on, in whichever order: the test
	// notification follows by itself, once.
	if s.pushTest == nil || s.pushOn == nil || !s.pushOn(deviceID) {
		return
	}
	a.mu.Lock()
	send := a.done[stepNotifications] && a.done[stepPasskey] && !a.tested
	a.tested = a.tested || send
	a.mu.Unlock()
	if send {
		s.pushTest(deviceID)
	}
}

// addProgressChange follows the registry: a passkey enrolled, or the
// device cut off.
func (s *Server) addProgressChange(c devices.Change) {
	switch c.Kind {
	case devices.PasskeyAdded:
		s.addProgress(c.Device.ID, stepPasskey)
	case devices.Revoked:
		s.addProgress(c.Device.ID, stepRevoked)
	}
}

// pairSeen is POST /pair/seen: the pairing page on the phone says it has
// the link. The secret must match, so nobody else can light "Scanned"; the
// answer is the same either way.
func (s *Server) pairSeen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		O string `json:"o"`
		S string `json:"s"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req)
	w.WriteHeader(http.StatusNoContent)
	a := s.adds.session(req.O, "")
	if a == nil {
		return
	}
	got := sha256.Sum256([]byte(req.S))
	a.mu.Lock()
	want, ok := a.secret[req.O]
	ok = ok && a.live[req.O] // a withdrawn code lights nothing
	a.mu.Unlock()
	if ok && subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
		a.emit(stepScanned, nil)
	}
}

// notifying lists the devices with notifications on.
func (s *Server) notifying() []string {
	out := []string{}
	if s.pushOn == nil {
		return out
	}
	for _, d := range s.Devices().List() {
		if !d.Revoked() && s.pushOn(d.ID) {
			out = append(out, d.ID)
		}
	}
	return out
}

func (s *Server) addRoutes(mux *http.ServeMux, a Authz) {
	mux.HandleFunc("GET /devices/add/state", a.Local(s.addState))
	mux.HandleFunc("GET /devices/routes", a.Local(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		routes := s.pages.Routes(ctx)
		writeJSON(w, map[string]any{"routes": routes, "problems": RouteProblems(routes), "peers": s.pages.TailnetPeers(ctx), "best": bestOrNil(routes)})
	}))
	mux.HandleFunc("POST /devices/add/test", a.Local(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Device string `json:"device"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req) != nil || req.Device == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		d, ok := s.Devices().Get(req.Device)
		switch {
		case !ok || d.Revoked():
			s.fail(w, r, http.StatusNotFound, apiError{Error: "no_device", Message: "That device isn't paired any more.", Fix: "Scan a new code to pair it again."})
		case s.pushTest == nil:
			s.fail(w, r, http.StatusConflict, apiError{Error: "push_off", Message: "Notifications are turned off for " + s.twinName() + ".", Fix: "Turn them on in the settings file (menu bar → Open settings file): under push, set enabled to true. Then restart " + s.twinName() + "."})
		case s.pushOn == nil || !s.pushOn(d.ID):
			s.fail(w, r, http.StatusConflict, apiError{Error: "no_subscription", Message: "That phone hasn't turned on notifications yet.", Fix: "On the phone, open " + s.twinName() + " from its Home Screen and tap Enable notifications."})
		default:
			s.pushTest(d.ID)
			writeJSON(w, map[string]bool{"sent": true})
		}
	}))
}

func bestOrNil(routes []Route) *Route {
	if r, ok := BestRoute(routes); ok {
		return &r
	}
	return nil
}

// addState is the stream the "Add your phone" page listens to.
func (s *Server) addState(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	send := func(ev AddEvent) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	a := s.adds.open()
	defer func() {
		s.withdraw(a) // a closed page's codes pair nothing
		s.adds.close(a)
	}()
	refresh, recheck := s.adds.refresh, s.adds.recheck
	if refresh <= 0 {
		refresh = OfferRefresh
	}
	if recheck <= 0 {
		recheck = routeRecheck
	}
	ctx := r.Context()
	// offer shows a new code, or the fixes when no route works; it reports
	// whether a code is showing.
	offer := func() bool {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		routes := s.pages.Routes(rctx)
		peers := s.pages.TailnetPeers(rctx)
		cancel()
		s.withdraw(a) // only the code on screen pairs
		best, ok := BestRoute(routes)
		if !ok {
			send(AddEvent{Step: stepNoRoute, Routes: RouteProblems(routes), Peers: peers, Twin: s.twinName()})
			return false
		}
		o, err := s.Devices().NewOffer(devices.KindPWA, nil, 0)
		if err != nil {
			send(AddEvent{Step: stepNoRoute, Routes: []Route{{Kind: best.Kind, Problem: "I couldn't make a pairing code: " + err.Error() + ".", Fix: "Close this page and open it again from the menu."}}, Twin: s.twinName()})
			return false
		}
		s.adds.remember(a, o)
		link := addPayload(best, o, s.twinName())
		svg, err := qr.SVG(link, 280)
		if err != nil {
			send(AddEvent{Step: stepNoRoute, Routes: []Route{{Kind: best.Kind, Problem: "The pairing link is too long for a QR code.", Fix: "Use a shorter name for " + s.twinName() + ", or copy the link instead."}}, Link: link, Twin: s.twinName()})
			return false
		}
		send(AddEvent{Step: stepOffer, Route: &best, Routes: routes, Peers: peers, QR: string(svg), Link: link, Expires: o.Expires, Twin: s.twinName()})
		return true
	}
	showing := offer()
	next := time.NewTimer(recheck)
	if showing {
		next.Reset(refresh)
	}
	defer next.Stop()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-a.events:
			send(ev)
		case <-keepalive.C:
			fmt.Fprint(w, ": still here\n\n")
			fl.Flush()
		case <-next.C:
			a.mu.Lock()
			claimed := a.device != ""
			a.mu.Unlock()
			if claimed {
				continue // this page's phone is paired: no new code
			}
			if showing = offer(); showing {
				next.Reset(refresh)
			} else {
				next.Reset(recheck)
			}
		}
	}
}
