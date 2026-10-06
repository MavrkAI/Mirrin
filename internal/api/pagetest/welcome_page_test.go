package pagetest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// welcomeCast is GET /welcome/personas as the API answers it
// (internal/api/welcome.go welcomePersonas).
var welcomeCast = []map[string]any{
	{"id": "mirrin", "name": "Mirrin", "tagline": "The gentleman who's two steps ahead.", "greeting": "Good to see you, sir. What can I take off your plate?", "address": "sir", "wake_phrase": "Hey Mirrin", "instant_wake": true, "character": "maverick"},
	{"id": "nyra", "name": "Nyra", "tagline": "Composed and warm, with an eye for what others miss.", "greeting": "Hello. Nyra here.", "address": "", "wake_phrase": "Hey Nyra", "instant_wake": false, "character": "nyra"},
	{"id": "pickoo", "name": "Pickoo", "tagline": "A cheerful little penguin who gets things done.", "greeting": "Hiya! Pickoo here.", "address": "", "wake_phrase": "Hey Pickoo", "instant_wake": false, "character": "penguin"},
}

// welcomeBodies records what the page posted, by path.
type welcomeBodies struct {
	mu   sync.Mutex
	seen map[string][]map[string]any
}

func (b *welcomeBodies) record(reply any) http.HandlerFunc {
	out, _ := json.Marshal(reply)
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		b.mu.Lock()
		b.seen[r.URL.Path] = append(b.seen[r.URL.Path], v)
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}
}

func (b *welcomeBodies) last(path string) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.seen[path]; len(s) > 0 {
		return s[len(s)-1]
	}
	return nil
}

// welcomeStep2 opens the welcome page with a model found on this computer,
// uses it, and waits for step 2's cards.
func welcomeStep2(t *testing.T, voiceReady bool, setup ...chromedp.Action) (context.Context, *daemon, *welcomeBodies) {
	t.Helper()
	d := newDaemon(t)
	html := []byte(readPage(t, "../welcome.html"))
	d.handle("GET /welcome", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html)
	})
	bodies := &welcomeBodies{seen: map[string][]map[string]any{}}
	d.handle("GET /welcome/brains", jsonH([]map[string]any{{"Provider": "ollama", "Label": "Ollama", "Model": "llama3.1", "Ready": true}}))
	d.handle("GET /welcome/restore/report", jsonH(map[string]string{"detail": ""}))
	d.handle("GET /welcome/personas", jsonH(welcomeCast))
	d.handle("GET /voice/setup/status", jsonH(map[string]bool{"ready": voiceReady}))
	d.handle("POST /welcome/brain", bodies.record(map[string]bool{"ok": true}))
	d.handle("POST /welcome/preview", bodies.record(map[string]bool{"spoken": true}))
	d.handle("POST /welcome/names", bodies.record(map[string]bool{"ok": true}))
	ctx := tab(t)
	run(t, ctx, append(setup, desktop(), chromedp.Navigate(d.url("/welcome")))...)
	waitFor(t, ctx, `document.querySelectorAll('#cast input[name=persona]').length === 3 && !!document.querySelector('#detected button')`, "the cast and the model found")
	run(t, ctx, chromedp.Click(`#detected button`, chromedp.ByQuery))
	waitFor(t, ctx, visible("#cast"), "step 2")
	return ctx, d, bodies
}

// checkedPersona is the persona chosen in step 2.
const checkedPersona = `(document.querySelector('#cast input:checked') || {}).value`

// Step 2 shows Mirrin, Nyra and Pickoo as the screen draws them, from
// the shared drawings. Arrow keys move through them; the twin's name and how
// it addresses you follow the one chosen until you change the name yourself.
func TestWelcomeMeetsTheCast(t *testing.T) {
	ctx, d, bodies := welcomeStep2(t, true)
	if d.count("GET /characters.js") == 0 || !eval[bool](t, ctx, `typeof Characters === 'object' && typeof Characters.draw === 'function'`) {
		t.Fatal("the page didn't load /characters.js")
	}
	uniqueIDs := `(() => { const ids = [...document.querySelectorAll('[id]')].map(e => e.id); return ids.length === new Set(ids).size; })()`
	for what, expr := range map[string]string{
		"three characters, in order": `[...document.querySelectorAll('#cast .who')].map(c => c.querySelector('b').textContent).join(',') === 'Mirrin,Nyra,Pickoo'`,
		"each drawn":                 `[...document.querySelectorAll('#cast .who')].every(c => c.querySelectorAll('.pic svg .eyes').length === 1)`,
		"as the screen draws them":   `!!document.querySelector('#cast .pic.maverick #c0Suit') && !!document.querySelector('#cast .pic.nyra #c1Shell') && !!document.querySelector('#cast .pic.penguin #c2Body') && !document.querySelector('#cast .pic.robot')`,
		"112px across":               `[...document.querySelectorAll('#cast .pic')].every(p => Math.round(p.getBoundingClientRect().width) === 112)`,
		"every id once":              uniqueIDs,
		"real radios with labels":    `[...document.querySelectorAll('#cast input')].every(i => i.type === 'radio' && i.name === 'persona' && i.closest('label'))`,
		"Mirrin first and chosen":    checkedPersona + ` === 'mirrin' && document.activeElement === document.querySelector('#cast input:checked')`,
		"his name":                   `document.querySelector('#twin').value === 'Mirrin'`,
		"sir for him":                `document.querySelector('#address').value === 'sir' && document.querySelector('#addressq').textContent === 'How should Mirrin address you?'`,
		"a Hear button each":         `[...document.querySelectorAll('#cast .hear')].map(b => b.textContent).join(',') === 'Hear Mirrin,Hear Nyra,Hear Pickoo'`,
		"the slower wake line":       `[...document.querySelectorAll('#cast .who')].map(c => c.querySelector('.slow') ? c.querySelector('.slow').textContent : '').join('|') === '|Answers to “Hey Nyra”, a beat slower than Mirrin for now.|Answers to “Hey Pickoo”, a beat slower than Mirrin for now.'`,
		"changeable later":           `document.querySelector('#names').textContent.includes('You can change any of this later from the menu bar.')`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("step 2: %s", what)
		}
	}

	// An arrow key moves to Nyra: her name, her way of addressing you,
	// and her hello, a tilt of the head.
	run(t, ctx, chromedp.KeyEvent(kb.ArrowRight))
	waitFor(t, ctx, checkedPersona+` === 'nyra'`, "Nyra chosen by an arrow key")
	waitFor(t, ctx, `document.querySelector('#cast .pic.nyra').classList.contains('hi') && document.querySelector('#cast .pic.nyra .head').getAnimations().some(a => a.animationName === 'tilt')`, "Nyra's hello")
	if !eval[bool](t, ctx, `document.querySelector('#twin').value === 'Nyra' && document.querySelector('#address').value === 'name' && document.querySelector('#addressq').textContent === 'How should Nyra address you?'`) {
		t.Fatalf("Nyra chosen, but the name reads %q and the question %q", eval[string](t, ctx, `document.querySelector('#twin').value`), textOf(t, ctx, "#addressq"))
	}
	run(t, ctx, chromedp.KeyEvent(kb.ArrowRight))
	waitFor(t, ctx, checkedPersona+` === 'pickoo' && document.querySelector('#twin').value === 'Pickoo' && document.querySelector('#address').value === 'name' && document.querySelector('#addressq').textContent === 'How should Pickoo address you?'`, "Pickoo's name and By my name")
	waitFor(t, ctx, `document.querySelector('#cast .pic.penguin .fl.r').getAnimations().some(a => a.animationName === 'hi')`, "Pickoo's wave")

	// Hear Nyra asks the twin to say her hello in her voice.
	run(t, ctx, chromedp.Click(`#cast .who:nth-child(2) .hear`, chromedp.ByQuery))
	waitFor(t, ctx, `!document.querySelector('#cast .who:nth-child(2) .hear').disabled`, "the preview done")
	if got := bodies.last("/welcome/preview"); got == nil || got["Persona"] != "nyra" {
		t.Fatalf("Hear Nyra posted %v", got)
	}

	// A name of your own stays, whoever you choose next.
	run(t, ctx, chromedp.Evaluate(`document.querySelector('#twin').select(); true`, nil), chromedp.SendKeys("#twin", "Ember", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#addressq').textContent === 'How should Ember address you?'`, "the question with the new name")
	run(t, ctx, chromedp.Focus(`#cast input:checked`, chromedp.ByQuery), chromedp.KeyEvent(kb.ArrowLeft))
	waitFor(t, ctx, checkedPersona+` === 'nyra'`, "Nyra chosen")
	if got := eval[string](t, ctx, `document.querySelector('#twin').value`); got != "Ember" {
		t.Fatalf("the name changed to %q after it was edited", got)
	}

	run(t, ctx, chromedp.SendKeys("#you", "Akshaya Kumar", chromedp.ByQuery), chromedp.Click(`#nameform > button`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#names').hidden`, "step 2 sent")
	got := bodies.last("/welcome/names")
	if got["You"] != "Akshaya Kumar" || got["Twin"] != "Ember" || got["Persona"] != "nyra" || got["Address"] != "name" {
		t.Fatalf("names posted %v", got)
	}
}

// Before voice is set up there is nothing to hear: each card quotes its
// greeting instead.
func TestWelcomeQuotesTheGreetingWithoutVoice(t *testing.T) {
	ctx, _, _ := welcomeStep2(t, false)
	if eval[int](t, ctx, `document.querySelectorAll('#cast .hear').length`) != 0 {
		t.Fatal("a Hear button with no voice set up")
	}
	if got := eval[string](t, ctx, `[...document.querySelectorAll('#cast .who q')].map(q => q.textContent).join('|')`); got != "Good to see you, sir. What can I take off your plate?|Hello. Nyra here.|Hiya! Pickoo here." {
		t.Fatalf("the greetings read %q", got)
	}
}

// With reduced motion, focus and the pointer still choose and follow, but
// nothing on the page moves.
func TestWelcomeCastIsStillWithReducedMotion(t *testing.T) {
	ctx, _, _ := welcomeStep2(t, true, reducedMotion())
	run(t, ctx, chromedp.KeyEvent(kb.ArrowRight))
	waitFor(t, ctx, checkedPersona+` === 'nyra' && document.querySelector('#twin').value === 'Nyra'`, "Nyra chosen")
	type pt struct{ X, Y float64 }
	p := eval[pt](t, ctx, `(() => { const r = document.querySelector('#cast .pic.penguin').getBoundingClientRect(); return {X: r.left + r.width / 2, Y: r.top + r.height / 2}; })()`)
	run(t, ctx, chromedp.MouseEvent(input.MouseMoved, p.X, p.Y))
	waitFor(t, ctx, `document.querySelector('#cast .pic.penguin').classList.contains('hi')`, "the pointer over Pickoo")
	if n := eval[int](t, ctx, `document.getAnimations().length`); n != 0 {
		t.Fatalf("%d animations under reduced motion: %s", n, strings.Join(eval[[]string](t, ctx, `document.getAnimations().map(a => a.animationName || 'transition')`), ", "))
	}
}
