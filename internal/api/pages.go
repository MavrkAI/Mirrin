package api

import (
	"bytes"
	"context"
	_ "embed"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

//go:embed devices_add.html
var devicesAddHTML []byte

//go:embed devices.html
var devicesHTML []byte

//go:embed backup.html
var backupHTML []byte

//go:embed trust.html
var trustHTML []byte

//go:embed restore_review.html
var restoreReviewHTML []byte

// page serves one of the twin's HTML pages, with one cookie policy for all of
// them. The menu opens a page with ?token=<master key>: on the loopback
// listener that becomes a cookie holding this browser's own key (or keeps the
// one it has), and the address bar loses the token either way. alt, when
// given, answers requests that aren't a browser asking for the page (the
// health report's JSON).
func (s *Server) page(a authz, scope devices.Scope, body []byte, alt http.HandlerFunc) http.HandlerFunc {
	if !bytes.Contains(body, []byte(`<meta name="mirrin-name"`)) {
		body = injectHead(body, s.twinName()) // the page knows the twin's name before it has asked
	}
	return func(w http.ResponseWriter, r *http.Request) {
		l := listenerFrom(r.Context())
		if !a.reachable(l) {
			s.fail(w, r, http.StatusNotFound, s.notHere())
			return
		}
		if q := r.URL.Query(); q.Has("token") {
			tok := q.Get("token")
			q.Del("token")
			target := r.URL.Path
			if enc := q.Encode(); enc != "" {
				target += "?" + enc
			}
			if l.kind == kindLoopback && s.isMaster(tok, l) {
				if p, ok := s.authenticate(w, r, l); !ok || !p.Has(scope) {
					_, dtok, err := s.Devices().AddLocal(browserName(r))
					if err != nil {
						slog.Warn("couldn't save a browser's key", "err", err)
						s.fail(w, r, http.StatusInternalServerError, s.cantSave())
						return
					}
					s.setDeviceCookie(w, r, dtok)
				}
			}
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		if alt != nil && !wantsPage(r) {
			a.Require(scope, alt)(w, r)
			return
		}
		p, ok := s.authenticate(w, r, l)
		if !ok {
			s.unauthorized(w, r, p, l, a.exp == LoopbackOnly)
			return
		}
		if a.exp == LoopbackOnly && l.kind != kindLoopback && (p.Device == nil || !p.Device.Has(devices.Admin)) {
			s.fail(w, r, http.StatusNotFound, s.notHere())
			return
		}
		if !p.Has(scope) {
			s.fail(w, r, http.StatusForbidden, s.notAllowed(p))
			return
		}
		if p.cookie != "" {
			s.setDeviceCookie(w, r, p.cookie) // sliding: 180 days from the last visit
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		if l.kind != kindRemote { // remote listeners already send the full policy
			h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		}
		_, _ = w.Write(body)
	}
}

// cantSave is the answer when this browser's key can't be written down.
func (s *Server) cantSave() apiError {
	where := "the twin's data folder"
	if f := s.Devices().File(); f != "" {
		where = filepath.Dir(f)
	}
	return apiError{Error: "cant_save", Message: "I couldn't save this browser's key in " + where + ".",
		Fix: "Check there's free space and that the folder is writable, then open this page from " + s.twinName() + "'s menu again."}
}

// The owner's pages for other devices and safety (loopback only, §5.1 of
// docs/cloud-design.md): "Add your phone" (/devices/add), Devices
// (/devices/page), Backup (/backup), Trust (/trust) and the device review
// after a restore (/restore/review). They answer on this computer only,
// whatever reach.admin_remote says: they show pairing links, the backup
// words and what leaves the machine.

// Route kinds: how another device can reach the twin.
const (
	RouteCloud     = "cloud"     // the paid handle: listed only when reach.mode is cloud, never offered
	RouteRelay     = "relay"     // the owner's own mirrin-relay
	RouteTailscale = "tailscale" // the tailnet, with a ts.net certificate
	RouteFiles     = "files"     // the owner's own certificate files
)

// Route is one way a phone can reach the twin, and whether it works now.
// A route that doesn't says why (Problem) and what to do (Fix, FixURL).
type Route struct {
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url,omitempty"`
	Ready   bool   `json:"ready"`
	Problem string `json:"problem,omitempty"`
	Fix     string `json:"fix,omitempty"`
	FixURL  string `json:"fix_url,omitempty"`
}

// TailnetPeer is another device on the owner's tailnet, so the page can say
// whether their phone is on it yet. (The design calls it Peer; here that
// name is the caller of a request.)
type TailnetPeer struct {
	Name   string `json:"name"`
	OS     string `json:"os"`
	Online bool   `json:"online"`
}

// RoutesBackend says how other devices can reach the twin.
type RoutesBackend interface {
	Routes(ctx context.Context) []Route
	TailnetPeers(ctx context.Context) []TailnetPeer
}

// routeRank orders working routes by how well they serve a phone away from
// home: one that works from any network first.
func routeRank(kind string) int {
	switch kind {
	case RouteCloud:
		return 0
	case RouteRelay:
		return 1
	case RouteTailscale:
		return 2
	case RouteFiles:
		return 3
	}
	return 9
}

// problemRank orders the routes that don't work yet: Tailscale first (free,
// and the quickest to set up).
func problemRank(kind string) int {
	switch kind {
	case RouteTailscale:
		return 0
	case RouteRelay:
		return 1
	case RouteFiles:
		return 2
	}
	return 5
}

// BestRoute is the route the one QR code carries: the best one that works
// now, at an https address (a phone app needs one).
func BestRoute(routes []Route) (Route, bool) {
	best, ok := Route{}, false
	for _, r := range routes {
		if !r.Ready || !strings.HasPrefix(r.BaseURL, "https://") {
			continue
		}
		if !ok || routeRank(r.Kind) < routeRank(best.Kind) {
			best, ok = r, true
		}
	}
	return best, ok
}

// RouteProblems is the empty state: the free routes that don't work yet,
// Tailscale first, each with its fix.
func RouteProblems(routes []Route) []Route {
	var out []Route
	for _, r := range routes {
		if !r.Ready && r.Kind != RouteCloud {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return problemRank(out[i].Kind) < problemRank(out[j].Kind) })
	return out
}

// PageURL is a menu link to one of the twin's pages on this computer (path
// may carry a query). Like the other menu links it carries the master key,
// which the page swaps for the browser's own cookie.
func (s *Server) PageURL(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return s.localBase() + path + sep + "token=" + s.masterKey()
}

// LocalPages is what the device and safety pages need from the twin.
type LocalPages interface {
	RoutesBackend
	BackupBackend // pages_backup.go
	TrustBackend  // pages_trust.go
	ReviewBackend // pages_backup.go: the device review after a restore
}

// localPage serves one of these pages, on the loopback listener only.
func (s *Server) localPage(body []byte) http.HandlerFunc {
	page := s.page(authz{s: s, exp: LoopbackOnly}, devices.Admin, body, nil)
	return func(w http.ResponseWriter, r *http.Request) {
		if listenerFrom(r.Context()).kind != kindLoopback {
			s.fail(w, r, http.StatusNotFound, s.notHere())
			return
		}
		page(w, r)
	}
}

// WithLocalPages adds "Add your phone", Devices, Backup, Trust and the
// device review after a restore.
func (s *Server) WithLocalPages(b LocalPages) *Server {
	if b == nil {
		return s
	}
	s.pages = b
	return s.Mount("local-pages", LoopbackOnly, func(mux *http.ServeMux, a Authz) {
		mux.HandleFunc("GET /devices/add", s.localPage(devicesAddHTML))
		mux.HandleFunc("GET /devices/page", s.localPage(devicesHTML))
		mux.HandleFunc("GET /backup", s.localPage(backupHTML))
		mux.HandleFunc("GET /trust", s.localPage(trustPage())) // trust_visibility.go
		mux.HandleFunc("GET /restore/review", s.localPage(restoreReviewHTML))
		s.addRoutes(mux, a)    // pages_add.go
		s.backupRoutes(mux, a) // pages_backup.go
		s.trustRoutes(mux, a)  // pages_trust.go
		if sb, ok := b.(SpendingBackend); ok {
			s.spendingRoutes(mux, a, sb) // pages_spending.go
		}
	})
}
