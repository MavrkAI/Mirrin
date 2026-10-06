package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
	"github.com/MavrkAI/Mirrin/internal/devices"
)

type welcomeFake struct {
	you, persona, address string
	err                   error
	restored              bool
	previewed             []string
	note                  string   // the hello's small print
	voice                 bool     // SayHello speaks
	said                  []string // what SayHello was asked to say
}

func (*welcomeFake) DetectBrains(context.Context) []Brain {
	return []Brain{{Provider: "ollama", Model: "llama3.1", Ready: true}}
}
func (b *welcomeFake) UseBrain(context.Context, string, string, string) error { return b.err }
func (b *welcomeFake) SetNames(_ context.Context, you, twin, persona, address string) error {
	b.you, b.persona, b.address = you, persona, address
	return nil
}
func (b *welcomeFake) PreviewPersona(_ context.Context, id string) bool {
	b.previewed = append(b.previewed, id)
	return true
}
func (b *welcomeFake) Hello(_ context.Context, delta, status func(string)) (HelloReply, error) {
	status("Looking at your evening…")
	if b.err != nil {
		return HelloReply{}, b.err
	}
	s := "Hello, " + b.you + "."
	delta(s)
	return HelloReply{Text: s, Note: b.note}, nil
}
func (b *welcomeFake) SayHello(_ context.Context, line string) bool {
	b.said = append(b.said, line)
	return b.voice
}
func (*welcomeFake) RestoreSources(context.Context) []Source {
	return []Source{{Kind: "folder", Label: "Backup folder"}}
}
func (b *welcomeFake) RestoreWelcome(context.Context, WelcomeRestoreRequest) (any, error) {
	b.restored = true
	return map[string]bool{"ok": true}, nil
}

func TestWelcomeFirstReply(t *testing.T) {
	e := newEnv(t)
	b := &welcomeFake{}
	e.s.WithWelcome(b)
	for _, q := range []req{{method: "POST", path: "/welcome/names", body: `{"You":"Mina","Twin":"Ember","Persona":"pickoo"}`}, {method: "POST", path: "/welcome/hello"}} {
		q.header = bearer(master)
		start := time.Now()
		w := e.do(onLoopback, q)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q.path, w.Code, w.Body)
		}
		if q.path == "/welcome/hello" {
			if time.Since(start) > 5*time.Second || !strings.Contains(w.Body.String(), "event: delta") || !strings.Contains(w.Body.String(), "event: done") || !strings.Contains(w.Body.String(), "Mina") {
				t.Fatalf("not a prompt streamed greeting: %s", w.Body)
			}
			if w.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatal(w.Header())
			}
		}
	}
}

// The first hello's stream says what the twin is looking at before any
// words, then the small print and the words; asked to say it out loud, the
// twin does once voice is set up, and the stream says it spoke.
func TestWelcomeHelloStream(t *testing.T) {
	events := func(body string) []string {
		var out []string
		for _, line := range strings.Split(body, "\n") {
			if ev, ok := strings.CutPrefix(line, "event: "); ok {
				out = append(out, ev)
			}
		}
		return out
	}
	for _, c := range []struct {
		name, body string
		voice      bool
		want       string
		said       int
	}{
		{"quiet", "", true, "status delta note done", 0},
		{"out loud", `{"Speak":true}`, true, "status delta note done spoke", 1},
		{"out loud, no voice yet", `{"Speak":true}`, false, "status delta note done", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			b := &welcomeFake{you: "Akshay", note: "Weather comes from Open-Meteo.", voice: c.voice}
			e.s.WithWelcome(b)
			w := e.do(onLoopback, req{method: "POST", path: "/welcome/hello", body: c.body, header: bearer(master)})
			if w.Code != 200 || strings.Join(events(w.Body.String()), " ") != c.want {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if !strings.Contains(w.Body.String(), "data: \"Looking at your evening…\"") || !strings.Contains(w.Body.String(), "Weather comes from Open-Meteo.") {
				t.Fatal(w.Body)
			}
			if len(b.said) != c.said || (c.said > 0 && b.said[0] != "Hello, Akshay.") {
				t.Fatalf("said %q", b.said)
			}
		})
	}
}
func TestWelcomeAlwaysLoopbackOnly(t *testing.T) {
	e := newEnv(t)
	e.s.WithWelcome(&welcomeFake{})
	e.s.adminRemote = true
	_, remoteAdmin, err := e.store.Add("Admin phone", devices.KindPWA, devices.AllScopes, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []req{{path: "/welcome"}, {path: "/welcome/brains"}, {method: "POST", path: "/welcome/brain", body: `{}`}, {method: "POST", path: "/welcome/names", body: `{}`}, {path: "/welcome/personas"}, {path: "/welcome/channels"}, {method: "POST", path: "/welcome/preview", body: `{}`}, {method: "POST", path: "/welcome/hello"}, {path: "/welcome/restore/sources"}, {method: "POST", path: "/welcome/restore", body: `{}`}} {
		for _, token := range []string{master, remoteAdmin} {
			q.header = bearer(token)
			if w := e.do(onRemote, q); w.Code != 404 {
				t.Errorf("%s: %d", q.path, w.Code)
			}
		}
	}
}

// Step 3 offers Telegram, and WhatsApp in builds that have it. A twin with
// no Channels page, which pairs them, has none to offer.
func TestWelcomeChannelsAsBuilt(t *testing.T) {
	want := `["telegram"]`
	if whatsapp.Built {
		want = `["telegram","whatsapp"]`
	}
	for _, c := range []struct {
		name, want string
		pairs      bool
	}{{"with the Channels page", want, true}, {"without it", `[]`, false}} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if !c.pairs {
				e.s.chans = nil
			}
			e.s.WithWelcome(&welcomeFake{})
			w := e.do(onLoopback, req{path: "/welcome/channels", header: bearer(master)})
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != c.want {
				t.Fatalf("%d %s, want %s", w.Code, w.Body, c.want)
			}
		})
	}
}
func TestWelcomeRequiresAuthAndOrigin(t *testing.T) {
	e := newEnv(t)
	e.s.WithWelcome(&welcomeFake{})
	if w := e.do(onLoopback, req{path: "/welcome/brains"}); w.Code != 401 {
		t.Fatal(w.Code)
	}
	h := bearer(master)
	h["Origin"] = "https://evil.example"
	if w := e.do(onLoopback, req{method: "POST", path: "/welcome/names", body: `{"You":"Mina"}`, header: h}); w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
}
func TestWelcomeHidesRawErrors(t *testing.T) {
	e := newEnv(t)
	b := &welcomeFake{err: errors.New("raw secret sk-private stack trace")}
	e.s.WithWelcome(b)
	for _, path := range []string{"/welcome/brain", "/welcome/hello"} {
		w := e.do(onLoopback, req{method: "POST", path: path, body: `{}`, header: bearer(master)})
		if strings.Contains(w.Body.String(), "sk-private") || !strings.Contains(w.Body.String(), "Try again") {
			t.Fatal(w.Body)
		}
	}
}
func TestWelcomeRestoreNeedsConfirmation(t *testing.T) {
	e := newEnv(t)
	b := &welcomeFake{}
	e.s.WithWelcome(b)
	w := e.do(onLoopback, req{method: "POST", path: "/welcome/restore", body: `{"Kind":"folder"}`, header: bearer(master)})
	if w.Code != 400 || b.restored {
		t.Fatal(w.Code, b.restored)
	}
	w = e.do(onLoopback, req{method: "POST", path: "/welcome/restore", body: `{"Kind":"folder","Confirm":true}`, header: bearer(master)})
	if w.Code != 200 || !b.restored {
		t.Fatal(w.Code, b.restored)
	}
}

// restoredFake is a welcome backend with a restore to report.
type restoredFake struct {
	welcomeFake
	info RestoreWelcomeInfo
}

func (b *restoredFake) WelcomeRestoreReport(context.Context) RestoreWelcomeInfo { return b.info }
func (b *restoredFake) DismissWelcomeRestore(context.Context) error             { return nil }

// After a restore the page gets the twin's welcome with how to draw it; a
// report alone comes back as it is, with no character to draw.
func TestWelcomeRestoreReportDrawsTheTwin(t *testing.T) {
	for _, c := range []struct {
		name string
		info RestoreWelcomeInfo
		want map[string]any
	}{
		{"welcomed", RestoreWelcomeInfo{Line: "Welcome back, sir. Same Mirrin, new Mac.", Persona: "mirrin", Todo: []RestoreTodo{{Text: "Review your devices.", URL: "/restore/review"}}, Detail: "Your twin is home."},
			map[string]any{"line": "Welcome back, sir. Same Mirrin, new Mac.", "character": "maverick", "detail": "Your twin is home."}},
		{"report alone", RestoreWelcomeInfo{Persona: "mirrin", Detail: "Your backup couldn't be restored."},
			map[string]any{"line": nil, "character": nil, "detail": "Your backup couldn't be restored."}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.s.WithWelcome(&restoredFake{info: c.info})
			w := e.do(onLoopback, req{path: "/welcome/restore/report", header: bearer(master)})
			var got map[string]any
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("%s: got %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestWelcomeRejectsLargeRequest(t *testing.T) {
	e := newEnv(t)
	e.s.WithWelcome(&welcomeFake{})
	w := e.do(onLoopback, req{method: "POST", path: "/welcome/brain", body: `{"Key":"` + strings.Repeat("x", 17000) + `"}`, header: bearer(master)})
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}

// Step 2 of the welcome offers the bundled personas in a fixed order, each
// with what the page shows of it and how characters.js draws it.
func TestWelcomePersonasInOrder(t *testing.T) {
	e := newEnv(t)
	e.s.WithWelcome(&welcomeFake{})
	w := e.do(onLoopback, req{path: "/welcome/personas", header: bearer(master)})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	keys := []string{"id", "name", "tagline", "greeting", "address", "wake_phrase", "instant_wake", "character"}
	want := []struct {
		id, name, address, wake, character string
		instant                            bool
	}{
		{"mirrin", "Mirrin", "sir", "Hey Mirrin", "maverick", true},
		// each names a wake model, and this backend can't say it's missing
		{"nyra", "Nyra", "", "Hey Nyra", "nyra", true},
		{"pickoo", "Pickoo", "", "Hey Pickoo", "penguin", true},
	}
	if len(got) != len(want) {
		t.Fatalf("%d personas: %s", len(got), w.Body)
	}
	for i, p := range want {
		g := got[i]
		if len(g) != len(keys) {
			t.Errorf("%s has fields %v", p.id, g)
		}
		for _, k := range keys {
			if _, ok := g[k]; !ok {
				t.Errorf("%s has no %s", p.id, k)
			}
		}
		if g["id"] != p.id || g["name"] != p.name || g["address"] != p.address || g["wake_phrase"] != p.wake || g["character"] != p.character || g["instant_wake"] != p.instant {
			t.Errorf("persona %d: %v", i, g)
		}
		if g["tagline"] == "" || g["greeting"] == "" {
			t.Errorf("%s has no tagline or greeting", p.id)
		}
	}
	if got[1]["greeting"] != "Hello. Nyra here." {
		t.Errorf("Nyra's greeting %q", got[1]["greeting"])
	}
}

// The names step carries how the twin is to address the owner, and Hear
// asks the backend to say a persona's hello; a backend that can't speaks
// nothing.
func TestWelcomeNamesAndPreview(t *testing.T) {
	e := newEnv(t)
	b := &welcomeFake{}
	e.s.WithWelcome(b)
	w := e.do(onLoopback, req{method: "POST", path: "/welcome/names", body: `{"You":"Akshaya Kumar","Twin":"Nyra","Persona":"nyra","Address":"name"}`, header: bearer(master)})
	if w.Code != 200 || b.you != "Akshaya Kumar" || b.persona != "nyra" || b.address != "name" {
		t.Fatalf("%d %s: %+v", w.Code, w.Body, b)
	}
	w = e.do(onLoopback, req{method: "POST", path: "/welcome/preview", body: `{"persona":"nyra"}`, header: bearer(master)})
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"spoken":true}` || len(b.previewed) != 1 || b.previewed[0] != "nyra" {
		t.Fatalf("%d %s %v", w.Code, w.Body, b.previewed)
	}

	quiet := newEnv(t)
	quiet.s.WithWelcome(keyCheckingWelcome{})
	w = quiet.do(onLoopback, req{method: "POST", path: "/welcome/preview", body: `{"persona":"nyra"}`, header: bearer(master)})
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"spoken":false}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// The drawings are code anyone may load, on every listener, as the app's
// icons are: the welcome page and the screen both need them, and a phone
// or wall screen loads them over the HTTPS listener before it is paired.
func TestCharactersScriptIsPublic(t *testing.T) {
	e := newEnv(t)
	ok := func(where string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") || !bytes.Equal(w.Body.Bytes(), charactersJS) || !strings.Contains(w.Body.String(), "window.Characters") {
			t.Fatalf("%s: %d %s", where, w.Code, w.Header())
		}
	}
	ok("loopback", e.do(onLoopback, req{path: "/characters.js"}))
	ok("remote", e.do(onRemote, req{path: "/characters.js"}))
	w := httptest.NewRecorder()
	e.s.remoteHandler(RemoteOptions{Hostnames: []string{"twin.test"}}).ServeHTTP(w, httptest.NewRequest("GET", "https://twin.test/characters.js", nil))
	ok("the HTTPS listener", w)
}
