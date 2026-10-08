// Package api is the twin's HTTP surface: the pages its menu opens, the
// presence screen, and the JSON the terminal, voice and phone clients use.
//
// Trust comes from which listener a request arrived on (auth.go):
//   - The loopback listener (api.listen, 127.0.0.1 by default) is this
//     computer. The master key in data/api.token works there, and only there,
//     along with the menu's ?token= links, which swap it for a cookie of the
//     browser's own.
//   - Listeners other devices reach take per-device credentials (package
//     devices), each with its own scopes, revocable on its own. A device gets
//     one by pairing (pair.go).
//   - The old plain-HTTP api.remote listener keeps working for what was
//     paired before devices had their own keys, and moves them onto their
//     own keys as they show up.
package api

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/health"
)

//go:embed health.html
var healthHTML []byte

// Status is what the daemon reports about itself.
type Status struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Model     string   `json:"model"`
	Tools     int      `json:"tools"`
	Protocols []string `json:"protocols"`
	Channels  []string `json:"channels"`
	Paused    bool     `json:"paused"`
	Pending   int      `json:"pending"`
	Uptime    string   `json:"uptime"`
	Presence  string   `json:"presence"`
}

// Backend is what the server needs from the daemon.
type Backend interface {
	Status(ctx context.Context) Status
	Message(ctx context.Context, in channels.Inbound) (string, error)
	MessageStreaming(ctx context.Context, in channels.Inbound, onDelta func(string)) (string, error)
	MessageEvents(ctx context.Context, in channels.Inbound, ev agent.Events) (string, error)
	SetPaused(paused bool)
	RunProtocol(ctx context.Context, name string) error
	// RunJob triggers a built-in job now: patterns, portrait, nudge, health.
	RunJob(ctx context.Context, name string) error
}

// Server is the twin's HTTP server.
type Server struct {
	addr    string
	backend Backend
	memory  MemoryBackend
	remote  bool

	healthBackend HealthBackend
	screen        ScreenBackend
	protocols     ProtocolsBackend
	chans         ChannelsBackend
	accounts      AccountsBackend
	usage         UsageBackend
	public        map[string]http.HandlerFunc // unauthenticated webhooks (they verify their own signatures)

	name        string
	adminRemote bool
	onPaired    func(PairEvent)
	mounts      []mount
	stepUp      *stepUpState // stepup.go: passkeys for approvals from other devices

	dmu  sync.RWMutex
	devs *devices.Store

	kmu       sync.RWMutex
	token     string   // the master key (data/api.token)
	retired   []string // master keys replaced since this run began (ResetMasterKey)
	tokenPath string   // where the master key is kept, so it can be replaced

	once         sync.Once
	handler      http.Handler
	routeHandler http.Handler

	lmu              sync.Mutex
	reached          []Base                 // remote (TLS) listeners, for pairing links
	remoteIdentities map[string]func() Base // live certificate identities
	remoteLeaves     map[string]remoteLeaf  // pages_trust.go: the certificate each one presents

	live      liveSet
	limiter   *ipLimiter
	legacyLog atomic.Int64 // unix time the old shared key was last logged
	alarm     Alarm        // alarm.go

	pages LocalPages // pages.go: Add your phone, Devices, Backup, Trust
	// statusURLs are the relays' status endpoints for this twin (pwa.go).
	statusURLs func() []string
	adds       addHub // pages_add.go: the "Add your phone" pages open now
	kits       kitHub // pages_backup.go: Recovery Kits waiting for their check word
	// pushTest sends a device a test notification, and pushOn says whether
	// it has notifications on (WithPush sets both).
	pushTest func(deviceID string)
	pushOn   func(deviceID string) bool
}

// WithPublic mounts an unauthenticated handler (for provider webhooks). It
// answers on every listener, even through a proxy: a webhook proves itself
// with its provider's signature.
func (s *Server) WithPublic(prefix string, h http.HandlerFunc) *Server {
	if s.public == nil {
		s.public = map[string]http.HandlerFunc{}
	}
	s.public[prefix] = h
	return s
}

// ScreenBackend supplies the presence screen.
type ScreenBackend interface {
	Screen(ctx context.Context) any
	Events() *events.Bus
	DecideApproval(ctx context.Context, id int64, approve bool) (string, error)
	ScreenshotPath(p string) (string, bool)
}

// WithScreen enables /ui, /screen, /events and approval decisions.
func (s *Server) WithScreen(b ScreenBackend) *Server { s.screen = b; return s }

// HealthBackend supplies self-check reports.
type HealthBackend interface {
	Health() health.Report
	RunHealth(ctx context.Context) health.Report
}

// WithHealth enables /health.
func (s *Server) WithHealth(h HealthBackend) *Server { s.healthBackend = h; return s }

// WithMemory enables the memory page.
func (s *Server) WithMemory(m MemoryBackend) *Server { s.memory = m; return s }

// WithName sets the twin's name for the pages' messages.
func (s *Server) WithName(name string) *Server { s.name = strings.TrimSpace(name); return s }

// WithDevices keeps paired devices in store (data/devices.json) rather than
// in memory.
func (s *Server) WithDevices(store *devices.Store) *Server {
	if store == nil {
		return s
	}
	store.OnChange(s.onDeviceChange)
	s.dmu.Lock()
	s.devs = store
	s.dmu.Unlock()
	return s
}

// Devices is the registry the server checks credentials against.
func (s *Server) Devices() *devices.Store {
	s.dmu.RLock()
	defer s.dmu.RUnlock()
	return s.devs
}

// OnPaired is told about every device that gets its own key: a pairing, an
// old-style code or screen moved onto a key of its own. Browsers on this
// computer opened from the menu are not reported.
func (s *Server) OnPaired(fn func(PairEvent)) *Server { s.onPaired = fn; return s }

// AllowAdminRemote lets devices with the admin scope use the settings pages
// from other devices too (reach.admin_remote). Off by default.
func (s *Server) AllowAdminRemote() *Server { s.adminRemote = true; return s }

// localBase is the loopback address pages and the menu use.
func (s *Server) localBase() string { return "http://" + LoopbackAddr(s.addr) }

// UIURL is the presence screen link the menu opens (it carries the master key,
// which the page swaps for a cookie of the browser's own).
func (s *Server) UIURL() string { return s.localBase() + "/ui?token=" + s.masterKey() }

// MemoryURL is the link the tray opens.
func (s *Server) MemoryURL() string { return s.localBase() + "/memory?token=" + s.masterKey() }

// HealthURL is the health page link.
func (s *Server) HealthURL() string { return s.localBase() + "/health?token=" + s.masterKey() }

// TokenPath is where the bearer token lives.
func TokenPath(dataDir string) string { return filepath.Join(dataDir, "api.token") }

// LoadOrCreateToken returns the token, generating one on first run.
func LoadOrCreateToken(dataDir string) (string, error) {
	p := TokenPath(dataDir)
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	tok := newMasterKey()
	return tok, writeMasterKey(p, tok)
}

// ResetToken replaces the master key in dataDir with a new one, for a twin
// that isn't running (a running one uses Server.ResetMasterKey). It returns
// the new key.
func ResetToken(dataDir string) (string, error) {
	tok := newMasterKey()
	return tok, writeMasterKey(TokenPath(dataDir), tok)
}

func newMasterKey() string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf) // crypto/rand never fails on supported platforms
	return hex.EncodeToString(buf)
}

// writeMasterKey saves a master key: 0600, through a temp file and a rename,
// so a crash never leaves a half-written key.
func writeMasterKey(path, tok string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".api-token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// WithTokenFile says where the master key is kept (data/api.token), so
// ResetMasterKey can replace it there too.
func (s *Server) WithTokenFile(path string) *Server {
	s.kmu.Lock()
	s.tokenPath = path
	s.kmu.Unlock()
	return s
}

// ResetMasterKey replaces the master key with a new one, at once. Anything
// that still holds the old key from another computer (a screen or terminal
// paired the old way) is cut off. This computer keeps working: its terminals
// read the new key from the file, and the menu's links, which carry the old
// one, still open on the loopback listener until the twin restarts.
func (s *Server) ResetMasterKey() error {
	tok := newMasterKey()
	s.kmu.Lock()
	if s.tokenPath != "" {
		if err := writeMasterKey(s.tokenPath, tok); err != nil {
			s.kmu.Unlock()
			return err
		}
	}
	if s.token != "" {
		s.retired = append(s.retired, s.token)
	}
	s.token = tok
	s.kmu.Unlock()
	return nil
}

// RevokeResult is what revoking a device did.
type RevokeResult struct {
	Device devices.Device `json:"device"`
	// KeyChanged: the device came in with the master key (Device.SharedKey),
	// so the master key was replaced too.
	KeyChanged bool `json:"key_changed,omitempty"`
	// KeyError: it came in with the master key, which couldn't be replaced.
	KeyError string `json:"key_error,omitempty"`
}

// RevokeDevice cuts a device off at once. A device that first came in with
// the master key may still hold it, so the master key is replaced too
// (ResetMasterKey); otherwise revoking it would cut off only its own key.
func (s *Server) RevokeDevice(id string) (RevokeResult, error) {
	store := s.Devices()
	before, _ := store.Get(id)
	d, err := store.Revoke(id)
	if err != nil {
		return RevokeResult{}, err
	}
	res := RevokeResult{Device: d}
	if !d.SharedKey || before.Revoked() {
		return res, nil
	}
	if err := s.ResetMasterKey(); err != nil {
		res.KeyError = err.Error()
		slog.Warn("a revoked device came in with the master key, which couldn't be replaced", "device", d.ID, "err", err)
		return res, nil
	}
	res.KeyChanged = true
	return res, nil
}

// New builds a server. Paired devices live in memory until WithDevices gives
// it the registry on disk.
func New(addr, token string, backend Backend) *Server {
	s := &Server{addr: addr, token: strings.TrimSpace(token), backend: backend, limiter: newIPLimiter(20, 200)}
	s.WithDevices(devices.NewMemory())
	return s
}

// AllowRemote permits binding to a non-loopback address.
func (s *Server) AllowRemote() *Server { s.remote = true; return s }

// Start listens on api.listen until ctx ends. A loopback address is the
// loopback listener. A LAN or Tailscale address (api.remote) serves this
// computer's own connections as loopback and everyone else's as the old
// plain-HTTP remote listener; a specific address also gets a loopback
// listener on the same port, so the menu, the terminal and Google sign-in
// always have 127.0.0.1.
func (s *Server) Start(ctx context.Context) error {
	host, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		return fmt.Errorf("api listen %q: %w", s.addr, err)
	}
	if !isLoopbackHost(host) && !s.remote {
		return errors.New("api.listen must be a loopback address unless api.remote is true")
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	if isLoopbackHost(host) {
		return s.serve(ctx, ln, listener{kind: kindLoopback, via: "loopback"}, false)
	}
	// The menu's links and this computer's terminals use 127.0.0.1 on the
	// same port. If another program holds it, they would hand it the master
	// key: stop, and say so.
	var lo net.Listener
	if !isUnspecified(host) {
		if lo, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", port)); err != nil {
			ln.Close()
			return fmt.Errorf("another program is using 127.0.0.1:%s, which this computer's menu and terminals use to reach me; quit it, or pick another port in api.listen: %w", port, err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- s.serve(ctx, ln, listener{kind: kindLegacy}, true) }()
	if lo != nil {
		go func() { errc <- s.serve(ctx, lo, listener{kind: kindLoopback, via: "loopback"}, false) }()
	}
	err = <-errc
	cancel()
	return err
}

// serve runs one listener until ctx ends.
func (s *Server) serve(ctx context.Context, ln net.Listener, l listener, classify bool) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext:       connContext(l, classify),
	}
	stop := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	})
	defer stop()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Handler is every route behind the listener checks. Mount everything first:
// the routes are fixed on the first call.
func (s *Server) Handler() http.Handler {
	s.once.Do(s.initHandlers)
	return s.handler
}

// routes registers everything the server answers. Each route says where it
// answers (loopback only, or remote too) and what it needs (a scope, nothing,
// or the loopback listener with admin).
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	local := authz{s, LoopbackOnly}
	remote := authz{s, Remote}

	mux.HandleFunc("GET /healthz", remote.Public(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	}))
	for prefix, h := range s.public {
		mux.HandleFunc(prefix, remote.Public(h))
	}
	if s.chans != nil {
		s.channelRoutes(mux, local)
	}
	if s.accounts != nil {
		s.accountRoutes(mux, local)
	}
	mux.HandleFunc("GET /status", remote.Require(devices.View, s.status))
	mux.HandleFunc("POST /message", remote.Require(devices.Chat, s.message))
	mux.HandleFunc("POST /message/stream", remote.Require(devices.Chat, s.messageStream))
	mux.HandleFunc("POST /pause", local.Require(devices.Admin, s.pause))
	mux.HandleFunc("POST /protocols/run", local.Require(devices.Admin, s.runProtocol))
	mux.HandleFunc("POST /jobs/run", local.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		if err := s.backend.RunJob(ctx, req.Name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"ok": req.Name})
	}))
	if s.memory != nil {
		s.memoryRoutes(mux, s.memory, local)
	}
	if s.screen != nil {
		s.screenRoutes(mux, remote)
		s.browserLiveRoutes(mux, remote) // browser_live.go
		s.showRoutes(mux, remote)        // show.go
	}
	if s.usage != nil {
		s.usageRoutes(mux, remote) // usage.go
	}
	if s.protocols != nil {
		s.protocolRoutes(mux, local)
	}
	if s.healthBackend != nil {
		mux.HandleFunc("GET /health", s.page(local, devices.View, healthHTML, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, s.healthBackend.Health())
		}))
		mux.HandleFunc("POST /health/run", local.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
			defer cancel()
			writeJSON(w, s.healthBackend.RunHealth(ctx))
		}))
	}
	s.pairRoutes(mux, local, remote)
	s.selfRoutes(mux, remote)       // devices_self.go
	s.voiceSetupRoutes(mux, local)  // voice_setup.go
	s.voiceListenRoutes(mux, local) // voice_listen.go
	s.screenOpenRoutes(mux, local)  // voice_listen.go
	s.pwaRoutes(mux, remote)
	for _, m := range s.mounts {
		m.fn(mux, authz{s, m.exp})
	}
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.backend.Status(r.Context()))
}

// MessageRequest is the body for POST /message.
type MessageRequest struct {
	Channel string `json:"channel"`
	ChatID  string `json:"chat_id"`
	Text    string `json:"text"`
	// Client is the sending page's own id (a presence screen's): what the
	// twin hears and says in that turn goes to every other screen marked
	// with it, so the page that typed it doesn't show it twice.
	Client string `json:"client,omitempty"`
}

type clientKey struct{}

// WithClient notes on ctx the page a message came from (MessageRequest.Client).
func WithClient(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, clientKey{}, id)
}

// ClientFrom is the page a message came from, or "".
func ClientFrom(ctx context.Context) string {
	id, _ := ctx.Value(clientKey{}).(string)
	return id
}

// clientID keeps a page's id to what a page makes (letters and digits, a
// few dozen of them), so it can't carry anything else to other screens.
func clientID(s string) string {
	if len(s) > 64 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return ""
		}
	}
	return s
}

// MessageResponse is the reply.
type MessageResponse struct {
	Reply string `json:"reply"`
}

func (s *Server) message(w http.ResponseWriter, r *http.Request) {
	var req MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if e := s.chatFor(r, &req); e != nil {
		s.fail(w, r, http.StatusBadRequest, *e)
		return
	}
	ctx, cancel := s.turnContext(r, clientID(req.Client))
	defer cancel()
	reply, err := s.backend.Message(ctx, channels.Inbound{Channel: req.Channel, ChatID: req.ChatID, Sender: "owner", Text: req.Text, IsOwner: true})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, MessageResponse{Reply: reply})
}

// messageStream answers with server-sent events: "delta" events as text arrives,
// then one "done" event carrying the full reply (or "error").
func (s *Server) messageStream(w http.ResponseWriter, r *http.Request) {
	var req MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if e := s.chatFor(r, &req); e != nil {
		s.fail(w, r, http.StatusBadRequest, *e)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// The turn outlives its request (turnContext), so the twin has the words
	// from here on: say so at once, not with its first word. A client that
	// loses the line before then can tell "not sent" from "sent, reply cut",
	// and the screen knows its words took the twin's "stop asking?".
	fl.Flush()
	var mu sync.Mutex
	emit := func(event string, v any) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		fl.Flush()
	}
	ctx, cancel := s.turnContext(r, clientID(req.Client))
	defer cancel()
	reply, err := s.backend.MessageEvents(ctx, channels.Inbound{Channel: req.Channel, ChatID: req.ChatID, Sender: "owner", Text: req.Text, IsOwner: true},
		agent.Events{
			OnDelta: func(d string) { emit("delta", map[string]string{"text": d}) },
			OnTool: func(tool, caption string) {
				if caption != "" {
					emit("note", map[string]string{"tool": tool, "text": caption})
				}
			},
		})
	if err != nil {
		emit("error", map[string]string{"error": err.Error()})
		return
	}
	emit("done", MessageResponse{Reply: reply})
}

// deviceChats are the conversations a paired device may talk in: the local
// front ends' own chats (the screen, a terminal, voice, the API). The owner's
// chats on WhatsApp, Telegram and the rest, their history and the requests
// waiting there, are reached from those apps, not by naming them here.
var deviceChats = map[string]string{"api": "local", "screen": "local", "cli": "terminal", "voice": "local"}

// chatFor fills in a message's channel and chat, and refuses a chat the
// sender may not name. Only this computer's master key names any chat.
func (s *Server) chatFor(r *http.Request, req *MessageRequest) *apiError {
	if req.Channel == "" {
		req.Channel = "api"
	}
	if p := PeerFrom(r.Context()); p.Master && p.Loopback {
		if req.ChatID == "" {
			req.ChatID = "local"
		}
		return nil
	}
	own, ok := deviceChats[req.Channel]
	if ok && req.ChatID == "" {
		req.ChatID = own
	}
	if !ok || req.ChatID != own {
		return &apiError{Error: "wrong_chat", Message: "A paired device can talk to " + s.twinName() + " only in its own chat, not in another app's.",
			Fix: "Talk here, on the screen or in `mirrin chat`; to reach " + s.twinName() + " on WhatsApp or another app, message it there."}
	}
	return nil
}

// errRevoked is why a revoked device's requests end (context.Cause), so a
// chat turn that outlives its request (turnContext) still stops for it.
var errRevoked = errors.New("this device was revoked")

// turnContext is a chat turn's context. It outlives the request: a phone
// that locks or a tab that closes mid-turn still gets its turn finished and
// recorded, within 15 minutes. Revoking the device still stops it, whether
// or not its client is still connected: the turn is tracked under the
// device itself, not only through the request.
func (s *Server) turnContext(r *http.Request, client string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(WithClient(context.WithoutCancel(r.Context()), client), 15*time.Minute)
	untrack := func() {}
	if p := PeerFrom(r.Context()); p.Device != nil {
		ctx, untrack = s.live.track(ctx, p.Device.ID)
	}
	stop := context.AfterFunc(r.Context(), func() {
		if errors.Is(context.Cause(r.Context()), errRevoked) {
			cancel()
		}
	})
	return ctx, func() { stop(); untrack(); cancel() }
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	var req struct{ Paused bool }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.backend.SetPaused(req.Paused)
	writeJSON(w, map[string]bool{"paused": req.Paused})
}

func (s *Server) runProtocol(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.backend.RunProtocol(r.Context(), req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"ok": req.Name})
}
