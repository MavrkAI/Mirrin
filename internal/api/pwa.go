package api

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

//go:embed pwa/*.html pwa/*.json pwa/pwa.js pwa/sw.js pwa/icons/*.png pwa/icons/*.svg
var pwaFiles embed.FS

// injectHead preserves the original screen byte for byte around its additions.
func injectHead(page []byte, twinName string) []byte {
	additions := `<link rel="manifest" href="/manifest.webmanifest"><meta name="theme-color" content="#07080b"><meta name="apple-mobile-web-app-capable" content="yes"><meta name="apple-mobile-web-app-title" content="` + html.EscapeString(twinName) + `"><meta name="mirrin-name" content="` + html.EscapeString(twinName) + `"><link rel="icon" href="/favicon.svg" type="image/svg+xml"><link rel="apple-touch-icon" href="/icons/apple-touch-icon-180.png"><script src="/pwa/pwa.js"></script>`
	return bytes.Replace(page, []byte("</head>"), []byte(additions+"</head>"), 1)
}

func pwaAsset(path string) []byte { b, _ := pwaFiles.ReadFile("pwa/" + path); return b }

// pwaShell contains only public assets, never API responses or credentials.
func (s *Server) pwaShell() map[string][]byte {
	return map[string][]byte{
		"/offline":                        bytes.ReplaceAll(pwaAsset("offline.html"), []byte("{{NAME}}"), []byte(html.EscapeString(s.twinName()))),
		"/pwa/pwa.js":                     pwaAsset("pwa.js"),
		"/icons/icon-192.png":             pwaAsset("icons/icon-192.png"),
		"/icons/icon-512.png":             pwaAsset("icons/icon-512.png"),
		"/icons/maskable-512.png":         pwaAsset("icons/maskable-512.png"),
		"/icons/apple-touch-icon-180.png": pwaAsset("icons/apple-touch-icon-180.png"),
		"/favicon.svg":                    pwaAsset("icons/favicon.svg"),
	}
}

func (s *Server) pwaVersion() (string, []byte) {
	hashes := map[string]string{}
	for path, b := range s.pwaShell() {
		h := sha256.Sum256(b)
		hashes[path] = hex.EncodeToString(h[:])
	}
	b, _ := json.Marshal(hashes)
	h := sha256.Sum256(append(append(append([]byte{}, b...), pwaAsset("sw.js")...), s.alarmEpoch()...)) // alarm.go bumps it
	return hex.EncodeToString(h[:12]), b
}

func (s *Server) pwaRoutes(mux *http.ServeMux, a Authz) {
	serve := func(b []byte, content string) http.HandlerFunc {
		return a.Public(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", content)
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Write(b)
		})
	}
	for path, b := range s.pwaShell() {
		content := "image/png"
		if path == "/offline" {
			content = "text/html; charset=utf-8"
		}
		if path == "/favicon.svg" {
			content = "image/svg+xml"
		}
		if path == "/pwa/pwa.js" {
			content = "text/javascript; charset=utf-8"
		}
		mux.HandleFunc("GET "+path, serve(b, content))
	}
	mux.HandleFunc("GET /start", serve(pwaAsset("start.html"), "text/html; charset=utf-8"))
	mux.HandleFunc("GET /characters.js", serve(charactersJS, "text/javascript; charset=utf-8")) // screen.go
	mux.HandleFunc("GET /manifest.webmanifest", a.Public(func(w http.ResponseWriter, r *http.Request) {
		start := "/ui"
		if t := r.URL.Query().Get("t"); validTicket(t) {
			start = "/start#t=" + url.QueryEscape(t)
		}
		name, _ := json.Marshal(s.twinName())
		target, _ := json.Marshal(start)
		b := []byte(strings.NewReplacer("{{NAME}}", string(name), "{{START}}", string(target)).Replace(string(pwaAsset("manifest.tmpl.json"))))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Write(b)
	}))
	mux.HandleFunc("GET /sw.js", a.Public(func(w http.ResponseWriter, r *http.Request) {
		version, hashes := s.pwaVersion()
		b := bytes.ReplaceAll(pwaAsset("sw.js"), []byte("__VERSION__"), []byte(version))
		b = bytes.ReplaceAll(b, []byte("__HASHES__"), hashes)
		w.Header().Set("Service-Worker-Allowed", "/")
		serve(b, "text/javascript; charset=utf-8")(w, r)
	}))
	mux.HandleFunc("GET /pwa/config.json", a.Require(devices.View, func(w http.ResponseWriter, r *http.Request) {
		v, _ := s.pwaVersion()
		urls := []string{}
		if s.statusURLs != nil {
			urls = append(urls, s.statusURLs()...)
		}
		writeJSON(w, map[string]any{"twin": s.twinName(), "version": v, "status_urls": urls})
	}))
}

// WithStatusURLs gives paired phones each relay's status endpoint for this
// twin, with its status key (in /pwa/config.json), so the offline page can
// say "asleep since 14:02" when the twin can't be reached. f is asked on
// every request; it returns nothing when no relay carries the twin.
func (s *Server) WithStatusURLs(f func() []string) *Server {
	s.statusURLs = f
	return s
}

func validTicket(t string) bool {
	if !strings.HasPrefix(t, "it_") || len(t) != 27 {
		return false
	}
	for _, c := range t[3:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// pwaHandler extends the pairing page at serve time, without changing its
// owner's source. The early script retains the ticket from the claim response.
func (s *Server) pwaHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WP-06's focused page takes precedence when mounted. Until then,
		// notification taps focus the existing authenticated approval card.
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/approve/") {
			id := strings.TrimPrefix(r.URL.Path, "/approve/")
			if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > 0 {
				if mux, ok := next.(*http.ServeMux); ok {
					if _, pattern := mux.Handler(r); pattern == "" {
						http.Redirect(w, r, "/ui#approval="+strconv.FormatInt(n, 10), http.StatusFound)
						return
					}
				}
			}
		}
		if r.URL.Path == "/pair/ticket" && r.Method == http.MethodPost && carriesCookie(r) {
			http.Error(w, "This app already has a pairing cookie. If it no longer works, remove the Home Screen app, pair again on your computer, and add it again.", http.StatusConflict)
			return
		}
		if r.URL.Path == "/pair" && (r.Method == "GET" || r.Method == "HEAD") {
			next.ServeHTTP(&pwaPairWriter{ResponseWriter: w, name: s.twinName()}, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type pwaPairWriter struct {
	http.ResponseWriter
	name string
}

func (w *pwaPairWriter) Write(b []byte) (int, error) {
	n := len(b)
	_, err := w.ResponseWriter.Write(injectHead(b, w.name))
	return n, err
}
