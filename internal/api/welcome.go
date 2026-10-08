package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/persona"
)

// Brain is a model the owner can choose without editing a config file.
type Brain struct {
	Provider, Label, Model, Detail string
	Ready                          bool
}
type Source struct{ Kind, Label, Path string }
type HumanError struct{ Sentence, Fix, URL string }

func (e *HumanError) Error() string { return e.Sentence }

type WelcomeBackend interface {
	DetectBrains(context.Context) []Brain
	UseBrain(ctx context.Context, provider, key, model string) error
	// SetNames saves both names and the persona, and how the twin is to
	// address the owner: "name" (their first name), "sir" or "ma'am"; ""
	// leaves that as it is.
	SetNames(ctx context.Context, you, twin, persona, address string) error
	// Hello is the twin's first words, in character, streamed: onStatus
	// says what it is doing first ("Looking at your evening…"), then
	// onDelta carries the words as they come.
	Hello(ctx context.Context, onDelta, onStatus func(string)) (HelloReply, error)
	RestoreSources(context.Context) []Source
}

// HelloReply is the twin's first words, and the small print the welcome
// page shows under them ("" for none): where the weather came from. Offer
// is the morning briefing the hello offers after them, nil for none.
type HelloReply struct {
	Text, Note string
	Offer      *BriefingOffer
}

// BriefingOffer is the first hello's "Want me to brief you at seven
// tomorrow?": the request it raised, the words, and the time it suggests.
type BriefingOffer struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
	Time string `json:"time"`
}

// WelcomeBriefing is a WelcomeBackend that answers the briefing offer from
// the welcome page's card: yes at a time ("07:30"), or later. It returns
// what the card says next.
type WelcomeBriefing interface {
	AnswerBriefing(ctx context.Context, yes bool, at string) (string, error)
}

// WelcomeCalendar is a WelcomeBackend that says whether a calendar is
// connected: until one is, the welcome page offers to connect it before
// the first hello, and the step can be skipped.
type WelcomeCalendar interface {
	CalendarConnected(ctx context.Context) bool
}

// WelcomeSpeaker is a WelcomeBackend that can say the first words out loud
// in the twin's own voice. It reports whether it spoke: it doesn't until
// voice is set up, and the page then uses the Mac's standard voice.
type WelcomeSpeaker interface {
	SayHello(ctx context.Context, line string) bool
}
type WelcomeRestoreRequest struct {
	Kind, Path, Phrase string
	Confirm            bool
}
type WelcomeRestorer interface {
	RestoreWelcome(context.Context, WelcomeRestoreRequest) (any, error)
}

// RestoreWelcomeInfo is what the welcome page says after a restore (GET
// /welcome/restore/report). It is read from the restored twin when the page
// asks, so it only claims what is really there. Detail is the restore's own
// report (welcome-restore.txt), "" when there is none. Line is "" when the
// rest couldn't be read, or the restore didn't finish: the page then shows
// Detail alone, as it did before.
type RestoreWelcomeInfo struct {
	// Line is the twin's welcome, from a template: "Welcome back, Akshay.
	// Same Mirrin, new Mac: 214 things I remember, your morning briefing and
	// 2 more routines, and 3 reminders came along."
	Line string `json:"line,omitempty"`
	// Persona is the restored twin's persona, and Character how the page
	// draws it (characters.js).
	Persona   string `json:"persona,omitempty"`
	Character string `json:"character,omitempty"`
	Facts     int    `json:"facts"`
	// Routines are the names of the enabled protocols.
	Routines  []string `json:"routines"`
	Reminders int      `json:"reminders"`
	// Todo is "Still to do here:", the device review first.
	Todo []RestoreTodo `json:"todo"`
	// StandBy says the old machine stands by until `mirrin backup resume`,
	// when the restore left it the note to.
	StandBy string `json:"stand_by,omitempty"`
	Detail  string `json:"detail"`
}

// RestoreTodo is a line of "Still to do here:" and the page that does it
// ("" for none).
type RestoreTodo struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
}

// WelcomePreviewer is a WelcomeBackend that can say a persona's greeting in
// that persona's own voice (the welcome page's "Hear Nyra"). It reports
// whether it spoke: it doesn't until voice is set up.
type WelcomePreviewer interface {
	PreviewPersona(ctx context.Context, id string) bool
}

// WelcomeWakeChecker is a WelcomeBackend that knows whether a persona's
// wake-word model (as its persona file names it) is on this computer or
// comes with this build. Without one, a persona that names a model counts
// as having it.
type WelcomeWakeChecker interface {
	HasWakeModel(model string) bool
}

// WelcomePersona is a character the welcome page offers (GET
// /welcome/personas). Character is how the page draws it (characters.js);
// InstantWake is true when a wake model for its name is here to use.
type WelcomePersona struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Tagline     string `json:"tagline"`
	Greeting    string `json:"greeting"`
	Address     string `json:"address"`
	WakePhrase  string `json:"wake_phrase"`
	InstantWake bool   `json:"instant_wake"`
	Character   string `json:"character"`
}

// welcomeOrder is the order the welcome page offers the bundled personas.
// Any other bundled one comes after them.
var welcomeOrder = []string{"mirrin", "nyra", "pickoo"}

// welcomePersonas are the bundled personas, as the welcome page offers them.
// hasModel says whether a persona's wake model is here to use.
func welcomePersonas(hasModel func(model string) bool) []WelcomePersona {
	all := persona.Bundled()
	rank := func(p persona.Persona) int {
		if i := slices.Index(welcomeOrder, p.ID); i >= 0 {
			return i
		}
		return len(welcomeOrder)
	}
	slices.SortStableFunc(all, func(a, b persona.Persona) int { return rank(a) - rank(b) })
	out := make([]WelcomePersona, 0, len(all))
	for _, p := range all {
		out = append(out, WelcomePersona{ID: p.ID, Name: p.Name, Tagline: p.Tagline, Greeting: persona.Render(p.Greeting, "", p.Address), Address: p.Address,
			WakePhrase: "Hey " + p.Spoken(), InstantWake: p.WakeModel != "" && hasModel(p.WakeModel), Character: characterOf(p.ID)})
	}
	return out
}

// wakeModelCheck asks the backend whether a wake model is here, when it
// can say (WelcomeWakeChecker).
func wakeModelCheck(b WelcomeBackend) func(string) bool {
	if c, ok := b.(WelcomeWakeChecker); ok {
		return c.HasWakeModel
	}
	return func(string) bool { return true }
}

// characterOf is how characters.js draws a persona (its kindFor): Nyra
// and Mirrin as themselves, everyone else as the penguin. Mirrin was MAVRK
// before, and a restored twin may still say so, or name the retired persona
// plain, who is Mirrin now.
func characterOf(id string) string {
	switch id {
	case "nyra":
		return "nyra"
	case "mirrin", "mavrk", "plain":
		return "maverick"
	}
	return "penguin"
}

// welcomeChannels are the ways to reach the owner away from this Mac that
// the welcome's step 3 offers, as this build has them: no WhatsApp in a
// -tags nowhatsapp build, and none without the Channels page, which pairs
// them.
func (s *Server) welcomeChannels() []string {
	out := []string{}
	if s.chans == nil {
		return out
	}
	out = append(out, "telegram")
	if whatsapp.Built {
		out = append(out, "whatsapp")
	}
	return out
}

//go:embed welcome.html
var welcomeHTML []byte

func (s *Server) WelcomeURL() string { return s.localBase() + "/welcome?token=" + s.masterKey() }
func welcomeError(err error) *HumanError {
	var h *HumanError
	if errors.As(err, &h) {
		return h
	}
	return &HumanError{Sentence: "That didn't work yet.", Fix: "Try again. If it keeps happening, open Health from the menu."}
}
func (s *Server) WithWelcome(b WelcomeBackend) *Server {
	return s.Mount("welcome", LoopbackOnly, func(m *http.ServeMux, a Authz) {
		page := s.page(authz{s: s, exp: LoopbackOnly}, devices.Admin, welcomeHTML, nil)
		m.HandleFunc("GET /welcome", func(w http.ResponseWriter, r *http.Request) {
			if listenerFrom(r.Context()).kind != kindLoopback {
				http.NotFound(w, r)
				return
			}
			page(w, r)
		})
		add := func(path string, fn func(http.ResponseWriter, *http.Request) error) {
			m.HandleFunc(path, a.Local(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				if err := fn(w, r); err != nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					writeJSON(w, welcomeError(err))
				}
			}))
		}
		decode := func(w http.ResponseWriter, r *http.Request, v any) error {
			d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
			d.DisallowUnknownFields()
			if err := d.Decode(v); err != nil {
				return &HumanError{Sentence: "I couldn't read that.", Fix: "Check the fields and try again."}
			}
			return nil
		}
		add("GET /welcome/brains", func(w http.ResponseWriter, r *http.Request) error {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			writeJSON(w, b.DetectBrains(ctx))
			return nil
		})
		add("POST /welcome/brain", func(w http.ResponseWriter, r *http.Request) error {
			var v struct{ Provider, Key, Model string }
			if err := decode(w, r, &v); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			if err := b.UseBrain(ctx, v.Provider, v.Key, v.Model); err != nil {
				return err
			}
			writeJSON(w, map[string]bool{"ok": true})
			return nil
		})
		add("POST /welcome/check", welcomeCheck(b)) // model_key.go
		add("GET /welcome/personas", func(w http.ResponseWriter, r *http.Request) error {
			writeJSON(w, welcomePersonas(wakeModelCheck(b)))
			return nil
		})
		add("GET /welcome/channels", func(w http.ResponseWriter, r *http.Request) error {
			writeJSON(w, s.welcomeChannels())
			return nil
		})
		add("POST /welcome/preview", func(w http.ResponseWriter, r *http.Request) error {
			var v struct{ Persona string }
			if err := decode(w, r, &v); err != nil {
				return err
			}
			spoken := false
			if p, ok := b.(WelcomePreviewer); ok {
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				defer cancel()
				spoken = p.PreviewPersona(ctx, v.Persona)
			}
			writeJSON(w, map[string]bool{"spoken": spoken})
			return nil
		})
		add("POST /welcome/names", func(w http.ResponseWriter, r *http.Request) error {
			var v struct{ You, Twin, Persona, Address string }
			if err := decode(w, r, &v); err != nil {
				return err
			}
			if err := b.SetNames(r.Context(), v.You, v.Twin, v.Persona, v.Address); err != nil {
				return err
			}
			writeJSON(w, map[string]bool{"ok": true})
			return nil
		})
		// The first hello streams as events: status, then delta as the words
		// come, note (the small print), done, and spoke once the twin said
		// it out loud; or error. {"Speak":true} asks for it out loud.
		add("POST /welcome/hello", func(w http.ResponseWriter, r *http.Request) error {
			var v struct{ Speak bool }
			if r.ContentLength != 0 {
				if err := decode(w, r, &v); err != nil {
					return err
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			send := func(event string, v any) {
				data, _ := json.Marshal(v)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
			defer cancel()
			out, err := b.Hello(ctx, func(s string) { send("delta", s) }, func(s string) { send("status", s) })
			if err != nil {
				send("error", welcomeError(err))
				return nil
			}
			if out.Note != "" {
				send("note", out.Note)
			}
			send("done", out.Text)
			if out.Offer != nil {
				send("offer", out.Offer)
			}
			if s, ok := b.(WelcomeSpeaker); ok && v.Speak {
				sctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				defer cancel()
				if s.SayHello(sctx, out.Text) {
					send("spoke", true)
				}
			}
			return nil
		})
		if cal, ok := b.(WelcomeCalendar); ok {
			add("GET /welcome/calendar", func(w http.ResponseWriter, r *http.Request) error {
				writeJSON(w, map[string]bool{"connected": cal.CalendarConnected(r.Context())})
				return nil
			})
		}
		if br, ok := b.(WelcomeBriefing); ok {
			add("POST /welcome/briefing", func(w http.ResponseWriter, r *http.Request) error {
				var v struct {
					Yes  bool
					Time string
				}
				if err := decode(w, r, &v); err != nil {
					return err
				}
				said, err := br.AnswerBriefing(r.Context(), v.Yes, v.Time)
				if err != nil {
					return err
				}
				writeJSON(w, map[string]string{"text": said})
				return nil
			})
		}
		if report, ok := b.(interface {
			WelcomeRestoreReport(context.Context) RestoreWelcomeInfo
			DismissWelcomeRestore(context.Context) error
		}); ok {
			add("GET /welcome/restore/report", func(w http.ResponseWriter, r *http.Request) error {
				info := report.WelcomeRestoreReport(r.Context())
				if info.Line != "" {
					info.Character = characterOf(info.Persona)
				}
				writeJSON(w, info)
				return nil
			})
			add("POST /welcome/restore/dismiss", func(w http.ResponseWriter, r *http.Request) error {
				if err := report.DismissWelcomeRestore(r.Context()); err != nil {
					return err
				}
				writeJSON(w, map[string]bool{"ok": true})
				return nil
			})
		}
		add("GET /welcome/restore/sources", func(w http.ResponseWriter, r *http.Request) error {
			writeJSON(w, b.RestoreSources(r.Context()))
			return nil
		})
		add("POST /welcome/restore", func(w http.ResponseWriter, r *http.Request) error {
			var v WelcomeRestoreRequest
			if err := decode(w, r, &v); err != nil {
				return err
			}
			if !v.Confirm {
				return &HumanError{Sentence: "Restoring replaces the twin on this computer.", Fix: "Confirm you want to restore. Your current twin will be kept aside."}
			}
			restorer, ok := b.(WelcomeRestorer)
			if !ok {
				return &HumanError{Sentence: "Restore isn't available here yet.", Fix: "Type mirrin restore in Terminal instead."}
			}
			out, err := restorer.RestoreWelcome(r.Context(), v)
			if err != nil {
				return err
			}
			writeJSON(w, out)
			return nil
		})
	})
}
