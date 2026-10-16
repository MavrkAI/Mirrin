package pagetest

import (
	"net/http"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// welcomeWardrobe is GET /welcome/wardrobe as the API answers it
// (internal/api/welcome_wardrobe.go).
var welcomeWardrobe = map[string]any{
	"outfits": map[string][]map[string]string{
		"maverick": {{"id": "dinner", "label": "Dinner jacket"}, {"id": "waistcoat", "label": "Waistcoat"}, {"id": "jumper", "label": "Knit jumper"}, {"id": "linen", "label": "Linen shirt"}},
		"nyra":     {{"id": "blazer", "label": "Navy blazer"}, {"id": "cardigan", "label": "Cardigan"}, {"id": "turtleneck", "label": "Turtleneck"}, {"id": "denim", "label": "Denim jacket"}},
		"penguin":  {{"id": "bowtie", "label": "Bow tie"}, {"id": "scarf", "label": "Scarf"}, {"id": "beanie", "label": "Beanie"}, {"id": "sunhat", "label": "Sun hat"}},
	},
	"palette": []map[string]string{
		{"name": "coral", "label": "Coral", "hex": "#ff6f61"}, {"name": "rose", "label": "Rose", "hex": "#e8718d"}, {"name": "mustard", "label": "Mustard", "hex": "#e3b23c"}, {"name": "forest", "label": "Forest", "hex": "#2f9e63"},
		{"name": "teal", "label": "Teal", "hex": "#2bb3a3"}, {"name": "sky", "label": "Sky", "hex": "#5bb8f5"}, {"name": "plum", "label": "Plum", "hex": "#9b59b6"}, {"name": "charcoal", "label": "Charcoal", "hex": "#4a4f5c"},
	},
}

const (
	pressedOutfit = `(document.querySelector('#outfits [aria-pressed=true]') || {}).textContent`
	pressedColour = `(document.querySelector('#colours [aria-pressed=true]') || {}).dataset.colour`
	outfitLabels  = `[...document.querySelectorAll('#outfits button')].map(b => b.textContent).join(',')`
)

// Step 2 dresses the twin: under the cast, the chosen character drawn
// large, its outfits to tap (each a picture of it in that outfit) and the
// palette as dots. A tap redraws the big picture and the card at once, as
// the screen will draw it; each character keeps its own choice while you
// look at the others; a saved look is where its character starts; and
// "Meet my twin" sends the look with the names.
func TestWelcomeDressesTheTwin(t *testing.T) {
	ctx, _, bodies := welcomeStep2(t, false)
	waitFor(t, ctx, visible("#dress")+` && document.querySelectorAll('#outfits button').length === 4`, "the dress-up under the cast")
	for what, expr := range map[string]string{
		"Mirrin drawn large":         `!!document.querySelector('#dresspic.maverick svg .eyes') && document.querySelector('#dresspic').getBoundingClientRect().width >= 160`,
		"as drawn, to start":         `!document.querySelector('#dresspic svg .outfit')`,
		"his wardrobe, in order":     outfitLabels + ` === 'Dinner jacket,Waistcoat,Knit jumper,Linen shirt'`,
		"the drawn outfit pressed":   pressedOutfit + ` === 'Dinner jacket'`,
		"each outfit a picture":      `[...document.querySelectorAll('#outfits button')].every(b => b.querySelector('svg .eyes')) && !!document.querySelector('#outfits button:nth-child(2) svg .outfit.waistcoat')`,
		"as drawn and eight colours": `document.querySelectorAll('#colours button').length === 9 && document.querySelector('#colours button').textContent === 'As drawn' && ` + pressedColour + ` === ''`,
		"each colour named":          `[...document.querySelectorAll('#colours button')].slice(1).every(b => b.getAttribute('aria-label') && b.style.getPropertyValue('--dot'))`,
		"every id once":              `(() => { const ids = [...document.querySelectorAll('[id]')].map(e => e.id); return ids.length === new Set(ids).size; })()`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("the dress-up: %s", what)
		}
	}

	// A waistcoat and coral: on the big picture and on his card at once.
	run(t, ctx, chromedp.Click(`#outfits button:nth-child(2)`, chromedp.ByQuery))
	waitFor(t, ctx, `!!document.querySelector('#dresspic svg .outfit.waistcoat') && !!document.querySelector('#cast .pic.maverick svg .outfit.waistcoat') && `+pressedOutfit+` === 'Waistcoat'`, "Mirrin in a waistcoat")
	run(t, ctx, chromedp.Click(`#colours button[data-colour=coral]`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#dresspic').style.getPropertyValue('--o1') === '#ff6f61' && document.querySelector('#cast .pic.maverick').style.getPropertyValue('--o1') === '#ff6f61' && `+pressedColour+` === 'coral'`, "in coral")

	// Pickoo: his own wardrobe, starting from the look the twin saved.
	run(t, ctx, chromedp.Focus(`#cast input:checked`, chromedp.ByQuery), chromedp.KeyEvent(kb.ArrowRight), chromedp.KeyEvent(kb.ArrowRight))
	waitFor(t, ctx, checkedPersona+` === 'pickoo' && `+outfitLabels+` === 'Bow tie,Scarf,Beanie,Sun hat'`, "Pickoo's wardrobe")
	if !eval[bool](t, ctx, `!!document.querySelector('#dresspic.penguin svg .outfit.scarf') && !document.querySelector('#dresspic.maverick') && `+pressedOutfit+` === 'Scarf' && `+pressedColour+` === 'teal' && document.querySelector('#dresspic').style.getPropertyValue('--o1') === '#2bb3a3'`) {
		t.Fatalf("Pickoo starts in %q and %q, not his saved scarf and teal", eval[string](t, ctx, pressedOutfit), eval[string](t, ctx, pressedColour))
	}
	run(t, ctx, chromedp.Click(`#outfits button:nth-child(3)`, chromedp.ByQuery))
	waitFor(t, ctx, `!!document.querySelector('#dresspic svg .outfit.beanie') && !!document.querySelector('#cast .pic.penguin svg .outfit.beanie')`, "Pickoo in a beanie")

	// Back to Mirrin: still in his waistcoat and coral. Then Pickoo again.
	run(t, ctx, chromedp.Focus(`#cast input:checked`, chromedp.ByQuery), chromedp.KeyEvent(kb.ArrowLeft), chromedp.KeyEvent(kb.ArrowLeft))
	waitFor(t, ctx, checkedPersona+` === 'mirrin' && `+pressedOutfit+` === 'Waistcoat' && `+pressedColour+` === 'coral' && !!document.querySelector('#dresspic.maverick svg .outfit.waistcoat')`, "Mirrin as dressed")
	run(t, ctx, chromedp.KeyEvent(kb.ArrowRight), chromedp.KeyEvent(kb.ArrowRight))
	waitFor(t, ctx, checkedPersona+` === 'pickoo' && `+pressedOutfit+` === 'Beanie'`, "Pickoo as dressed")

	// Meet my twin sends the look of the one chosen, the colour by its
	// palette name.
	run(t, ctx, chromedp.SendKeys("#you", "Akshay", chromedp.ByQuery), chromedp.Click(`#nameform > button`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#names').hidden`, "step 2 sent")
	got := bodies.last("/welcome/names")
	if got["Persona"] != "pickoo" || got["Outfit"] != "beanie" || got["Colour"] != "teal" {
		t.Fatalf("names posted %v", got)
	}
}

// The cast draws each character in the look the twin saved, and As drawn
// takes a colour away again.
func TestWelcomeCastWearsTheSavedLookAndAsDrawnUndoesIt(t *testing.T) {
	ctx, _, _ := welcomeStep2(t, false)
	waitFor(t, ctx, visible("#dress"), "the dress-up")
	if !eval[bool](t, ctx, `!!document.querySelector('#cast .pic.penguin svg .outfit.scarf') && document.querySelector('#cast .pic.penguin').style.getPropertyValue('--o1') === '#2bb3a3' && !document.querySelector('#cast .pic.maverick svg .outfit') && !document.querySelector('#cast .pic.maverick').style.getPropertyValue('--o1')`) {
		t.Fatal("the cast isn't drawn in the saved looks")
	}
	run(t, ctx, chromedp.Focus(`#cast input:checked`, chromedp.ByQuery), chromedp.KeyEvent(kb.ArrowRight), chromedp.KeyEvent(kb.ArrowRight))
	waitFor(t, ctx, checkedPersona+` === 'pickoo' && `+pressedColour+` === 'teal'`, "Pickoo in teal")
	run(t, ctx, chromedp.Click(`#colours button:first-child`, chromedp.ByQuery))
	waitFor(t, ctx, pressedColour+` === '' && !document.querySelector('#dresspic').style.getPropertyValue('--o1') && !document.querySelector('#cast .pic.penguin').style.getPropertyValue('--o1')`, "Pickoo's own colours back")
	run(t, ctx, chromedp.Click(`#outfits button:first-child`, chromedp.ByQuery))
	waitFor(t, ctx, `!document.querySelector('#dresspic svg .outfit') && !document.querySelector('#cast .pic.penguin svg .outfit')`, "Pickoo as drawn")
}

// On a phone the outfits still sit in their row, four across, with
// nothing cut off, and the page doesn't scroll sideways.
func TestWelcomeDressUpFitsAPhone(t *testing.T) {
	ctx, _, _ := welcomeStep2(t, false) // which lays the page out for a desktop
	waitFor(t, ctx, visible("#dress")+` && document.querySelectorAll('#outfits button').length === 4`, "the dress-up")
	run(t, ctx, phone())
	waitFor(t, ctx, `window.innerWidth === 390`, "a phone")
	fits := `(() => { const r = document.querySelector('#outfits'); const d = document.querySelector('#dress'); return r.scrollWidth <= r.clientWidth && d.scrollWidth <= d.clientWidth && [...r.children].every(b => b.getBoundingClientRect().right <= d.getBoundingClientRect().right) && document.documentElement.scrollWidth <= window.innerWidth; })()`
	if !eval[bool](t, ctx, fits) {
		t.Fatalf("the outfits overflow on a phone: rack %v wide in a panel %v wide", eval[float64](t, ctx, `document.querySelector('#outfits').scrollWidth`), eval[float64](t, ctx, `document.querySelector('#dress').clientWidth`))
	}
}

// A choice made with the keyboard keeps the keyboard where it was: the
// outfit or colour just pressed stays focused and reads as pressed, so
// the next Tab goes on from there.
func TestWelcomeDressUpKeepsKeyboardFocus(t *testing.T) {
	ctx, _, _ := welcomeStep2(t, false)
	waitFor(t, ctx, visible("#dress")+` && document.querySelectorAll('#outfits button').length === 4`, "the dress-up")
	run(t, ctx, chromedp.Focus(`#outfits button:nth-child(3)`, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	waitFor(t, ctx, `!!document.querySelector('#dresspic svg .outfit.jumper')`, "Mirrin in a jumper")
	if !eval[bool](t, ctx, `document.activeElement === document.querySelector('#outfits button:nth-child(3)') && document.activeElement.getAttribute('aria-pressed') === 'true'`) {
		t.Fatalf("after Enter on Knit jumper the focus is on %q", eval[string](t, ctx, `document.activeElement.tagName + '#' + document.activeElement.id + ' ' + document.activeElement.textContent`))
	}
	run(t, ctx, chromedp.Focus(`#colours button[data-colour=plum]`, chromedp.ByQuery), chromedp.KeyEvent(" "))
	waitFor(t, ctx, `document.querySelector('#dresspic').style.getPropertyValue('--o1') === '#9b59b6'`, "in plum")
	if !eval[bool](t, ctx, `document.activeElement === document.querySelector('#colours button[data-colour=plum]') && document.activeElement.getAttribute('aria-pressed') === 'true'`) {
		t.Fatalf("after Space on Plum the focus is on %q", eval[string](t, ctx, `document.activeElement.tagName + ' ' + (document.activeElement.dataset || {}).colour`))
	}
}

// A twin without the wardrobe route (an older or trimmed backend) gets
// step 2 as it was: the cast, no dress-up, and a names request without a
// look, so what it wears stays as it was.
func TestWelcomeWithoutAWardrobeStillNames(t *testing.T) {
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
	d.handle("GET /welcome/wardrobe", textH(404, "404 page not found"))
	d.handle("GET /voice/setup/status", jsonH(map[string]bool{"ready": false}))
	d.handle("POST /welcome/brain", bodies.record(map[string]bool{"ok": true}))
	d.handle("POST /welcome/names", bodies.record(map[string]bool{"ok": true}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/welcome")))
	waitFor(t, ctx, `document.querySelectorAll('#cast input[name=persona]').length === 3 && !!document.querySelector('#detected button')`, "the cast and the model found")
	run(t, ctx, chromedp.Click(`#detected button`, chromedp.ByQuery))
	waitFor(t, ctx, visible("#cast"), "step 2")
	if !eval[bool](t, ctx, `document.querySelector('#dress').hidden && !!document.querySelector('#cast .pic.penguin svg .outfit.scarf')`) {
		t.Fatal("without a wardrobe, the dress-up shows, or the cast lost its saved look")
	}
	run(t, ctx, chromedp.SendKeys("#you", "Akshay", chromedp.ByQuery), chromedp.Click(`#nameform > button`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#names').hidden`, "step 2 sent")
	got := bodies.last("/welcome/names")
	if _, has := got["Outfit"]; has || got["Persona"] != "mirrin" {
		t.Fatalf("names posted %v", got)
	}
	if _, has := got["Colour"]; has {
		t.Fatalf("names posted %v", got)
	}
}
