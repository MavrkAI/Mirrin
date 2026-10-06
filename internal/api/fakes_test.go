package api

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// fake is every backend the server can have, answering everything with
// success and remembering what it was asked.
type fake struct {
	mu         sync.Mutex
	listenErr  error    // what ListenNow answers
	held       bool     // BrowserTakeOver
	active     bool     // BrowserState: the twin is using its browser
	listens    int      // ListenNow calls
	opens      int      // OpenScreen calls
	cancelled  []string // CancelTask ids
	tipsOff    int      // StopTips calls
	replaceErr error    // what UseDifferentGoogleClient answers
	calls      []string
	redirect   string
	finished   bool
	bus        *events.Bus
	shot       string
	authState  string
	inbound    []channels.Inbound
	clients    []string // api.ClientFrom of each message
	provider   string   // the model key card's
	key        string
	aside      bool // the portrait was set aside after a forget
}

func (f *fake) called(what string) {
	f.mu.Lock()
	f.calls = append(f.calls, what)
	f.mu.Unlock()
}

func (f *fake) saw(what string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == what {
			return true
		}
	}
	return false
}

func (f *fake) Status(context.Context) Status { return Status{Name: "Mirrin"} }
func (f *fake) Message(_ context.Context, in channels.Inbound) (string, error) {
	f.called("message")
	f.mu.Lock()
	f.inbound = append(f.inbound, in)
	f.mu.Unlock()
	return "hi", nil
}
func (f *fake) MessageStreaming(context.Context, channels.Inbound, func(string)) (string, error) {
	return "hi", nil
}
func (f *fake) MessageEvents(ctx context.Context, in channels.Inbound, ev agent.Events) (string, error) {
	f.called("stream")
	f.mu.Lock()
	f.inbound = append(f.inbound, in)
	f.clients = append(f.clients, ClientFrom(ctx))
	f.mu.Unlock()
	if ev.OnDelta != nil {
		ev.OnDelta("hi")
	}
	return "hi", nil
}
func (f *fake) SetPaused(bool) { f.called("pause") }

// lastInbound is the last message the twin was handed.
func (f *fake) lastInbound() channels.Inbound {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inbound) == 0 {
		return channels.Inbound{}
	}
	return f.inbound[len(f.inbound)-1]
}
func (f *fake) RunProtocol(_ context.Context, name string) error {
	f.called("run:" + name)
	return nil
}
func (f *fake) RunJob(context.Context, string) error { return nil }

func (f *fake) Screen(context.Context) any { return map[string]any{"ok": true} }
func (f *fake) Events() *events.Bus        { return f.bus }
func (f *fake) DecideApproval(context.Context, int64, bool) (string, error) {
	f.called("decide")
	return "done", nil
}
func (f *fake) ScreenshotPath(string) (string, bool) { return f.shot, true }

func (f *fake) Health() health.Report                   { return health.Report{} }
func (f *fake) RunHealth(context.Context) health.Report { return health.Report{} }

func (f *fake) Facts(context.Context) ([]Fact, error) { return nil, nil }
func (f *fake) AddFact(context.Context, string, string) (int64, error) {
	f.called("addfact")
	return 1, nil
}
func (f *fake) DeleteFact(context.Context, int64) error          { return nil }
func (f *fake) Audit(context.Context, int) ([]AuditEntry, error) { return nil, nil }
func (f *fake) Portrait(context.Context) (string, time.Time, bool, error) {
	return "", time.Time{}, f.aside, nil
}
func (f *fake) RefreshPortrait(context.Context) (string, error) { return "", nil }
func (f *fake) Twin(context.Context) TwinInfo                   { return TwinInfo{} }

func (f *fake) ConnectorStates(context.Context) []ConnectorState                { return nil }
func (f *fake) ConnectChannel(context.Context, string, map[string]string) error { return nil }
func (f *fake) DisconnectChannel(context.Context, string) error                 { return nil }
func (f *fake) TestChannel(context.Context, string) error                       { return nil }
func (f *fake) ConnectFields(context.Context, string, map[string]string) error  { return nil }
func (f *fake) PairWhatsApp(context.Context, bool) error                        { return nil }
func (f *fake) WhatsAppPairing() *whatsapp.Snapshot                             { return nil }
func (f *fake) UnpairWhatsApp(context.Context) error                            { return nil }
func (f *fake) AccountStates(context.Context) []AccountState                    { return nil }
func (f *fake) SaveGoogleClient(context.Context, string, string, string) error  { return nil }
func (f *fake) SetGoogleFeature(context.Context, string, bool) error            { return nil }
func (f *fake) UseDifferentGoogleClient(context.Context) error {
	f.called("replace-client")
	return f.replaceErr
}

// ModelKey and SaveModelKey keep the model key the Accounts card sets; a key
// that isn't "sk-good…" is refused, as a provider would.
func (f *fake) ModelKey(context.Context) ModelKeyState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ModelKeyState{Provider: f.provider, Model: "m", Key: MaskKey(f.key), Providers: []string{"anthropic", "openai"}}
}
func (f *fake) SaveModelKey(_ context.Context, provider, key, _ string) error {
	if err := fakeCheckKey(key); err != nil {
		return err
	}
	f.mu.Lock()
	f.provider, f.key = provider, key
	f.mu.Unlock()
	return nil
}
func (f *fake) CheckBrain(_ context.Context, _, key, _ string) error { return fakeCheckKey(key) }
func fakeCheckKey(key string) error {
	if !strings.HasPrefix(key, "sk-good") {
		return &HumanError{Sentence: "That key wasn't accepted.", Fix: "Copy it again from your provider."}
	}
	return nil
}
func (f *fake) DisconnectGoogle(context.Context) error                     { return nil }
func (f *fake) InstalledProtocols(context.Context) []ProtocolInfo          { return nil }
func (f *fake) InstalledPacks(context.Context) []PackInfo                  { return nil }
func (f *fake) SearchRegistry(context.Context, string) ([]PackInfo, error) { return nil, nil }
func (f *fake) InstallPack(_ context.Context, name string) (string, error) { return name, nil }
func (f *fake) RemovePack(context.Context, string) error                   { return nil }
func (f *fake) SetProtocolEnabled(_ context.Context, name string, on bool) error {
	f.called(fmt.Sprintf("enabled %s %v", name, on))
	return nil
}
func (f *fake) SkipNextProtocol(_ context.Context, name string) error {
	f.called("skip " + name)
	return nil
}
func (f *fake) RescheduleProtocol(_ context.Context, name, schedule string) error {
	f.called("schedule " + name + " " + schedule)
	return nil
}
func (f *fake) Spend(context.Context) (memory.Spend, error) {
	return memory.Spend{Unpriced: []string{}, Currency: "USD", Estimate: true}, nil
}
func (f *fake) BeginGoogle(_ context.Context, redirect string) (string, error) {
	f.mu.Lock()
	f.redirect = redirect
	f.authState = "st4te"
	f.mu.Unlock()
	return "https://accounts.google.com/o/oauth2/auth?client_id=x&redirect_uri=" + redirect + "&state=st4te", nil
}
func (f *fake) ExplainGoogleError(code string) string {
	if code == "access_denied" {
		return "The sign-in didn't go through."
	}
	return "Google stopped the sign-in with an error it didn't name."
}
func (f *fake) FinishGoogle(_ context.Context, state, code string) error {
	f.mu.Lock()
	f.finished = true
	f.mu.Unlock()
	return nil
}

const master = "0123456789abcdef0123456789abcdef0123456789abcdef"

// env is a server with every backend, and helpers to ask it things as if
// over each kind of listener.
type env struct {
	t     *testing.T
	s     *Server
	f     *fake
	store *devices.Store
	path  string

	mu     sync.Mutex
	paired []PairEvent
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	shot := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(shot, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "devices.json")
	store, err := devices.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{bus: events.New(), shot: shot}
	e := &env{t: t, f: f, store: store, path: path}
	e.s = New("127.0.0.1:7742", master, f).WithMemory(f).WithHealth(f).WithScreen(f).WithProtocols(f).WithChannels(f).WithAccounts(f).WithUsage(f).
		WithDevices(store).WithName("Mirrin").OnPaired(func(p PairEvent) {
		e.mu.Lock()
		e.paired = append(e.paired, p)
		e.mu.Unlock()
	})
	e.s.WithPublic("/phone/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("twilio ok")) })
	e.s.limiter = newIPLimiter(1e9, 1e9) // the matrix sends hundreds of requests from one address
	return e
}

// pairedEvents waits briefly for announcements (they are sent in the background).
func (e *env) pairedEvents(n int) []PairEvent {
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		got := append([]PairEvent(nil), e.paired...)
		e.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// where a request arrives.
type where int

const (
	onLoopback where = iota
	onRemote         // a TLS listener for twin.example.ts.net
	onLegacy         // plain-HTTP api.remote on a Tailscale address
)

func (w where) String() string { return [...]string{"loopback", "remote", "legacy"}[w] }

type req struct {
	method, path, body string
	header             map[string]string
	cookies            []*http.Cookie
	ip                 string
	host               string // the Host header, when not the listener's own
}

func (e *env) do(at where, q req) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if q.body != "" {
		body = strings.NewReader(q.body)
	}
	if q.method == "" {
		q.method = http.MethodGet
	}
	host, remote := "127.0.0.1:7742", "127.0.0.1:50000"
	l := listener{kind: kindLoopback, via: "loopback"}
	switch at {
	case onRemote:
		host, remote = "twin.example.ts.net", "203.0.113.5:40000"
		l = listener{kind: kindRemote, via: "tailscale", hosts: []string{"twin.example.ts.net"}}
	case onLegacy:
		host, remote = "100.64.0.1:7742", "100.64.0.9:40000"
		l = listener{kind: kindLegacy, via: "tailscale"}
	}
	if q.ip != "" {
		remote = q.ip + ":40000"
	}
	r := httptest.NewRequest(q.method, "http://"+host+q.path, body)
	r.Host = host
	if q.host != "" {
		r.Host = q.host
	}
	r.RemoteAddr = remote
	if at == onRemote {
		r.TLS = &tls.ConnectionState{}
	}
	for k, v := range q.header {
		r.Header.Set(k, v)
	}
	for _, c := range q.cookies {
		r.AddCookie(c)
	}
	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), listenerKey, l), 2*time.Second)
	defer cancel()
	if strings.HasPrefix(q.path, "/events") {
		// An event stream never ends by itself: hang up after its first frame.
		c2, cancel2 := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel2()
		ctx = c2
	}
	w := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(w, r.WithContext(ctx))
	return w
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

// cookieFrom returns the cookie called name a response set.
func cookieFrom(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (f *fake) CancelTask(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	return nil
}

func (f *fake) StopTips(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tipsOff++
	return nil
}

func (f *fake) OpenScreen(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	return nil
}

func (f *fake) ListenNow(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listens++
	return f.listenErr
}

func (f *fake) BrowserState(context.Context) BrowserState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return BrowserState{Open: true, URL: "https://example.com/", Title: "Example", Held: f.held, Active: f.active}
}
func (f *fake) BrowserWatch(context.Context) (<-chan BrowserFrame, error) {
	ch := make(chan BrowserFrame)
	close(ch)
	return ch, nil
}
func (f *fake) BrowserInput(context.Context, BrowserInput) error { return nil }
func (f *fake) BrowserTakeOver(_ context.Context, on bool) error {
	f.mu.Lock()
	f.held = on
	f.mu.Unlock()
	return nil
}
